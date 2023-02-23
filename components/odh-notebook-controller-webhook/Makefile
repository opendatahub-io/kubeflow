CONTAINER_ENGINE = podman
IMG ?= quay.io/opendatahub/odh-notebook-controller-webhook
TAG ?= $(shell git describe --tags --always)
WEBHOOK_CERT_DIR = /tmp/k8s-webhook-server/serving-certs
K8S_NAMESPACE = odh-notebook-controller-system
GO_VERSION = go
KUSTOMIZE = kustomize

.PHONY: fmt
fmt: ## Run go fmt against code.
	${GO_VERSION} fmt

.PHONY: vet
vet: ## Run go vet against code.
	${GO_VERSION} vet 

##@ Build

.PHONY: go-build
go-build: fmt vet ## Build webhook binary.
	${GO_VERSION} build -o bin/webhook main.go

.PHONY: build
build: ## Build docker image with the webhook.
	cd ../ && ${CONTAINER_ENGINE} build . -t ${IMG}:${TAG} -f odh-notebook-controller-webhook/Dockerfile

.PHONY: push
push: ## Push docker image with the webhook.
	${CONTAINER_ENGINE} push ${IMG}:${TAG}

.PHONY: image
image: build push ## Build and push docker image with the webhook.

.PHONY: deploy
deploy: 
	$(KUSTOMIZE) build ./config/webhook | kubectl apply -f -

.PHONY: undeploy
undeploy: 
	$(KUSTOMIZE) build ./config/webhook | kubectl delete --ignore-not-found=true -f -

.PHONY: e2e-test
e2e-test: ## Run e2e tests for the controller
	go test ./e2e/ -run ^TestE2EWebhook -v --namespace=${K8S_NAMESPACE} ${E2E_TEST_FLAGS}

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Ensure that the directory exists
	mkdir -p $(LOCALBIN)

KUSTOMIZE = $(LOCALBIN)/kustomize
.PHONY: kustomize
kustomize: ## Download kustomize locally if necessary.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/v3/cmd/kustomize@v3.2.0
