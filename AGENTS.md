# AI Agent Instructions — ODH Kubeflow

This repository is the OpenDataHub (ODH) fork of
[kubeflow/kubeflow](https://github.com/kubeflow/kubeflow), containing two
Go-based Kubernetes controllers for managing Jupyter Notebook workloads.

For full architecture details, component descriptions, request flow diagrams,
and CRD specifications, see [ARCHITECTURE.md](./ARCHITECTURE.md).
For developer workflow, prerequisites, and review process, see
[CONTRIBUTING.md](./CONTRIBUTING.md).

## Build

There is no root-level `go.mod`. Build each component independently:

```sh
# Upstream controller
cd components/notebook-controller && make manager

# ODH controller
cd components/odh-notebook-controller && make build
```

Docker/Podman images:
```sh
cd components/odh-notebook-controller
make docker-build IMG=<registry>/odh-notebook-controller TAG=<tag>
make docker-push  IMG=<registry>/odh-notebook-controller TAG=<tag>
```

## Test

### Unit tests (envtest-based, Ginkgo)
```sh
cd components/notebook-controller && make test
cd components/odh-notebook-controller && make test   # runs RBAC=false + RBAC=true
```

Coverage profiles: `cover.out` / `cover-rbac-{false,true}.out`.

### End-to-end tests
```sh
cd components/odh-notebook-controller
export KUBECONFIG=/path/to/kubeconfig
make e2e-test -e K8S_NAMESPACE=<namespace>
```

Pass `E2E_TEST_FLAGS="--skip-deletion=true"` to skip notebook deletion tests.

## Debug

### Run locally with webhook tunnel
```sh
cd components/odh-notebook-controller
make deploy-dev -e K8S_NAMESPACE=<ns>   # Deploys ktunnel for webhook redirect
make run -e K8S_NAMESPACE=<ns>          # Starts controller locally
```

### Envtest debug options
| Variable                | Effect                                             |
|-------------------------|----------------------------------------------------|
| `DEBUG_WRITE_KUBECONFIG`| Writes kubeconfig for inspecting the envtest cluster |
| `DEBUG_WRITE_AUDITLOG`  | Writes kube-apiserver audit logs to disk            |
| `DISABLE_WEBHOOK`       | Disables the admission webhook during local run     |

## Lint and Format

```sh
cd components/odh-notebook-controller   # same targets for notebook-controller

golangci-lint run --timeout=5m
go fmt ./...
go mod verify
go mod tidy -diff
```

## Deploy

```sh
cd components/odh-notebook-controller

make deploy -e K8S_NAMESPACE=<ns> -e IMG=<image>   # Deploy (includes upstream)
make deploy-dev -e K8S_NAMESPACE=<ns>              # Dev overlay (ktunnel)
make undeploy                                       # Undeploy
```

## Conventions

- Go version is kept in sync across all `go.mod` files, Dockerfiles, and
  downstream Konflux Dockerfiles.
- Generated code (`zz_generated.deepcopy.go`) is regenerated via
  `bash ci/generate_code.sh` — always commit the results.
- OWNERS file (Prow-style) controls review assignment; PRs require 2 reviews
  plus an `/approve` comment.
