CONTAINER_ENGINE = podman
IMG ?= quay.io/opendatahub/odh-notebook-controller
TAG ?= $(shell git describe --tags --always)

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt

.PHONY: vet
vet: ## Run go vet against code.
	go vet 

##@ Build

.PHONY: go-build
go-build: fmt vet ## Build webhook binary.
	go build -o bin/webhook main.go

.PHONY: build
build: ## Build docker image with the webhook.
	cd ../ && ${CONTAINER_ENGINE} build . -t ${IMG}:${TAG} -f odh-notebook-controller-webhook/Dockerfile

.PHONY: push
push: ## Push docker image with the webhook.
	${CONTAINER_ENGINE} push ${IMG}:${TAG}

.PHONY: image
image: build push ## Build and push docker image with the webhook.

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
