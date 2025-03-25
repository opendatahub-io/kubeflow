# ODH Kubeflow

This repository is forked from the [Kubeflow upstream
repository](https://github.com/kubeflow/kubeflow).

## Rebase

Follow the instructions in the [REBASE.md](./REBASE.md) file to understand how
to rebase this repository without conflicts.

## Dependency updates

Follow the instructions in the [DEPENDENCIES.md](./DEPENDENCIES.md) file to
understand how to update the dependencies of this repository.

Go version is to be kept in sync across all of go.mod file, Dockerfile, and the
Go version used by Dockerfile.konflux files in the
[red-hat-data-services/kubeflow](https://github.com/red-hat-data-services/kubeflow) fork.
