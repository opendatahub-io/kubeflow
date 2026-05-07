This directory contains Tekton `PipelineRun` definitions used by Konflux for the `opendatahub-io/kubeflow` repository on the `main` branch.

The baseline configuration comes from the stable branch:
https://github.com/opendatahub-io/kubeflow/tree/stable/.tekton

## Pull-request pipelines

The PR pipeline definitions (`*-pull-request.yaml`) were adapted from stable with the following changes:

- Branch references were updated from `stable` to `main`.
- PR-built images are configured to expire after `7d`.
- Added an explicit `on-cel-expression` so builds run only when relevant files or directories change.
- Added pipeline timeouts.
- Configured running pipelines to cancel when a new change is pushed to the PR.
- Additional pipeline parameters were added:

```yaml
- name: enable-group-testing
  value: "true"
- name: build-source-image
  value: "false"
- name: skip-checks
  value: "true"
- name: enable-slack-failure-notification
  value: "false"
```

## Push pipelines

The push pipeline definitions (`*-push.yaml`) trigger when changes are merged to `main`. They build multi-arch container images, push them to Quay, and trigger e2e group testing.

Key differences from the stable branch push pipelines:

- Added `pathChanged()` filter in the CEL expression so builds only trigger when `components/` or `.tekton/` files change.
- Added pipeline timeouts (2h pipeline / 1h per task).
- `pipeline-type` is set to `"kubeflow-main-build"` instead of the default `"push"`. This deliberately prevents the `trigger-operator-build` task from running, which would otherwise kick off downstream operator, operator-bundle, and FBC fragment CI builds. Those downstream triggers are not needed on the `main` branch.
- `enable-group-testing` is set to `"true"` to run e2e tests after a successful build.
- Push-built images use the `:odh-main` tag and do not expire.

## Promotion e2e gate

The `kubeflow-promotion-e2e-gate.yaml` pipeline runs as a quality gate for branch promotions (`main` → `stable`, `stable` → `v1.10-branch`). It triggers automatically on PRs targeting `stable` or `v1.10-branch`, and can be re-triggered manually with a `/run-e2e-gate` comment.

Unlike the group test (which deploys controllers directly via kustomize), this pipeline:

1. Provisions an ephemeral HyperShift cluster via Konflux EaaS.
2. Installs the full ODH operator from the `odh-stable` CatalogSource (`quay.io/opendatahub/opendatahub-operator-catalog:odh-stable`).
3. Patches the deployed controller images with the PR-built versions of `odh-notebook-controller` and `kubeflow-notebook-controller`.
4. Creates a `DataScienceCluster` and waits for all components to be ready.
5. Runs the workbenches e2e test suite from [`opendatahub-io/opendatahub-tests`](https://github.com/opendatahub-io/opendatahub-tests) (`uv run pytest tests/workbenches/`).

The pipeline definition lives in [`odh-konflux-central`](https://github.com/opendatahub-io/odh-konflux-central) at `integration-tests/kubeflow/promotion-e2e-gate-pipeline.yaml`.
