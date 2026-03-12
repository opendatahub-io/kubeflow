This directory contains Tekton `PipelineRun` definitions used by Konflux for the `opendatahub-io/kubeflow` repository on the `main` branch.

The baseline configuration comes from the stable branch:
https://github.com/opendatahub-io/kubeflow/tree/stable/.tekton

These definitions were adapted with the following changes:

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
