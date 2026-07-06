# Rebasing upstream

The [**Pull GitHub App**](https://github.com/wei/pull) has been configured to
automatically update the ODH Kubeflow fork with last changes from upstream.

When the bot detects a change in the upstream, it will create a new PR.
[**Example PR**](https://github.com/opendatahub-io/kubeflow/pull/9).

## Approving PR

This PR won't be merged until it is approved by using the [**Openshift CI
commands**](https://prow.k8s.io/command-help).

The PR is created with the `do-not-merge/hold` label to avoid automatic merge.
When you want to approve the PR, firstly remove this label by adding the
following comment to the PR:

```shell
/unhold
```

Finally, add a comment in the PR to approve it:

```shell
/approve
```

Wait until the PR is automatically merged.

## Pull Configuration

The **Pull GitHub App** is configured in the [pull.yml file](.github/pull.yml).
