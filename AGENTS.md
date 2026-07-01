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

## Chaos Validation (operator-chaos)

This repo integrates [operator-chaos](https://github.com/opendatahub-io/operator-chaos)
for shift-left upgrade validation (**Level 1-3**).

### Knowledge model and CI (L1 + L2)

A repo-local knowledge model at `chaos/knowledge/workbenches.yaml` describes
the operator topology (Deployments, ServiceAccounts, webhooks, steady-state
checks). Experiment definitions live in `chaos/experiments/` — these are
declarative YAML descriptions of chaos scenarios (pod-kill, network-partition,
etc.) that are **schema-validated in CI only**; they are not executed against a
cluster at this maturity level and are prepared for future L4 runtime execution
via `operator-chaos run`.

A GitHub Actions workflow (`.github/workflows/operator_chaos_validation.yaml`)
runs on PRs that touch API types, controllers, CRDs, or the chaos artifacts and:

- validates the knowledge model (`validate --knowledge`, `preflight --local`)
- validates experiment definitions (`validate` for each YAML in `chaos/experiments/`)
- diffs the knowledge model between base and PR branches (`diff --breaking`)
- diffs the Notebook CRD schema between base and PR branches (`diff-crds`)
- previews upgrade experiments (`simulate-upgrade --dry-run`)
- runs ChaosClient SDK integration tests (`make test-chaos` in both components)

### ChaosClient SDK tests (L3)

L3 tests are **per-component** — each controller is tested independently for
its resilience to Kubernetes API faults. This is because each controller has its
own reconcile loop and must handle failures (retries, requeues, idempotency) on
its own. Combined system-level resilience testing (controller cooperation under
failure) is the domain of L4 runtime experiments.

The knowledge model (L1/L2) already covers both components as a single operator
because topology, drift detection, and upgrade simulation operate at the
operator level, not per-component.

**odh-notebook-controller** (`chaostests/chaos_test.go`):
Uses an **isolated envtest** (no controller manager) so the chaos reconciler is
the only actor touching the API server. This eliminates race conditions and
makes all fault scenarios — including Create and Delete — fully deterministic.

Covered scenarios:
- Get errors propagate as `sdk.ChaosError`
- Transient Get recovery: reconciler converges after `FaultConfig.Deactivate()`
- Create errors propagate as `sdk.ChaosError`
- Transient Create recovery: reconciler converges after deactivation
- List errors propagate as `sdk.ChaosError`
- Transient List recovery
- Update faults with no drift: reconciler stays healthy
- Delete errors during finalization: reconciler propagates errors
- Transient Delete recovery: finalization completes after faults clear
- Intermittent errors (15% Get + List + Create rate): reconciler converges

**notebook-controller** (`chaostests/chaos_test.go`):
Same isolated envtest pattern. The upstream reconciler always updates the
StatefulSet on every reconcile (via `CopyStatefulSetFields`), so the
"Update-no-drift" scenario does not apply here.

Covered scenarios:
- Get errors propagate as `sdk.ChaosError`
- Transient Get recovery
- Create errors propagate as `sdk.ChaosError`
- Transient Create recovery
- List errors propagate as `sdk.ChaosError`
- Transient List recovery
- Intermittent errors (15% Get + List + Create rate): reconciler converges

### Local validation
```sh
cd components/odh-notebook-controller
make chaos-validate   # validates knowledge model + preflight
make test-chaos       # runs ChaosClient SDK integration tests

cd components/notebook-controller
make test-chaos       # runs ChaosClient SDK integration tests
```

### Maintenance

When CRDs, webhooks, or managed resources change, update
`chaos/knowledge/workbenches.yaml` and the experiment YAMLs in
`chaos/experiments/` in the same PR. If the reconciler gains new
sub-reconcilers or API operations, add corresponding
chaos test scenarios in `chaostests/chaos_test.go`.

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
