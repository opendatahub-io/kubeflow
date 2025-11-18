# Image URL to use all building/pushing image targets
IMG ?= ghcr.io/kubeflow/kubeflow/notebook-controller
TAG ?= $(shell git describe --tags --always --dirty)
ARCH ?= linux/amd64

CONTAINER_ENGINE ?= podman

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd"

# Based on recommendation https://sdk.operatorframework.io/docs/building-operators/golang/references/envtest-setup/
ENVTEST_ASSETS_DIR=$(shell pwd)/testbin

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: manager

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: check-license
check-license: ## Check third-party license
	./third_party/check-license.sh

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=role crd:maxDescLen=0 webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# Extract go version from go.mod (e.g., "1.23.6" from "go 1.23.6")
GO_VERSION = $(shell grep '^go ' go.mod | awk '{print $$2}')
.PHONY: govulncheck
govulncheck: ## Run govulncheck against code. Uses the Go version specified in go.mod.
	@if [ -z "$(GO_VERSION)" ]; then \
		echo "Warning: No go version directive found in go.mod, using default Go version"; \
		go run golang.org/x/vuln/cmd/govulncheck@latest -show=verbose ./...; \
	else \
		echo "Using Go version: $(GO_VERSION)"; \
		GOVERSION=go$(GO_VERSION) go run golang.org/x/vuln/cmd/govulncheck@latest -show=verbose ./...; \
	fi

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test -v ./... -coverprofile cover.out
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test -v ./controllers/... -coverprofile cover.out

.PHONY: manager
manager: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go

# Run the controller locally with culling enabled
.PHONY: run-culling
run-culling: generate fmt vet manifests
	ENABLE_CULLING=true \
	CLUSTER_DOMAIN=cluster.local \
	CULL_IDLE_TIME=10 \
	IDLENESS_CHECK_PERIOD=1 \
	DEV=true \
	go run ./main.go

##@ Build
.PHONY: docker-build
docker-build: test ## Build docker image with the manager.
	cd .. && ${CONTAINER_ENGINE} build . --platform="${ARCH}" -t ${IMG}:${TAG} -f ./notebook-controller/Dockerfile

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	${CONTAINER_ENGINE} push ${IMG}:${TAG}

.PHONY: docker-build-multi-arch
docker-build-multi-arch: ##  Build multi-arch docker images with docker buildx
	cd .. && docker buildx build --load --platform ${ARCH} --tag ${IMG}:${TAG} -f ./notebook-controller/Dockerfile .


.PHONY: docker-build-push-multi-arch
docker-build-push-multi-arch: ## Build multi-arch docker images with docker buildx and push to docker registry
	cd .. && docker buildx build --platform ${ARCH} --tag ${IMG}:${TAG} --push -f ./notebook-controller/Dockerfile .


.PHONY: image
image: docker-build docker-push ## Build and push docker image with the manager.

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: docker-build docker-push manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	sed -i'' -e 's@newName: .*@newName: '"${IMG}"'@' ./config/base/kustomization.yaml
	sed -i'' -e 's@newTag: .*@newTag: '"${TAG}"'@' ./config/base/kustomization.yaml
	$(KUSTOMIZE) build config/base | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/base | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Ensure that the directory exists
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN = $(LOCALBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.18.0

KUSTOMIZE = $(shell pwd)/bin/kustomize
.PHONY: kustomize
kustomize: ## Download kustomize locally if necessary.
	$(call go-get-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5@v5.0.2)

ENVTEST = $(shell pwd)/bin/setup-envtest
ENVTEST_VERSION?=v0.0.0-20250723164028-948d554cd9c7

.PHONY: envtest
envtest: ## Download setup-envtest locally if necessary.
	$(call go-get-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION))

# go-get-tool will 'go get' any package $2 and install it to $1.
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))
define go-get-tool
@[ -f $(1) ] || { \
set -e ;\
echo "Downloading $(2)" ;\
GOBIN=$(PROJECT_DIR)/bin go install $(2) ;\
}
endef
