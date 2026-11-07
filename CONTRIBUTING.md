# Contributing to ODH Kubeflow

Thank you for your interest in contributing to the ODH Kubeflow project.
This guide covers everything you need to get started with development.

## Prerequisites

Ensure you have the following tools installed:

- [Go](https://golang.org/dl/) — version must be **at least** the version in the
  `go` directive of `components/*/go.mod` (currently 1.25.x)
- [Docker](https://docs.docker.com/install/) or
  [Podman](https://podman.io/getting-started/installation) — for building
  container images
- [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) v1.22+
- [kustomize](https://kubectl.docs.kubernetes.io/installation/kustomize/) v5.0+
- Access to a Kubernetes v1.22+ or OpenShift 4.x cluster
- [pre-commit](https://pre-commit.com/#install) — for local git hooks

## Development Workflow

1. Fork the repository and clone your fork.
2. Create a feature branch from `main`.
3. Make your changes in the relevant component under `components/`.
4. Run tests, linting, and formatting (see sections below).
5. Commit your changes and push to your fork.
6. Open a pull request against `main`.

## Building

Each controller is built independently from its directory:

```sh
# Build all components from the repo root
make build

# Or build a specific component
cd components/notebook-controller && make manager
cd components/odh-notebook-controller && make build
```

To build container images:

```sh
cd components/odh-notebook-controller
make docker-build IMG=<registry>/odh-notebook-controller TAG=<tag>
make docker-push  IMG=<registry>/odh-notebook-controller TAG=<tag>
```

## Testing

### Unit tests

Unit tests use the [Kubernetes envtest framework](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest)
and [Ginkgo](https://onsi.github.io/ginkgo/):

```sh
# Run all unit tests from repo root
make test

# Or run per-component
cd components/notebook-controller && make test
cd components/odh-notebook-controller && make test
```

The ODH controller runs two test passes (with `SET_PIPELINE_RBAC=false` and
`SET_PIPELINE_RBAC=true`). Coverage profiles are written to `cover*.out`.

### End-to-end tests

E2e tests require a running cluster:

```sh
cd components/odh-notebook-controller
export KUBECONFIG=/path/to/kubeconfig
make e2e-test -e K8S_NAMESPACE=<namespace>
```

Use `E2E_TEST_FLAGS="--skip-deletion=true"` to skip notebook deletion tests.

## Debugging

### Running controllers locally

For the ODH controller with webhook support:

```sh
cd components/odh-notebook-controller
make deploy-dev -e K8S_NAMESPACE=<ns>   # Deploys ktunnel for webhook redirect
make run -e K8S_NAMESPACE=<ns>          # Starts controller locally
```

For the upstream controller:

```sh
cd components/notebook-controller
export DEV="true"
make run
```

### Envtest debug options

| Variable                 | Effect                                               |
|--------------------------|------------------------------------------------------|
| `DEBUG_WRITE_KUBECONFIG` | Writes kubeconfig for inspecting the envtest cluster |
| `DEBUG_WRITE_AUDITLOG`   | Writes kube-apiserver audit logs to disk             |

## Linting and Formatting

### Linting

The project uses [golangci-lint](https://golangci-lint.run/) (v2.8.0):

```sh
# Lint all components from repo root
make lint

# Or lint a specific component
cd components/odh-notebook-controller
golangci-lint run --timeout=5m
```

### Formatting

```sh
# Format all components
make fmt

# Or format a specific component
cd components/odh-notebook-controller && go fmt ./...
```

### Module verification

```sh
make verify-modules
```

This runs `go mod verify` and `go mod tidy -diff` for all components.

### Pre-commit hooks

The repository includes a `.pre-commit-config.yaml` that runs linting,
formatting, and module checks automatically before each `git commit`.

**Setup (one-time per clone):**

```sh
# Install pre-commit if you don't have it
pip install pre-commit    # or: dnf install pre-commit / brew install pre-commit

# Install the git hooks into your local repo
pre-commit install
```

After this, every `git commit` will automatically validate staged files.
If any hook fails, the commit is blocked until you fix the issue.

> **Note:** The Go-based hooks (`go vet`, `go mod tidy`) use your system `go`
> binary. Your installed Go version must be **at least** the version declared in
> `components/*/go.mod`. CI enforces this exact version via `go-version-file`,
> so if your local Go is too old, the hooks (or CI) will fail.

**Useful commands:**

```sh
# Run all hooks against all files (not just staged ones)
pre-commit run --all-files

# Run a specific hook by ID
pre-commit run golangci-lint

# Update hook versions to their latest releases
pre-commit autoupdate

# Bypass hooks for a one-off commit (use sparingly)
git commit --no-verify
```

### Vulnerability scanning

```sh
make govulncheck
```

## Code Generation

Generated code (e.g., `zz_generated.deepcopy.go`) must be committed.
Regenerate with:

```sh
make generate
# or equivalently
bash ci/generate_code.sh
```

CI will fail if generated files are out of date.

## Review and Merge Process

- Once the PR is submitted, you can either select specific reviewers or let the bot to select reviewers [automatically](https://prow.ci.openshift.org/plugins?repo=opendatahub-io%2Fnotebooks).
- For the PR to be merged, it must receive 2 reviews from the repository [approvals/reviewers](/OWNERS). Following that, an `/approve` comment must be added by someone with approval rights. If the author of the PR has approval rights, it is preferred that they perform the merge action.
