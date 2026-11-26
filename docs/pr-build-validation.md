# PR build validation (Konflux build simulation)

This workflow simulates, at PR time, what Konflux actually does to a pull
request and what the shipped images must satisfy. It exists because a number
of failures in this repository's history were invisible to every check that
ran on PRs — the unit tests passed, the linters passed, `kustomize build`
passed — and the breakage surfaced only after merge, in the Konflux build or
on a fresh install.

The two Konflux pull-request PipelineRuns define the scope:

| PipelineRun | Image | Dockerfile | Build context |
|---|---|---|---|
| `.tekton/odh-notebook-controller-pull-request.yaml` | `quay.io/opendatahub/odh-notebook-controller:odh-pr` | `odh-notebook-controller/Dockerfile` | `components` |
| `.tekton/odh-kf-notebook-controller-pull-request.yaml` | `quay.io/opendatahub/kubeflow-notebook-controller:odh-pr` | `notebook-controller/Dockerfile` | `components` |

Only those two images are simulated. Other Dockerfiles in the tree are not
built by Konflux from PRs, so validating them would duplicate coverage that
other workflows own and drift from what Konflux actually validates.

## Jobs

| Job | What it proves |
|---|---|
| `gate` | decides, from the changed files, whether the PR touches build inputs at all; `[skip konflux-sim]` in the PR title force-skips |
| `static contracts` | every PipelineRun `dockerfile`/`path-context` resolves; every `COPY` source exists in the build context; `go.mod` and the builder image's `GOLANG_VERSION` agree |
| `hermetic build (odh\|kf)` | each Go module downloads once, then builds with `GOPROXY=off GONOPROXY=none` — the exact `CGO_ENABLED=1 … -tags strictfipsruntime` build the Dockerfile runs |
| `image build (odh\|kf)` | the Konflux image builds from the PipelineRun's dockerfile + context; `/manager --help` runs and prints flags |
| `manifest contracts` | all six shipped overlays render, pass kubeconform (typo guard included), and satisfy the RBAC / webhook / env / params contracts below |
| `flag contract` | every flag the overlays pass to the manager is defined by the binary the image ships |
| `kind deploy` | both PR images deploy into a vanilla Kind cluster, a `Notebook` is admitted through the real webhook and reconciled into a StatefulSet, and neither manager crash-loops |
| `summary` | the only required check: fails unless every job above ran green or was skipped by the gate |

## Manifest contracts and the tickets behind them

Each check in `scripts/manifest_contracts.py` traces to a real failure
(Jira component **Notebooks Server** — "Workbenches" does not exist there):

| Check | Failure it replays |
|---|---|
| `rbac-watch-coverage` | RHOAIENG-100911 — the manager watches Namespaces but the shipped ClusterRole omitted `list/watch`, so a fresh install never reached `DataScienceCluster Ready` |
| `rbac-identity` | RHOAIENG-100514/-100515 class — a binding whose subject ServiceAccount is not in the rendered set |
| `webhook-contract` | RHOAIENG-99636 — webhooks blocked by a NetworkPolicy that does not allow the webhook port; RHOAIENG-84738 — webhook cert secret not mounted |
| `duplicate-env` | RHOAIENG-57579 class — two env vars with the same name; the API server silently keeps the last |
| `manager-flags` / `help-flags` | RHOAIENG-100779 class — an overlay passes a flag the binary does not define; the container crash-loops during flag parsing |
| `image-params` | a `params.env` value that never reaches the rendered workload (stale tag, broken replacement) |
| `config-refs` | a workload referencing a ConfigMap/Secret that nothing renders — discovered live: the kf `notebook-controller-deployment` sits in `CreateContainerConfigError` until `notebook-controller-culler-config` exists (created at runtime by the odh-dashboard/workbenches backend cluster-settings API, not by any manifest in this repo or the operators) |
| `copy-sources` | a Dockerfile `COPY` whose source is missing from the `components` context; the Konflux image build fails after merge |
| go-version contract | RHOAIENG-95421 class — `go.mod` newer than the builder image's Go toolset |

Rendered overlays covered: `config/overlays/{odh,rhoai}` and `config/base`
(odh-notebook-controller), `config/overlays/{kubeflow,openshift,standalone}`
(notebook-controller). Each overlay is validated as an independent unit
because each is applied on its own.

The kubeconform step deliberately does **not** discard the exit status:
`-ignore-missing-schemas` counts a misspelled built-in kind (`Deploymnet`)
as "no schema found", so the JSON report is post-processed and any *skipped*
resource that claims to be a built-in kind fails the job. CRDs and other
CRD-backed kinds (e.g. `ServiceMonitor`) remain allowed to skip.

## Real findings surfaced while building this simulation

Both were caught by the Kind phase on unmodified `main`, fixed in the
simulation with a documented mock/workaround. **Severity is low**: the
platform operator (`opendatahub-io/workbenches-operator`,
`internal/controller/manifests.go`) renders only
`kf-notebook-controller/overlays/openshift` — the `standalone` overlay is
carried in the vendored manifests tree but deployed by no supported
consumer, so these are documented rather than ticketed:

1. **The kf controller cannot start from `config/overlays/standalone`.** The
   rendered Deployment references `notebook-controller-culler-config`, which
   is created at runtime by the odh-dashboard/workbenches backend
   (`clusterSettingsUtils.ts`), not by any manifest in this repo or the
   operators. Until it exists the pod sits in `CreateContainerConfigError`.
2. **The standalone overlay omits the TLS-profile RBAC.** The kf manager
   reads the OpenShift `APIServer` TLS profile with a *dynamic* client; the
   kube-apiserver authorizes before it routes, so on a cluster without the
   `config.openshift.io` API the read returns Forbidden (not NoMatch), the
   manager fail-closes and crash-loops:
   `unable to resolve TLS profile: apiservers.config.openshift.io "cluster"
   is forbidden`. The ClusterRole exists only in
   `config/overlays/openshift/rbac/tls_clusterrole.yaml`. The odh controller
   is unaffected: it reads the profile through a typed (RESTMapper-based)
   client, which fails client-side with NoMatch and falls back to hardened
   defaults.

Either gap only bites if `standalone` is rendered on a cluster without the
odh-dashboard; filing a follow-up is worthwhile only if the maintainers
consider that path supported.

## Local usage

```sh
./scripts/validate-build.sh static
./scripts/validate-build.sh hermetic odh    # or kf
./scripts/validate-build.sh image odh       # or kf
./scripts/validate-build.sh manifests
./scripts/validate-build.sh flag-contract odh
./scripts/validate-build.sh kind            # builds both images, own cluster
```

Requirements: `go`, `kustomize`, `kubectl`, `kind`, `openssl`, `python3` with
PyYAML, and `docker` or `podman`. `kubeconform` is downloaded (pinned by
SHA-256) when `SIM_KUBECONFORM_BIN` is not set. All environment overrides are
prefixed `SIM_` (see the script header); `SIM_BUILD_DIR` keeps renders and
logs between subcommands.

The `kind` subcommand creates (and afterwards deletes) its own cluster named
`kind-kubeflow-sim`; set `SIM_KIND_CLUSTER` to reuse an existing one, which
it will never delete. On OpenShift-only paths it mocks the service-ca TLS
material and patches the webhook `caBundle` itself.

## Known gaps

* **Non-hermetic Konflux builds.** Both PipelineRuns resolve the shared
  `odh-konflux-central` `multi-arch-container-build` pipeline with
  `hermetic=false`, so Konflux builds of this repo are not hermetic today.
  The hermetic job proves the repo *could* be, and catches a dependency the
  prefetch would miss if hermetic builds are turned on.
* **Single-arch.** Neither PipelineRun sets `build-platforms`; the shared
  default builds `linux/x86_64` only. `linux/arm64`, `ppc64le` and `s390x`
  legs of the image are never built or tested (RHOAIENG-79767 class).
* **Kind is not OpenShift.** The Kind phase proves the images deploy and
  reconcile; OpenShift-specific behaviour (service-ca certs, Routes, TLS
  profile, SCCs) is only covered by the static contracts above.
* **Cross-component runtime dependencies** (see *Real findings* above): the
  Kind phase mocks `notebook-controller-culler-config` and applies the
  openshift TLS RBAC so the simulation can exercise the controllers; both
  only affect the unsupported `standalone` overlay path.
* **Platform operator handoff.** Both images are shipped by
  `opendatahub-io/opendatahub-operator` through
  `RELATED_IMAGE_ODH_NOTEBOOK_CONTROLLER_IMAGE` /
  `RELATED_IMAGE_ODH_KF_NOTEBOOK_CONTROLLER_IMAGE` (component keys
  `odh-notebook-controller` / `kf-notebook-controller` in
  `manifests-config.yaml`), and the `workbenches` module e2e lives in
  `opendatahub-io/workbenches-operator` (`tests/e2e`). `DataScienceCluster`-
  level failures (RHOAIENG-99595, RHOAIENG-104015, RHOAIENG-103124) belong
  to that side of the boundary and are out of scope here.
* **Generated-artifact drift and render success** are already covered by
  `code-quality.yaml` (`ci/generate_code.sh`, `ci/kustomize.sh`); this
  workflow does not duplicate them.
