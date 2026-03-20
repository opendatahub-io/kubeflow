# Architecture

This document describes the high-level architecture of the ODH Kubeflow
repository. It is intended for contributors and AI agents working with the
codebase.

## Overview

This repository is the **OpenDataHub (ODH) fork** of
[kubeflow/kubeflow](https://github.com/kubeflow/kubeflow). It focuses on the
**Notebook** subsystem — two Kubernetes controllers that manage Jupyter notebook
workloads on OpenShift and Kubernetes clusters.

The upstream Kubeflow notebook controller handles core lifecycle management,
while the ODH notebook controller adds OpenShift-specific capabilities such as
ingress routing, authentication proxy injection, and DSPA secret management.

## Directory Structure

```text
.
├── components/
│   ├── notebook-controller/         # Upstream Kubeflow Notebook controller
│   ├── odh-notebook-controller/     # ODH extensions (webhook, routing, auth)
│   ├── common/                      # Shared Go library code
│   ├── base/                        # Base kustomize overlays
│   ├── proposals/                   # Design proposals
│   └── testing/                     # Shared test utilities
├── ci/                              # CI helper scripts
│   ├── generate_code.sh             # Regenerate all generated Go code
│   └── kustomize.sh                 # Validate kustomize manifests
├── conformance/                     # Kubeflow conformance test configs
├── .github/
│   ├── workflows/                   # GitHub Actions CI pipelines
│   └── ISSUE_TEMPLATE/              # Issue and bug report templates
├── AGENTS.md                        # AI agent context file
├── ARCHITECTURE.md                  # This file
├── CONTRIBUTING.md                  # Developer guide
├── OWNERS                           # Prow-style reviewer/approver list
└── Makefile                         # Root-level build/test orchestration
```

## Components

### notebook-controller (upstream)

**Path:** `components/notebook-controller/`

The upstream Kubeflow Notebook controller watches `Notebook` custom resources
(`kubeflow.org/v1`) and reconciles them into Kubernetes workloads:

- Creates a **StatefulSet** for each Notebook (one replica running the notebook
  server container)
- Creates a **Service** to expose the notebook pod
- Supports **idle culling** — scales the StatefulSet to zero after a
  configurable idle period, and scales it back up on access

Key source files:
- `controllers/notebook_controller.go` — main reconciliation loop
- `controllers/culling_controller.go` — idle notebook culling logic
- `config/crd/` — CRD definitions for `notebooks.kubeflow.org`

### odh-notebook-controller (ODH extensions)

**Path:** `components/odh-notebook-controller/`

The ODH Notebook controller extends the upstream behavior with
OpenShift-specific features. It runs alongside the upstream controller and
watches the same `Notebook` CRD:

- **Gateway API routing** — creates `HTTPRoute` objects for each notebook to
  enable ingress via the Kubernetes Gateway API
- **kube-rbac-proxy injection** — a mutating webhook injects a kube-rbac-proxy
  sidecar when `notebooks.opendatahub.io/inject-auth: "true"` is set, providing
  RBAC-based authorization via `SubjectAccessReview`
- **DSPA secret management** — manages Data Science Pipelines Application
  secrets for notebook integration
- **Feast configuration** — injects Feast feature store configuration
- **Runtime class handling** — manages notebook runtime configurations

Key source files:
- `controllers/notebook_controller.go` — ODH reconciliation loop
- `controllers/notebook_webhook.go` — mutating admission webhook
- `controllers/notebook_route.go` — HTTPRoute management
- `controllers/notebook_kube_rbac_auth.go` — kube-rbac-proxy resources
- `controllers/notebook_dspa_secret.go` — DSPA secret handling
- `controllers/notebook_feast_config.go` — Feast integration
- `e2e/` — end-to-end test suite

### common

**Path:** `components/common/`

Shared Go library code used by both controllers (e.g., shared types and
utility functions).

## Notebook CRD

The primary custom resource is `Notebook` (`kubeflow.org/v1`):

```yaml
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: my-notebook
  annotations:
    notebooks.opendatahub.io/inject-auth: "true"   # ODH: enable RBAC proxy
spec:
  template:
    spec:
      containers:
        - name: my-notebook
          image: <notebook-image>
```

The `spec.template` field is a `NotebookTemplateSpec` containing a nested `spec`
field that holds the standard Kubernetes `corev1.PodSpec` (see
`components/notebook-controller/api/v1/notebook_types.go`). The full path to
container definitions is therefore `spec.template.spec.containers[]`, as shown
in the YAML example above.

## Request Flow

```text
User → Gateway/Ingress → HTTPRoute (created by ODH controller)
                              ↓
                      Notebook Service
                              ↓
                    StatefulSet Pod
                    ├── notebook container
                    └── kube-rbac-proxy sidecar (if inject-auth=true)
                              ↓
                    SubjectAccessReview (Kubernetes RBAC check)
```

### Webhook Flow

When a Notebook CR is created or updated:

1. The Kubernetes API server sends an admission review to the ODH mutating
   webhook
2. The webhook checks for ODH-specific annotations
3. If `inject-auth: "true"`, the webhook mutates the pod template to inject the
   kube-rbac-proxy sidecar container with appropriate RBAC configuration
4. The mutated Notebook CR is persisted
5. Both controllers reconcile — upstream creates the StatefulSet/Service, ODH
   creates the HTTPRoute and supporting resources

The following diagram (from
`components/odh-notebook-controller/assets/odh-notebook-controller-auth-diagram.txt`)
shows the full interaction between both controllers, the webhook, and all
resources created by the ODH controller:

```text
                                                                     ┌──────┐
             1. New Notebook      ┌─────────────────────┐ 3.Store    │      │
             ────────────────────►│    Openshift API    ├───────────►│ etcd │
                                  └──▲───┬───────────▲──┘            │      │
                                     │   │           │               └──────┘
                                     │   │           │
                         4.Watch Nb  │   │           │ 5.Watch Nb
                        ┌────────────┘   │           └──────────┐
                        │                │                      │
                        │                │                      │
                        │                │                      │
              ┌─────────┴───────────┐    │            ┌─────────┴───────────┐
    4.1 Create│      Kubeflow       │    │            │        ODH          │5.1 Create
         ┌────┤ Notebook Controller │    │            │ Notebook Controller ├────┐
         │    └─────────────────────┘    │            ├─────────────────────┤    │
         │                               │ 2. Auth    ├─────────────────────┤    │
         │                               └───────────►│       Webhook       │    │
         │                                            └─────────────────────┘    │
         │                                                                       │
         │                                            ┌─────────────────────┐    │
         │                                            │     Notebook SA     ◄────┤
         │    ┌─────────────────────┐                 └─────────────────────┘    │
         └────►    Notebook STS     │                                            │
              └─────────────────────┘                 ┌─────────────────────┐    │
                                                      │ kube-rbac-proxy CM  ◄────┤
                                                      └─────────────────────┘    │
                                                                                 │
                                                      ┌─────────────────────┐    │
                                                      │ kube-rbac-proxy Svc ◄────┤
                                                      └─────────────────────┘    │
                                                                                 │
                                                      ┌─────────────────────┐    │
                                                      │ Notebook HTTPRoute  ◄────┤
                                                      └─────────────────────┘    │
                                                                                 │
                                                      ┌─────────────────────┐    │
                                                      │ ClusterRoleBinding  ◄────┤
                                                      └─────────────────────┘    │
                                                                                 │
                                                      ┌─────────────────────┐    │
                                                      │  Network Policies   ◄────┘
                                                      └─────────────────────┘
```

## Multi-Module Go Layout

The repository uses a **multi-module** Go layout. There is no root `go.mod`.
Each component has its own module:

- `components/notebook-controller/go.mod` — module `github.com/kubeflow/kubeflow/components/notebook-controller`
- `components/odh-notebook-controller/go.mod` — module `github.com/opendatahub-io/kubeflow/components/odh-notebook-controller`
- `components/common/go.mod` — shared library module

The ODH controller depends on the upstream controller for CRD types and the
common module for shared utilities.

## CI / CD

### GitHub Actions

GitHub Actions workflows in `.github/workflows/` handle unit tests, integration
tests, and code quality checks:

| Workflow | Purpose |
|----------|---------|
| `notebook_controller_unit_test.yaml` | Unit tests + Codecov for upstream controller |
| `odh_notebook_controller_unit_test.yaml` | Unit tests + Codecov for ODH controller |
| `notebook_controller_integration_test.yaml` | Integration tests for upstream controller |
| `odh_notebook_controller_integration_test.yaml` | Integration tests for ODH controller |
| `code-quality.yaml` | golangci-lint, go mod tidy, govulncheck, kustomize |

All GitHub Actions workflows run on pull requests to `main`.

### Tekton / Konflux

Container image builds and end-to-end testing are handled by Tekton
`PipelineRun` definitions in `.tekton/`, managed through
[Konflux](https://github.com/opendatahub-io/odh-konflux-central) with
Pipelines-as-Code:

| Pipeline | Purpose |
|----------|---------|
| `odh-notebook-controller-pull-request.yaml` | Build `odh-notebook-controller` image on PRs |
| `odh-kf-notebook-controller-pull-request.yaml` | Build `kubeflow-notebook-controller` image on PRs |
| `kubeflow-group-test.yaml` | Group integration/e2e test (triggered via `/group-test` comment) |

Build pipelines run automatically when files under `components/` or `.tekton/`
change. PR-built images expire after 7 days. The group test pipeline references
a central test pipeline in `odh-konflux-central` and can run for up to 10 hours.

## Deployment

Both controllers are deployed via **kustomize** overlays:

- `components/notebook-controller/config/` — CRDs, RBAC, manager deployment
- `components/odh-notebook-controller/config/` — webhook config, RBAC,
  manager deployment with OpenShift overlays

The ODH controller's `make deploy` target deploys both controllers together.

## Relationship to Upstream

This fork tracks `kubeflow/kubeflow` and periodically rebases (see
[REBASE.md](./REBASE.md)). The upstream `notebook-controller` is kept as close
to upstream as possible, while `odh-notebook-controller` contains all
ODH/RHOAI-specific extensions.
