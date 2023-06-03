
# Image URL to use all building/pushing image targets
IMG ?= quay.io/opendatahub/odh-notebook-controller
TAG ?= $(shell git describe --tags --always)

KF_IMG ?= quay.io/opendatahub/kubeflow-notebook-controller
KF_TAG ?= 1.6-9ae9413

CONTAINER_ENGINE ?= podman

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.23

# Kubernetes configuration
K8S_NAMESPACE = odh-notebook-controller-system

# Webhook configuration
WEBHOOK_PORT = 8443
WEBHOOK_CERT_DIR = /tmp/k8s-webhook-server/serving-certs

# E2E tests additional flags
E2E_TEST_FLAGS = "--skip-deletion=false" # See README.md

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set).
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
all: build

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

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

external-manifests: controller-gen ## Generate CustomResourceDefinition objects for 3rd party APIs.
    # Openshift CRDs
	go get github.com/openshift/api
	$(CONTROLLER_GEN) crd \
		paths=${GOPATH}/pkg/mod/github.com/openshift/api@v3.9.0+incompatible/route/v1 \
		output:crd:artifacts:config=config/crd/external
    # Kubeflow CRDS
	$(KUSTOMIZE) build ../notebook-controller/config/crd > config/crd/external/kubeflow.org_notebooks.yaml

.PHONY: license
license: ## Generate third-party license
	go list -m all | cut -d ' ' -f 1 > ./third_party/dep.txt
	cd ./third_party && python3 ./concatenate_license.py --output=./license.txt

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	ACK_GINKGO_DEPRECATIONS=1.16.5 \
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
		go test ./controllers/... -ginkgo.v -ginkgo.progress -test.v -coverprofile cover.out

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate fmt vet certificates ktunnel ## Run a controller from your host.
	$(KTUNNEL) inject deployment odh-notebook-controller-ktunnel \
		$(WEBHOOK_PORT) --eject=false --namespace $(K8S_NAMESPACE) &
	go run ./main.go

.PHONY: docker-build
docker-build: test ## Build docker image with the manager.
	cd ../ && ${CONTAINER_ENGINE} build . -t ${IMG}:${TAG} -f odh-notebook-controller/Dockerfile

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	${CONTAINER_ENGINE} push ${IMG}:${TAG}

.PHONY: image
image: docker-build docker-push ## Build and push docker image with the manager.

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: setup-kf
setup-kf: kustomize ## Replace Kustomize manifests with your environment configuration.
	sed -i'' -e 's,namespace: .*,namespace: '"${K8S_NAMESPACE}"',' \
		../notebook-controller/config/overlays/openshift/kustomization.yaml
	sed -i'' -e 's,newName: .*,newName: '"${KF_IMG}"',' \
		../notebook-controller/config/overlays/openshift/kustomization.yaml
	sed -i'' -e 's,newTag: .*,newTag: '"${KF_TAG}"',' \
 	../notebook-controller/config/overlays/openshift/kustomization.yaml

.PHONY: setup
setup: manifests kustomize ## Replace Kustomize manifests with your environment configuration.
	sed -i'' -e 's,namespace: .*,namespace: '"${K8S_NAMESPACE}"',' ./config/default/kustomization.yaml
	sed -i'' -e 's,newName: .*,newName: '"${IMG}"',' ./config/base/kustomization.yaml
	sed -i'' -e 's,newTag: .*,newTag: '"${TAG}"',' ./config/base/kustomization.yaml

.PHONY: setup-service-mesh
setup-service-mesh: kustomize ## Replace Kustomize manifests with your environment configuration.
	sed -i'' -e 's,namespace: .*,namespace: '"${K8S_NAMESPACE}"',' \
			../notebook-controller/config/overlays/service-mesh/smm.yaml
	sed -i'' -e 's,namespace: .*,namespace: '"${K8S_NAMESPACE}"',' \
		../notebook-controller/config/overlays/service-mesh/kustomization.yaml
	sed -i'' -e 's,newName: .*,newName: '"${KF_IMG}"',' \
		../notebook-controller/config/overlays/service-mesh/kustomization.yaml
	sed -i'' -e 's,newTag: .*,newTag: '"${KF_TAG}"',' \
	../notebook-controller/config/overlays/service-mesh/kustomization.yaml
	sed -i'' -e 's,host: .*,host: opendatahub.'"$(shell kubectl get ingress.config.openshift.io cluster -o 'jsonpath={.spec.domain}')"',' \
    	../notebook-controller/config/overlays/service-mesh/gateway-route.yaml

.PHONY: deploy-service-mesh
deploy-service-mesh: ## Deploy Service Mesh to the Openshift cluster.
	./e2e/scripts/install-ossm-release.sh install-operators
	./e2e/scripts/install-ossm-release.sh install-smcp

.PHONY: undeploy-service-mesh
undeploy-service-mesh: ## Undeploy Service Mesh and related operators
	./e2e/scripts/install-ossm-release.sh delete-smcp
	./e2e/scripts/install-ossm-release.sh delete-operators

.PHONY: deploy-kf
deploy-kf: setup-kf ## Deploy kubeflow controller to the Openshift cluster.
	$(KUSTOMIZE) build ../notebook-controller/config/overlays/openshift | oc apply -f -

.PHONY: deploy
deploy: setup deploy-kf ## Deploy controller to the Openshift cluster.
	$(KUSTOMIZE) build config/base | oc apply -f -

.PHONY: deploy-with-mesh
deploy-with-mesh: setup-service-mesh deploy
	$(KUSTOMIZE) build ../notebook-controller/config/overlays/service-mesh | oc apply -f -

.PHONY: undeploy-with-mesh
undeploy-with-mesh: setup-service-mesh undeploy
	$(KUSTOMIZE) build ../notebook-controller/config/overlays/service-mesh | oc delete --ignore-not-found=true -f -

.PHONY: deploy-dev
deploy-dev: setup deploy-kf ## Deploy controller to the Openshift cluster.
	$(KUSTOMIZE) build config/development | oc apply -f -

.PHONY: certificates
certificates: ## Extract webhook certificates generated by the Openshift CA.
	mkdir -p $(WEBHOOK_CERT_DIR)
	oc get secret odh-notebook-controller-webhook-cert \
		-o go-template='{{index .data "tls.crt" | base64decode }}' > $(WEBHOOK_CERT_DIR)/tls.crt
	oc get secret odh-notebook-controller-webhook-cert \
		-o go-template='{{index .data "tls.key" | base64decode }}' > $(WEBHOOK_CERT_DIR)/tls.key

.PHONY: undeploy-kf
undeploy-kf: setup-kf ## Undeploy kubeflow controller from the Openshift cluster.
	$(KUSTOMIZE) build ../notebook-controller/config/overlays/openshift | oc delete --ignore-not-found=true -f -

.PHONY: undeploy
undeploy: setup undeploy-kf ## Undeploy controller from the Openshift cluster.
	$(KUSTOMIZE) build config/base | oc delete --ignore-not-found=true -f -

.PHONY: undeploy-dev
undeploy-dev: setup undeploy-kf ## Undeploy controller from the Openshift cluster.
	$(KUSTOMIZE) build config/development | oc delete --ignore-not-found=true -f -

e2e-test-%:
	$(eval deploymentMode:=$(subst e2e-test-,,$@))
	go test ./e2e/ -run ^TestE2ENotebookController -v \
		--nb-namespace=${K8S_NAMESPACE} \
		--deploymentMode=$(deploymentMode) \
		${E2E_TEST_FLAGS}

.PHONY: e2e-test
e2e-test: e2e-test-oauth ## Run e2e tests for the controller with oauth proxy

.PHONY: run-ci-e2e-tests
run-ci-e2e-tests:
	bash ./run-e2e-test.sh

.PHONY: run-ci-e2e-tests-service-mesh
run-ci-e2e-service-mesh-tests:
	bash ./run-e2e-test-service-mesh.sh

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Ensure that the directory exists
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN = $(LOCALBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if necessary.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.8.0

KUSTOMIZE = $(LOCALBIN)/kustomize
.PHONY: kustomize
kustomize: ## Download kustomize locally if necessary.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/v3/cmd/kustomize@v3.2.0

ENVTEST = $(LOCALBIN)/setup-envtest
.PHONY: envtest
envtest: ## Download setup-envtest locally if necessary.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

KTUNNEL = $(LOCALBIN)/ktunnel
.PHONY: ktunnel
ktunnel: ## Download ktunnel locally if necessary.
	GOBIN=$(LOCALBIN) go install github.com/omrikiei/ktunnel@v1.4.8
