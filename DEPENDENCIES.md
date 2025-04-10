# Upgrading Go Version and Dependencies in Kubeflow

This guide outlines the steps to upgrade the Go version and dependencies in the Kubeflow project, including the necessary changes in the red-hat-data-services/kubeflow fork.

## Upgrading Go Version

Upgrading the Go version should be done in a separate PR to isolate the changes and make review easier.

> [!IMPORTANT]  
> Images are to be built in the [ubi8/go-toolset](https://catalog.redhat.com/software/containers/ubi8/go-toolset/5ce8713aac3db925c03774d1) container.
> It contains a customized FIPS-compatible version of Go, that however lags behind the latest upstream Go version.
> Always use a Go version that has a supporting go-toolset image available.

1. Begin by reading [Go release notes](https://go.dev/doc/devel/release) to identify potential incompatibilities.

2. Update the Go version in the following files:
   - `components/**` `/` `go.mod`: Update the `go` directive at the top of the file.
   - `components/**` `/` `Dockerfile`: Update the Go version in the base image.

3. Run the following commands to update and verify the build:

    ```shell
    go mod tidy
    make test
    ```

4. Commit these changes and create a pull request for the Go version upgrade.

   > [!WARNING]  
   > Use the `Manifest List Digest` and not the `Image Digest` when locating sha256 in the Red Hat Image Catalog entry.

5. After merging the Go upgrade PR, update the fork at https://github.com/red-hat-data-services/kubeflow:
   - Locate all `Dockerfile.konflux` files in the repository.
   - Update the Go version in each of these files to match the new version.
   - Refer to the [ubi8/go-toolset](https://catalog.redhat.com/software/containers/ubi8/go-toolset/5ce8713aac3db925c03774d1) in Red Hat image catalog to locate `sha256` hash
   - Create a pull request in the fork repository with these changes.

6. Review CI/CD configuration files (especially openshift/release OCP-CI yamls) that specify a Go version.

## Upgrading Dependencies

Upgrading dependencies can be done separately from the Go version upgrade. However, some dependency upgrades may require a newer Go version.

1. To update all dependencies to their latest minor or patch versions:

    ````shell
    go get -u ./...
    ````

    To update to major versions, you'll need to update import paths manually and run `go get` for each updated package.

2. Run `go mod tidy` to clean up the `go.mod` and `go.sum` files, pay attention to not increasing required Go version, e.g.:

    ````shell
    go mod tidy -go=1.21.9
    go: go.opentelemetry.io/auto/sdk@v1.1.0 requires go@1.22.0, but 1.21.9 is requested
    ````

   (The above suggests to either bump the required Go version or to use an older version of dependency.)

3. Verify that the project still builds and tests pass:

    ````shell
    make test
    ````

4. Review the changes in `go.mod` and `go.sum`. Pay special attention to major version upgrades, as they may include breaking changes.

5. If any dependencies require a newer Go version, you may need to upgrade Go first following the steps in the "Upgrading Go Version" section.

6. Commit the changes to `go.mod` and `go.sum`, and create a pull request for the dependency upgrades.
