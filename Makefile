COMPONENTS = components/notebook-controller components/odh-notebook-controller

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Build

.PHONY: build
build: ## Build all controller binaries.
	$(MAKE) -C components/notebook-controller manager
	$(MAKE) -C components/odh-notebook-controller build

##@ Test

.PHONY: test
test: ## Run unit tests for all components.
	$(MAKE) -C components/notebook-controller test
	$(MAKE) -C components/odh-notebook-controller test

.PHONY: e2e-test
run-ci-e2e-tests: ## Run e2e tests (requires KUBECONFIG).
	$(MAKE) -C components/odh-notebook-controller run-ci-e2e-tests

##@ Code Quality

.PHONY: lint
lint: ## Run golangci-lint for all components (via pre-commit).
	@fail=0; \
	pre-commit run golangci-lint-notebook-controller --all-files || fail=1; \
	pre-commit run golangci-lint-odh-notebook-controller --all-files || fail=1; \
	exit $$fail

.PHONY: fmt
fmt: ## Run go fmt for all components.
	@for comp in $(COMPONENTS); do \
		$(MAKE) -C $$comp fmt ; \
	done

.PHONY: vet
vet: ## Run go vet for all components.
	@for comp in $(COMPONENTS); do \
		$(MAKE) -C $$comp vet ; \
	done

.PHONY: verify-modules
verify-modules: ## Run go mod verify and go mod tidy -diff for all components.
	@fail=0; for comp in $(COMPONENTS); do \
		echo "=== Verifying $$comp ===" ; \
		(cd $$comp && go mod verify && go mod tidy -diff) || fail=1 ; \
	done; exit $$fail

.PHONY: govulncheck
govulncheck: ## Run govulncheck for all components.
	@for comp in $(COMPONENTS); do \
		$(MAKE) -C $$comp govulncheck ; \
	done

.PHONY: generate
generate: ## Regenerate all generated code.
	bash ci/generate_code.sh

##@ Deployment

.PHONY: deploy
deploy: ## Deploy both controllers (requires K8S_NAMESPACE).
	$(MAKE) -C components/odh-notebook-controller deploy

.PHONY: undeploy
undeploy: ## Undeploy both controllers.
	$(MAKE) -C components/odh-notebook-controller undeploy
