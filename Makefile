SHORT_NAME = kong-ingress

include versioning.mk

REPO_PATH := kolihub.io/${SHORT_NAME}
DEV_ENV_IMAGE := quay.io/koli/go-dev:0.2.0
DEV_ENV_WORK_DIR := /go/src/${REPO_PATH}
DEV_ENV_PREFIX := docker run --rm -v ${CURDIR}:${DEV_ENV_WORK_DIR} -w ${DEV_ENV_WORK_DIR}
DEV_ENV_CMD := ${DEV_ENV_PREFIX} ${DEV_ENV_IMAGE}

BINARY_DEST_DIR := rootfs/usr/bin
GOOS ?= linux
GOARCH ?= amd64

SHELL ?= /bin/bash

# Common flags passed into Go's linker.
GOTEST := go test --race -v
LDFLAGS := "-s -w \
-X kolihub.io/kong-ingress/pkg/version.version=${VERSION} \
-X kolihub.io/kong-ingress/pkg/version.gitCommit=${GITCOMMIT} \
-X kolihub.io/kong-ingress/pkg/version.buildDate=${DATE}"

build:
	rm -rf ${BINARY_DEST_DIR}
	mkdir -p ${BINARY_DEST_DIR}
	env GOOS=${GOOS} GOARCH=${GOARCH} go build -ldflags ${LDFLAGS} -o ${BINARY_DEST_DIR}/kong-ingress cmd/main.go

docker-build:
	docker build --rm -t ${IMAGE} rootfs
	docker tag ${IMAGE} ${MUTABLE_IMAGE}

docker-run:
	docker run --rm -it --name kong-ingress -v ~/.minikube:/certs ${MUTABLE_IMAGE} --apiserver=https://192.168.99.101:8443 --cert-file=/certs/client.crt --key-file=/certs/client.key --ca-file=/certs/ca.crt --auto-claim --wipe-on-delete --kong-server=http://192.168.99.101:31016 --v=4 --logtostderr

test: test-unit

test-unit:
	${GOTEST} ./pkg/...

.PHONY: build test docker-build
