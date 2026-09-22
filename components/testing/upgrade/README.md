# Notebook Controller Upgrade Test

This directory contains a lightweight, iterative upgrade test for:

- `components/notebook-controller`
- `components/odh-notebook-controller`

The goal is to validate that upgrading both controllers from a baseline image to
the current target images does not break existing workbench workloads.

## What This Covers

The test performs:

1. cluster mode selection (`kind`, `openshift`, or `auto`)
2. baseline controller deployment
3. workload seeding (running notebook, auth-injected running notebook, stopped notebook)
4. pre-upgrade snapshot capture
5. target controller rollout
6. post-upgrade snapshot capture
7. machine-checkable invariants:
   - running notebook pods are not recreated (UID/start time unchanged)
   - running notebook pods do not increase restart count
   - stopped notebook remains stopped
   - expected generated resources exist (StatefulSet, Service, HTTPRoute, NetworkPolicies)
   - controllers are healthy after rollout
   - optional strict log pattern checks

## What This Does Not Cover

- Full RHOAI operator lifecycle and OLM bundle behavior.
- Full product integration with every dependent controller/service.
- Multi-version upgrade matrix (MVP uses one baseline at a time).
- Rollback validation (target to baseline).
- Baseline uses **current checkout manifests** with baseline **images** from the PR
  target branch tip (`:main` by default), not old git-tag manifests.
- This is intentionally “trunk → branch tip”, not a pinned release N-1 matrix
  (override baselines via flags / workflow_dispatch when you need that).

## Files

- `run-upgrade-test.sh`: orchestrates baseline -> seed -> snapshot -> upgrade -> assert.
- `cluster_kind.sh`: KinD provisioning/prerequisites (Istio, ServiceMonitor CRD, fake OpenShift CRD, Gateway API).
- `cluster_openshift.sh`: validates an existing `oc` login/session.
- `deploy_controllers.sh`: deploy helpers for baseline/target images (`config/overlays/odh` + `kubeflow`/`openshift`).
- `seed-workbenches.sh`: reusable workload seeding script.
- `snapshot-state.sh`: deterministic pre/post snapshots and logs/events capture.
- `assert-invariants.sh`: upgrade assertions.

## Baseline images

Default baseline is the **target branch tip** published on quay (`:main`), so PR CI
exercises upgrade from whatever is on the destination branch to the current checkout:

| Controller | Default baseline |
|---|---|
| notebook-controller | `quay.io/opendatahub/kubeflow-notebook-controller:main` |
| odh-notebook-controller | `quay.io/opendatahub/odh-notebook-controller:main` |

Override for release-pin / N-1 experiments:

```sh
--baseline-kf-image quay.io/opendatahub/kubeflow-notebook-controller:1.10-<sha>
--baseline-odh-image quay.io/opendatahub/odh-notebook-controller:1.10-<sha>
```

(or the same via `workflow_dispatch` inputs / `BASELINE_*_IMAGE` env).

Note: `:main` is a floating tag and may lag git `main` briefly after a merge until
images are rebuilt. Both controllers still deploy with **current-checkout manifests**;
only the container images differ between baseline and target phases.

## Local Usage

Prerequisites:

- `kubectl`, `kustomize`, `kind`, `podman` (or Docker), `openssl`, `grep`, `make`
- for OpenShift mode: `oc`, logged-in cluster, and a registry you can push to

KinD provider: the harness uses Docker when `docker info` works; otherwise it sets
`KIND_EXPERIMENTAL_PROVIDER=podman`. On Podman-only hosts, ensure the socket is up:

```sh
systemctl --user enable --now podman.socket
# optional override:
# export KIND_EXPERIMENTAL_PROVIDER=podman
```

Rootless Podman/KinD often hits inotify limits (`couldn't initialize inotify: too many open files`
in controller logs). Raise them (see [kind known issues](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files)):

```sh
sudo sysctl fs.inotify.max_user_watches=524288
sudo sysctl fs.inotify.max_user_instances=512
# persist:
# echo -e 'fs.inotify.max_user_watches=524288\nfs.inotify.max_user_instances=512' | sudo tee /etc/sysctl.d/99-kind-inotify.conf
```

Istio is installed only when `istiod` is missing or not ready. Force a reinstall with:

```sh
FORCE_ISTIO_INSTALL=true bash components/testing/upgrade/run-upgrade-test.sh --mode kind ...
```

The upgrade harness defaults to a **single-node** KinD config
(`components/testing/upgrade/kind-1-32-single.yaml`). Multi-node configs often fail
at worker join under rootless Podman. To force the CI 2-node config:

```sh
export KIND_CONFIG=components/testing/gh-actions/kind-1-32.yaml
```

To tear down the local cluster (with Podman-backed kind):

```sh
export KIND_EXPERIMENTAL_PROVIDER=podman
kind delete cluster --name odh-upgrade
```

If `kind delete` appears to do nothing, you likely need `KIND_EXPERIMENTAL_PROVIDER=podman`
(the harness sets it during the test run, but not in your shell for manual delete).

### KinD (simplest)

Omit both `--target-*` (or pass `--build-targets`) to build from the current checkout into
`localhost/notebook-controller:upgrade-test` and `localhost/odh-notebook-controller:upgrade-test`,
then run the upgrade:

```sh
bash components/testing/upgrade/run-upgrade-test.sh --mode kind
```
Equivalent explicit form:

```sh
bash components/testing/upgrade/run-upgrade-test.sh --mode kind --build-targets
```

After a build, the script prints a **re-run without rebuild** command. Example:

```sh
bash components/testing/upgrade/run-upgrade-test.sh \
  --mode kind \
  --target-kf-image localhost/notebook-controller:upgrade-test \
  --target-odh-image localhost/odh-notebook-controller:upgrade-test
```

Passing `--target-*` **without** `--build-targets` skips the image build (CI path and fast local iterations).

### OpenShift

`localhost/*` images are not pullable by the cluster. Auto-build requires a registry prefix; images
are built, pushed, then used as targets:

```sh
bash components/testing/upgrade/run-upgrade-test.sh \
  --mode openshift \
  --build-targets \
  --target-image-registry quay.io/<your-org>
```

Or set `TARGET_IMAGE_REGISTRY`. Optional: `--target-image-tag <tag>` (default `upgrade-test`).

You can still pass prebuilt/pushed `--target-kf-image` / `--target-odh-image` and skip `--build-targets`.

Artifacts are written to (gitignored):

- `components/testing/upgrade/artifacts/<timestamp>/`

## CI Workflow

Workflow file:

- `.github/workflows/odh_notebook_controller_upgrade_test.yaml`

Behavior:

- stock runner Podman with `runc` runtime (matches integration CI)
- builds target controller images with `docker-build-no-test` (+ GHCR build cache)
- runs KinD-based upgrade test from target-branch tip (`:main`) → PR tip, passing explicit
  `--target-kf-image` / `--target-odh-image` (no in-script rebuild)
- uploads snapshots/logs/events as artifacts
- supports `workflow_dispatch` baseline image overrides

## Extending Later

- Add baseline matrix (N-1, N-2) in a scheduled workflow.
- Deploy baseline from an old git tag’s manifests (closer RHOAI fidelity).
- Add workload profiles (custom RBAC, additional notebook images, kueue profiles).
- Add optional rollback checks as non-blocking jobs first.
