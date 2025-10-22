# ODH Notebook Controller

The controller will watch the **Kubeflow Notebook** custom resource events to
extend the Kubeflow notebook controller behavior with the following
capabilities:

- Openshift ingress controller integration.
- Kubernetes RBAC proxy sidecar injection.

It has been developed using **Golang** and
**[Kubebuilder](https://book.kubebuilder.io/quick-start.html)**.

## Implementation detail

By default, when the ODH notebook controller is deployed along with the
Kubeflow notebook controller, it will expose the notebook through the Gateway API
by creating an `HTTPRoute` object that routes traffic to the notebook service.

If the notebook annotation `notebooks.opendatahub.io/inject-auth` is set to
true, the kube-rbac-proxy will be injected as a sidecar proxy in the notebook
deployment to provide RBAC-based authorization:

```yaml
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: example
  annotations:
    notebooks.opendatahub.io/inject-auth: "true"
```

A [mutating webhook](./controllers/notebook_webhook.go) is part of the ODH
notebook controller, it will add the sidecar to the notebook deployment. The
controller will create all the objects needed by the proxy as explained in the
following diagram:

![ODH Notebook Controller kube-rbac-proxy injection
diagram](./assets/odh-notebook-controller-auth-diagram.png)

When accessing the notebook, you will have to authenticate with your Openshift
user, and you will only be able to access it if you have the necessary
permissions.

### kube-rbac-proxy Authorization

For kube-rbac-proxy injection, the authorization is delegated to Kubernetes RBAC through
the kube-rbac-proxy configuration. The proxy performs `SubjectAccessReview` checks
against the Kubernetes API to verify that the user has the necessary permissions.

The kube-rbac-proxy is configured with the following authorization rules:

```yaml
authorization:
  resourceAttributes:
    verb: get
    resource: notebooks
    apiGroup: kubeflow.org
    name: example
    namespace: opendatahub
```

This means users will only be able to access the notebook if they have the `get`
permission on the specific notebook resource in the specified namespace:

```shell
oc get notebook example -n <YOUR_NAMESPACE>
```

## Developer docs

Follow the instructions below if you want to extend the controller
functionality:

### Run unit tests

Unit tests have been developed using the [**Kubernetes envtest
framework**](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest).

Run the following command to execute them:

```shell
make test
```

Useful options for running tests (that are already included in the above `make` command) are

| Option                     | Description                                                                                   |
|----------------------------|-----------------------------------------------------------------------------------------------|
| KUBEBUILDER_ASSETS        | Environment variable that specifies path to where Kubebuilder has downloaded envtest binaries |
| -ginkgo.v -ginkgo.progress | Enables verbose output from Ginkgo                                                            |
| -test.v                    | Enables verbose output from Go tests (*testing.T)                                             |

The following environment variables are used to enable additional debug options for the tests

| Environment variable   | Description                                                                                                                                  |
|------------------------|----------------------------------------------------------------------------------------------------------------------------------------------|
| DEBUG_WRITE_KUBECONFIG | Writes a Kubeconfig file to disk. It can be used with `kubectl` or `k9s` to examine the envtest cluster when test is paused on a breakpoint. |
| DEBUG_WRITE_AUDITLOG   | Writes kube-apiserver auditlogs to disk. The config is in `envtest-audit-policy.yaml`, set the namespace of interest there.                  |

### Run locally

Install the `notebooks.kubeflow.org` CRD from the [Kubeflow notebook
controller](../notebook-controller) repository as a requirement.

When running the controller locally, the [admission webhook](./config/webhook)
will be running in your local machine. The requests made by the Openshift API
have to be redirected to the local port.

This will be solved by deploying the [Ktunnel
application](https://github.com/omrikiei/ktunnel) in your cluster instead of the
controller manager, it will create a reverse tunnel between the cluster and your
local machine:

```shell
make deploy-dev -e K8S_NAMESPACE=<YOUR_NAMESPACE>
```

Run the controller locally:

```shell
make run -e K8S_NAMESPACE=<YOUR_NAMESPACE>
```

### Deploy local changes

Build a new image with your local changes and push it to `<YOUR_IMAGE>` (by
default `quay.io/opendatahub/odh-notebook-controller`).

```shell
make image -e IMG=<YOUR_IMAGE>
```

Deploy the manager using the image in your registry:

```shell
make deploy -e K8S_NAMESPACE=<YOUR_NAMESPACE> -e IMG=<YOUR_IMAGE>
```

### Run e2e Tests

A user can run the e2e tests in the same namespace as the controllers. To deploy 
kubeflow notebook controller and ODH Notebook controller refer to [this](#run-locally) section. The
following environment variables must be set when running locally:

```shell
export KUBECONFIG=/path/to/kubeconfig
```

Once the above variables are set, run the following:

```shell
make e2e-test -e K8S_NAMESPACE=<YOUR_NAMESPACE>
```

Additional flags that can be passed to e2e-tests by setting up `E2E_TEST_FLAGS`
variable. Following table lists all the available flags to run the tests:

| Flag            | Description                                                                                                                                         | Default value |
|-----------------|-----------------------------------------------------------------------------------------------------------------------------------------------------|---------------|
| --skip-deletion | To skip running  of `notebook-deletion` test that includes deleting `Notebook` resources. Assign this variable to `true` to skip Notebook deletion. | false         |



Example command to run full test suite in a custom namespace, skipping the test
for Notebook deletion.

```shell
make e2e-test -e K8S_NAMESPACE=<YOUR_NAMESPACE> -e E2E_TEST_FLAGS="--skip-deletion=true"
```
