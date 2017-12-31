package controller

import "k8s.io/client-go/rest"

// Config defines configuration parameters for the Operator.
type Config struct {
	KongAdminHost  string
	Host           string
	TLSConfig      rest.TLSClientConfig
	ClusterDNS     string
	PodNamespace   string
	AutoClaim      bool
	WipeOnDelete   bool
	ResyncOnFailed int64
}
