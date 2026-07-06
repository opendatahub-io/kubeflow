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
- Push-built images use the `:main` tag and do not expire.
