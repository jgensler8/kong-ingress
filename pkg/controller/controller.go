package controller

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	auth0 "github.com/jgensler8/go-auth0/generated/client"
	kongswagger "github.com/jgensler8/kong-swagger/generated"
	"github.com/koli/kong-ingress/pkg/kong"
	"gopkg.in/square/go-jose.v2/json"
	"k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"github.com/pkg/errors"
)

// TODO: an user is limited on how many paths and hosts he could create, this limitation is based on a hard quota from a Plan
// Wait for https://github.com/Mashape/kong/issues/383

var (
	keyFunc                  = cache.DeletionHandlingMetaNamespaceKeyFunc
	pluginPrefix             = "kolihub.io/plugin-"
	jwtAuth0DomainAnnotation = "kolihub.io/x-jwt-auth0-domain"
)

// KongController watches the kubernetes api server and adds/removes apis on Kong
type KongController struct {
	client     kubernetes.Interface
	extClient  restclient.Interface
	kongcli    *kong.CoreClient
	kongclient *kongswagger.APIClient

	infIng cache.SharedIndexInformer
	infSvc cache.SharedIndexInformer
	infDom cache.SharedIndexInformer

	cfg *Config

	ingQueue *TaskQueue
	domQueue *TaskQueue
	svcQueue *TaskQueue
	recorder record.EventRecorder
}

// NewKongController creates a new KongController
func NewKongController(
	client kubernetes.Interface,
	extClient restclient.Interface,
	kongcli *kong.CoreClient,
	kongclient *kongswagger.APIClient,
	cfg *Config,
	resyncPeriod time.Duration,
) *KongController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: v1core.New(client.Core().RESTClient()).Events(""),
	})
	kc := &KongController{
		client:     client,
		extClient:  extClient,
		kongcli:    kongcli,
		kongclient: kongclient,
		recorder:   eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "kong-controller"}),
		cfg:        cfg,
	}
	kc.ingQueue = NewTaskQueue(kc.syncIngress, "kong_ingress_queue")
	kc.domQueue = NewTaskQueue(kc.syncDomain, "kong_domain_queue")
	kc.svcQueue = NewTaskQueue(kc.syncServices, "kong_service_queue")

	kc.infIng = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(client.Extensions().RESTClient(), "ingresses", metav1.NamespaceAll, fields.Everything()),
		&v1beta1.Ingress{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	kc.infIng.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ing := obj.(*v1beta1.Ingress)
			if !isKongIngress(ing) {
				glog.Infof("ignoring add for ingress %v based on annotation %v", ing.Name, ingressClassKey)
				return
			}
			kc.ingQueue.Add(obj)
		},
		UpdateFunc: func(o, n interface{}) {
			old := o.(*v1beta1.Ingress)
			new := n.(*v1beta1.Ingress)
			if old.ResourceVersion != new.ResourceVersion && isKongIngress(new) {
				kc.ingQueue.Add(n)
				return
			}
		},
		DeleteFunc: func(obj interface{}) {
			ing := obj.(*v1beta1.Ingress)
			if !isKongIngress(ing) {
				glog.Infof("ignoring delete for ingress %v based on annotation %v", ing.Name, ingressClassKey)
				return
			}
			kc.ingQueue.Add(obj)
		},
	})

	kc.infSvc = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(client.Core().RESTClient(), "services", metav1.NamespaceAll, fields.Everything()),
		&v1.Service{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	kc.infSvc.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj interface{}) {
			kc.svcQueue.Add(obj)
		},
		UpdateFunc: func(o, n interface{}) {
			old := o.(*v1.Service)
			new := n.(*v1.Service)
			if old.ResourceVersion != new.ResourceVersion {
				kc.svcQueue.Add(n)
			}
		},
		AddFunc: func(obj interface{}) {
			kc.svcQueue.Add(obj)
		},
	})

	kc.infDom = cache.NewSharedIndexInformer(
		cache.NewListWatchFromClient(extClient, "domains", metav1.NamespaceAll, fields.Everything()),
		&kong.Domain{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	kc.infDom.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    kc.addDomain,
		UpdateFunc: kc.updateDomain,
		DeleteFunc: kc.deleteDomain,
	})
	return kc
}

// Run starts the kong controller.
func (k *KongController) Run(workers int, stopc <-chan struct{}) {
	glog.Infof("Starting Kong controller")
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	defer k.ingQueue.shutdown()
	defer k.domQueue.shutdown()
	defer k.svcQueue.shutdown()

	go k.infIng.Run(stopc)
	go k.infSvc.Run(stopc)
	go k.infDom.Run(stopc)

	if !cache.WaitForCacheSync(stopc, k.infIng.HasSynced, k.infSvc.HasSynced) {
		return
	}

	// start up your worker threads based on threadiness.
	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens.
		// The .Until will then rekick the worker after one second
		go k.ingQueue.run(time.Second, stopc)
		go k.domQueue.run(time.Second, stopc)
		go k.svcQueue.run(time.Second, stopc)
	}

	// run will loop until "something bad" happens.
	// It will rekick the worker after one second
	<-stopc
	glog.Infof("Shutting down Kong controller")
}

// garbage collect kong apis
func (k *KongController) syncServices(key string, numRequeues int) error {
	obj, exists, err := k.infSvc.GetStore().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		glog.V(4).Infof("%s - gc=true, service resource doesn't exists", key)
		return nil
	}
	svc := obj.(*v1.Service)
	if svc.DeletionTimestamp == nil {
		return nil
	}

	for _, port := range svc.Spec.Ports {
		proto := "http"
		if port.Port == 443 {
			proto = "https"
		}
		upstreamURL := k.getUpstream(proto, svc.Namespace, svc.Name, port.Port)
		glog.V(4).Infof("%s - gc=true, cleaning up kong apis from upstream %s", key, upstreamURL)
		params := url.Values{"upstream_url": []string{upstreamURL}}
		apiList, err := k.kongcli.API().List(params)
		if err != nil {
			return fmt.Errorf("gc=true, failed listing apis [%s]", err)
		}
		for _, api := range apiList.Items {
			glog.V(4).Infof("%s - gc=true, removing kong api %s[%s]", key, api.Name, api.UID)
			if err := k.kongcli.API().Delete(api.Name); err != nil {
				return fmt.Errorf("gc=true, failed removing kong api %s, [%s]", api.Name, err)
			}
		}
		// remove the finalizer
		if _, err := k.client.Core().Services(svc.Namespace).Patch(
			svc.Name,
			types.MergePatchType,
			[]byte(`{"metadata": {"finalizers": []}}`),
		); err != nil {
			return fmt.Errorf("gc=true, failed patch service [%s]", err)
		}
	}
	return nil
}

func (k *KongController) ConfigurePluginsForAPI(uuid string, ing *v1beta1.Ingress) error {
	var err error
	for a, annotationValue := range ing.Annotations {
		if strings.HasPrefix(a, pluginPrefix) {
			pluginname := strings.TrimPrefix(a, pluginPrefix)
			plugin := kongswagger.Plugin{
				Name: pluginname,
			}
			var iplugin interface{}
			if pluginname == "key-auth" {
				config := kongswagger.PluginConfigKeyAuth{}
				err = json.Unmarshal([]byte(annotationValue), &config)
				iplugin = config
			} else if pluginname == "cors" {
				config := kongswagger.PluginConfigCors{}
				err = json.Unmarshal([]byte(annotationValue), &config)
				iplugin = config
			} else if pluginname == "jwt" {
				config := kongswagger.PluginConfigJwt{}
				err = json.Unmarshal([]byte(annotationValue), &config)
				iplugin = config
			} else if pluginname == "rate-limiting" {
				config := kongswagger.PluginConfigRateLimiting{}
				err = json.Unmarshal([]byte(annotationValue), &config)
				iplugin = config
			} else {
				err := fmt.Errorf("Invlaid plugin '%s' specificied for ing/%s/%s with annotation %s", pluginname, ing.Namespace, ing.Name, a)
				glog.Error(err)
				return err
			}
			if err != nil {
				glog.Infof("Failed to unmarshal plugin config for ing/%s/%s with annotation %s", ing.Namespace, ing.Name, a)
				return err
			}
			plugin.Config = &iplugin

			params := map[string]interface{}{
				"plugin": plugin,
			}

			list, _, err := k.kongclient.DefaultApi.ListPlugins(uuid)
			if err != nil {
				glog.Infof("Failed to list plugins for API %s", uuid)
				return err
			}
			found := false
			for _, p := range list.Data {
				if p.Name == plugin.Name {
					glog.Infof("Plugin (%s) already configured for API (%s). Note that new configuration is NOT applied", plugin.Name, uuid)
					found = true
					break
				}
			}
			if !found {
				_, _, err := k.kongclient.DefaultApi.CreatePlugin(uuid, params)
				if err != nil {
					glog.Infof("Failed to create plugin for ing/%s/%s with annotation %s", ing.Namespace, ing.Name, a)
					return err
				}
			}
		}
	}
	return err
}

func (k *KongController) TryConfigureCertificates(ing *v1beta1.Ingress) error {
	for _, t := range ing.Spec.TLS {
		for _, h := range t.Hosts {
			secret, err := k.client.CoreV1().Secrets(ing.Namespace).Get(t.SecretName, metav1.GetOptions{})
			if err != nil {
				glog.Errorf("Failed to list secrets to match with Ingress TLS certificates")
				return err
			}
			if secret.Type != v1.SecretTypeTLS {
				errmessage := fmt.Sprintf("Secret Specified for Ingress is not a TLS Secret (found %s instead)", secret.Type)
				glog.Error(errmessage)
				return errors.New(errmessage)
			}

			cert := kongswagger.Certificate{
				Cert: string(secret.Data["tls.crt"]),
				Key: string(secret.Data["tls.key"]),
				Snis: []string{ h },
			}
			options := map[string]interface{} {
				"certificate": cert,
			}
			_, _, err = k.kongclient.DefaultApi.CreateCertificate(options)
			if err != nil {
				glog.Errorf("Failed to create kong tls certificate for host %s in ingress %s/%s", h, ing.Namespace, ing.Name)
				return err
			}
		}
	}

	return nil
}

func (k *KongController) TryAutoConfigureAuth0(ing *v1beta1.Ingress) error {
	host := ""
	for k, v := range ing.Annotations {
		if k == jwtAuth0DomainAnnotation {
			host = v
			break
		}
	}
	if host == "" {
		return nil
	}

	cfg := auth0.DefaultTransportConfig().WithHost(host)
	client := auth0.NewHTTPClientWithConfig(nil, cfg)

	certBuf := bytes.NewBufferString("")
	_, err := client.Operations.GetPEM(nil, certBuf)
	if err != nil {
		glog.Errorf("Failed to get x509 certificate from Auth0")
		return err
	}
	block, _ := pem.Decode(certBuf.Bytes())
	var cert *x509.Certificate
	cert, err = x509.ParseCertificate(block.Bytes)
	if err != nil {
		glog.Errorf("Failed to parse x509 certificate returned by Auth0")
		return err
	}
	asn1Bytes, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		glog.Errorf("Failed to marshal public key from Auth0's certificate")
		return err
	}
	var pemkey = &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: asn1Bytes,
	}
	buf := bytes.NewBufferString("")
	err = pem.Encode(buf, pemkey)
	if err != nil {
		glog.Errorf("Failed to encode public key from Auth0's certificate")
		return err
	}

	_, res, err := k.kongclient.DefaultApi.GetConsumer(host)
	if err != nil {
		if res.StatusCode == http.StatusNotFound {
			consumer := kongswagger.Consumer{
				Username: host,
			}
			_, err := k.kongclient.DefaultApi.CreateConsumer(consumer)
			if err != nil {
				glog.Errorf("Failed to create default JWT-associated Consumer for host (%s)", host)
				return err
			}
		} else {
			glog.Errorf("Failed to get consumer (%s) in Auth0 auto-configuration", host)
			return err
		}
	}

	list, _, err := k.kongclient.DefaultApi.ListJWTCredentials(host)
	if err != nil {
		glog.Errorf("Failed to list JWT credentials for default consumer (%s)", host)
		return err
	}
	if list.Total == 0 {
		jwtcred := kongswagger.JwtCredential{
			Algorithm:    "RS256",
			RsaPublicKey: buf.String(),
			// iss field ends with a '/'
			Key:          "https://" + host + "/",
		}
		_, _, err = k.kongclient.DefaultApi.CreateJWTCredential(host, jwtcred)
		if err != nil {
			glog.Errorf("Failed to create JWT credential for default consumer (%s)", host)
			return err
		}
	}

	return nil
}

func (k *KongController) syncIngress(key string, numRequeues int) error {
	obj, exists, err := k.infIng.GetStore().GetByKey(key)
	if err != nil {
		glog.V(4).Infof("%s - failed retrieving object from store: %s", key, err)
		return err
	}
	if !exists {
		glog.V(4).Infof("%s - ingress doesn't exists", key)
		return nil
	}

	ing := obj.(*v1beta1.Ingress)
	if numRequeues > autoClaimMaxRetries {
		// The dirty state is used only to indicate the object couldn't recover
		// from a bad state, useful to warn clients.
		if err := k.setDirty(ing, numRequeues); err != nil {
			glog.Warningf("%s - failed set resource as dirty: %s", err)
		}
	}

	//if k.cfg.AutoClaim {
	//	if err := k.claimDomains(ing); err != nil {
	//		return fmt.Errorf("autoclaim=on, failed claiming domains [%s]", err)
	//	}
	//	time.Sleep(500 * time.Millisecond) // give time to sync the domains
	//}
	//isAllowed, notFoundHost, err := k.isClaimed(ing)
	//if err != nil {
	//	return fmt.Errorf("failed retrieving domains from indexer [%s]", err)
	//}
	//if !isAllowed {
	//	if numRequeues > 2 {
	//		k.recorder.Eventf(ing, v1.EventTypeWarning, "DomainNotFound", "The domain '%s' was not claimed, check its state", notFoundHost)
	//	}
	//	return fmt.Errorf("failed claiming domain %s, check its state!", notFoundHost)
	//}
	glog.V(4).Infof("%s - Allowed to sync ingress routes, found all domains.", key)
	// TODO: add tls
	// Rules could have repeated domains, it will be redundant but it will work.
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			glog.V(4).Infof("%s - HTTP is nil, skipping ...")
			continue
		}
		// Iterate for each path and generate a new API registry on kong.
		// A domain will have multiple endpoints allowing path based routing.
		for _, p := range r.HTTP.Paths {
			// TODO: validate the service port!
			serviceExists := false
			err := cache.ListAll(k.infSvc.GetStore(), labels.Everything(), func(obj interface{}) {
				svc := obj.(*v1.Service)
				if svc.Name == p.Backend.ServiceName && svc.Namespace == ing.Namespace {
					serviceExists = true
				}
			})
			if err != nil {
				return fmt.Errorf("failed listing services from cache: %s", err)
			}
			if !serviceExists {
				k.recorder.Eventf(ing, v1.EventTypeWarning, "ServiceNotFound", "Service '%s' not found for ingress", p.Backend.ServiceName)
				return fmt.Errorf("Service %s not found", p.Backend.ServiceName)
			}
			// A finalizer is necessary to clean the APIs associated with kong.
			// A service is directly related with several Kong APIs by its upstream.
			fdata := fmt.Sprintf(`{"metadata": {"finalizers": ["%s"]}}`, kong.Finalizer)
			finalizerPatchData := bytes.NewBufferString(fdata).Bytes()
			if _, err := k.client.Core().Services(ing.Namespace).Patch(
				p.Backend.ServiceName,
				types.StrategicMergePatchType,
				finalizerPatchData,
			); err != nil {
				return fmt.Errorf("failed configuring service: %s", err)
			}

			proto := "http"
			if p.Backend.ServicePort.IntVal == 443 {
				proto = "https"
			}
			upstreamURL := k.getUpstream(
				proto,
				ing.Namespace,
				p.Backend.ServiceName,
				p.Backend.ServicePort.IntVal,
			)
			pathURI := p.Path
			// An empty path or root one (/) has no distinction in Kong.
			// Normalize the path otherwise it will generate a distinct adler hash
			if pathURI == "/" || pathURI == "" {
				pathURI = "/"
			}
			apiName := fmt.Sprintf("%s~%s~%s", r.Host, ing.Namespace, GenAdler32Hash(pathURI))
			api, resp := k.kongcli.API().Get(apiName)
			if resp.Error() != nil && !apierrors.IsNotFound(resp.Error()) {
				k.recorder.Eventf(ing, v1.EventTypeWarning, "FailedAddRoute", "%s", resp)
				return fmt.Errorf("failed listing api: %s", resp)
			}

			stripUri, err := strconv.ParseBool(ing.Annotations["ingress.kubernetes.io/strip-uri"])
			if err != nil {
				stripUri = true
				glog.Infof("Failed to parse strip-uri annotation, setting it to the default value true")
			}

			preserveHost, err := strconv.ParseBool(ing.Annotations["ingress.kubernetes.io/preserve-host"])
			if err != nil {
				preserveHost = false
				glog.Infof("Failed to parse preserve-host annotation, setting it to the default value false")
			}

			apiBody := &kong.API{
				Name:         apiName,
				UpstreamURL:  upstreamURL,
				StripUri:     stripUri,
				PreserveHost: preserveHost,
			}
			if r.Host != "" {
				apiBody.Hosts = []string{r.Host}
			}
			if p.Path != "" {
				apiBody.URIs = []string{pathURI}
			}
			// It will trigger an update when providing the uuid,
			// otherwise a new record will be created.
			if api != nil {
				apiBody.UID = api.UID
				apiBody.CreatedAt = api.CreatedAt
			}
			api, resp = k.kongcli.API().UpdateOrCreate(apiBody)
			if resp.Error() != nil && !apierrors.IsConflict(resp.Error()) {
				return fmt.Errorf("failed adding api: %s", resp)
			}
			glog.Infof("%s - added route for %s[%s]", key, r.Host, api.UID)

			// configure the API
			err = k.ConfigurePluginsForAPI(api.UID, ing)
			if err != nil {
				return err
			}
			glog.Infof("%s - finished creating plugins for %s[%s]", key, r.Host, api.UID)

			err = k.TryConfigureCertificates(ing)
			if err != nil {
				return err
			}
			glog.Infof("%s - finished creating certificates for %s[%s]", key, r.Host, api.UID)

			err = k.TryAutoConfigureAuth0(ing)
			if err != nil {
				glog.Errorf("Failed to configure Auth0 for %s[%s]", r.Host, api.UID)
				return err
			}
		}
	}
	return nil
}

func (k *KongController) getUpstream(proto, ns, svcName string, svcPort int32) string {
	return fmt.Sprintf("%s://%s.%s.%s:%d",
		proto,
		svcName,
		ns,
		k.cfg.ClusterDNS,
		svcPort)
}

func (k *KongController) claimDomains(ing *v1beta1.Ingress) error {
	for _, d := range getHostsFromIngress(ing) {
		domainType := d.GetDomainType()
		if !d.IsValidDomain() {
			return fmt.Errorf("it's not a valid domain %s", d.GetDomain())
		}
		obj, exists, _ := k.infDom.GetStore().Get(d)
		glog.V(4).Infof("%s/%s - Trying to claim %s domain %s ...", ing.Namespace, ing.Name, domainType, d.GetDomain())
		if exists {
			dom := obj.(*kong.Domain)
			if reflect.DeepEqual(dom.Spec, d.Spec) {
				glog.V(4).Infof("%s/%s - Skip update on %s host %s, any changes found ...", ing.Namespace, ing.Name, domainType, d.GetDomain())
				continue
			}
			glog.Infof("%s/%s - Updating %s domain %s ...", ing.Namespace, ing.Name, domainType, d.GetDomain())
			domCopy := dom.DeepCopy()
			if domCopy == nil {
				return fmt.Errorf("failed deep copying resource [%v]", dom)
			}
			domCopy.Spec = d.Spec
			// If the domain exists, try to recover its status requeuing as a new domain
			if domCopy.Status.Phase != kong.DomainStatusOK {
				domCopy.Status = kong.DomainStatus{Phase: kong.DomainStatusNew}
			}
			res, err := k.extClient.Put().
				Resource("domains").
				Name(domCopy.Name).
				Namespace(ing.Namespace).
				Body(domCopy).
				DoRaw()
			if err != nil {
				return fmt.Errorf("failed updating domain [%s, %s]", string(res), err)
			}

		} else {
			res, err := k.extClient.Post().
				Resource("domains").
				Namespace(ing.Namespace).
				Body(d).
				DoRaw()
			if err != nil {
				return fmt.Errorf("failed creating new domain [%s, %s]", string(res), err)
			}
		}
	}
	return nil
}

// isClaimed validates if a domain exists and is allowed to be claimed (DomainStatusOK)
// for each host on an ingress resource.
func (k *KongController) isClaimed(ing *v1beta1.Ingress) (bool, string, error) {
	for _, rule := range ing.Spec.Rules {
		var d *kong.Domain
		err := cache.ListAllByNamespace(k.infDom.GetIndexer(), ing.Namespace, labels.Everything(), func(obj interface{}) {
			if d != nil {
				return // the host is allowed, stop processing further resources
			}
			dom := obj.(*kong.Domain)
			if dom.Status.Phase == kong.DomainStatusOK && dom.GetDomain() == rule.Host {
				d = dom
			}
		})
		if err != nil || d == nil {
			return false, rule.Host, err
		}
		glog.V(4).Infof("%s/%s - Found %s domain %s!", ing.Namespace, ing.Name, d.GetDomainType(), d.GetDomain())
	}
	return true, "", nil
}

// setDirty sets an annotation indicating the object could not recover from itself
func (k *KongController) setDirty(ing *v1beta1.Ingress, retries int) error {
	payload := []byte(`{"metadata": {"annotations": {"kolihub.io/dirty": "true"}}}`)
	if ing.Annotations != nil && ing.Annotations["kolihub.io/dirty"] == "true" {
		return nil // it's already dirty
	}
	glog.Infof("%s/%s - retries[%d], the object could not recover from itself, setting as dirty.", ing.Namespace, ing.Name, retries)
	_, err := k.client.Extensions().Ingresses(ing.Namespace).
		Patch(ing.Name, types.StrategicMergePatchType, payload)
	return err
}
