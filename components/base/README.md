# ODH Notebook Controller

The controller will watch the **Kubeflow Notebook** custom resource events to
extend the Kubeflow notebook controller behavior with the following
capabilities:

- Openshift ingress controller integration.
- Kubernetes RBAC proxy sidecar injection.

![ODH Notebook Controller RBAC injection
diagram](../odh-notebook-controller/assets/odh-notebook-controller-auth-diagram.png)


## Directory Base

This directory base would work as the bridge to include both kubeflow-notebook-controller
and odh-notebook-controller deployment.

## Deployment

Add the following configuration to your `KfDef` object to install the
`odh-notebook-controller` from odh-manifests:

```yaml
...
  - kustomizeConfig:
      repoRef:
        name: manifests
        path: odh-notebook-controller
    name: odh-notebook-controller
```

## Creating Notebooks

Create a notebook object with the image and other parameters such as the
environment variables, resource limits, tolerations, etc:

```shell
notebook_namespace=$(oc config view --minify -o jsonpath='{..namespace}')
cat <<EOF | oc apply -f -
---
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: minimal-notebook
  annotations:
    notebooks.opendatahub.io/inject-auth: "true"
spec:
  template:
    spec:
      containers:
        - name: minimal-notebook
          image: quay.io/thoth-station/s2i-minimal-notebook:v0.3.0
          imagePullPolicy: Always
          workingDir: /opt/app-root/src
          env:
            - name: NOTEBOOK_ARGS
              value: |
                --ServerApp.port=8888
                --ServerApp.token=''
                --ServerApp.password=''
                --ServerApp.base_url=/notebook/${notebook_namespace}/minimal-notebook
          ports:
            - name: notebook-port
              containerPort: 8888
              protocol: TCP
          resources:
            requests:
              cpu: "1"
              memory: 1Gi
            limits:
              cpu: "1"
              memory: 1Gi
          livenessProbe:
            initialDelaySeconds: 10
            periodSeconds: 5
            timeoutSeconds: 1
            successThreshold: 1
            failureThreshold: 3
            httpGet:
              scheme: HTTP
              path: /notebook/${notebook_namespace}/minimal-notebook/api
              port: notebook-port
EOF
```

Open the notebook URL in your browser:

```shell
APP_NAMESPACE="redhat-ods-applications"
NOTEBOOK_NAMESPACE="test-notebook"
NOTEBOOK_NAME="foo"
HTTPROUTE_NAME="nb-${NOTEBOOK_NAMESPACE}-${NOTEBOOK_NAME}"

GATEWAY_URL=$(oc get gateway -n openshift-ingress data-science-gateway -o jsonpath='{.spec.listeners[0].hostname}' 2>/dev/null)
GATEWAY_URL=${GATEWAY_URL:-$(oc get route -n openshift-ingress data-science-gateway -ojsonpath='{.spec.host}')}

NOTEBOOK_PATH=$(oc get httproute -n "${APP_NAMESPACE}" "${HTTPROUTE_NAME}" -o jsonpath='{.spec.rules[0].matches[0].path.value}')

firefox "https://${GATEWAY_URL}/${NOTEBOOK_PATH}"
```

Find more examples in the [notebook tests folder](../tests/resources/notebook-controller/).

## Notebook Culling

The notebook controller will scale to zero all the notebooks with last activity
older than the idle time. The controller will set the
`notebooks.kubeflow.org/last-activity` annotation when it detects a kernel with
activity.

To enable this feature, create a configmap with the culling configuration:

- **ENABLE_CULLING**: Enable culling feature (false by default).
- **IDLENESS_CHECK_PERIOD**: Polling frequency to update notebook last activity.
- **CULL_IDLE_TIME**: Maximum time to scale notebook to zero if no activity.

When the controller scales down the notebook pods, it will add the
`kubeflow-resource-stopped` annotation. Remove this annotation to start the
notebook server again.

For example, poll notebooks activity every 5 minutes and shutdown those that
have been in an idle state for more than 60 minutes:

```yaml
cat <<EOF | oc apply -f -
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: notebook-controller-culler-config
data:
  ENABLE_CULLING: "true"
  CULL_IDLE_TIME: "60" # In minutes (1 hour)
  IDLENESS_CHECK_PERIOD: "5" # In minutes
EOF
```

Restart the notebook controller deployment to refresh the configuration:

```shell
oc rollout restart deploy/notebook-controller-deployment
```

### Culling endpoint

The notebook controller [polls the
activity](https://github.com/kubeflow/kubeflow/blob/100657e8d1072136adf0a39315498b3d510c7c49/components/notebook-controller/pkg/culler/culler.go#L153-L155)
from a specific path:

```go
url := fmt.Sprintf(
    "http://%s.%s.svc.%s/notebook/%s/%s/api/kernels",
    nm, ns, domain, ns, nm)
```

Make sure the notebook is exposing the kernels at this path by configuring the
`base_url` parameter:

```shell
jupyter lab ... \
  --ServerApp.base_url=/notebook/${nb_namespace}/${nb_name}
```

## Notebook GPU

Install the [NVIDIA GPU Operator](https://github.com/NVIDIA/gpu-operator) in
your cluster.

When the operator is installed, make sure it labeled the nodes in your cluster
with the number of GPUs available, for example:

```shell
$ oc get node ${GPU_NODE_NAME} -o yaml | grep "nvidia.com/gpu.count"
nvidia.com/gpu.count: "1"
```

In the notebook object, add the number of GPUs to use in the
`notebook.spec.template.spec.containers.resources` field:

```yaml
resources:
  requests:
    nvidia.com/gpu: "1"
  limits:
    nvidia.com/gpu: "1"
```

Allow the notebook to be scheduled in a GPU node by adding the following
toleration to the `notebook.spec.template.spec.tolerations` field:

```yaml
tolerations:
  - effect: NoSchedule
    key: nvidia.com/gpu
    operator: Exists
```

Finally, create the notebook and wait until it is scheduled in a GPU node.

### Requirements

To update the notebook controller manifests, your environment must have the
following:

- [yq](https://github.com/mikefarah/yq#install) version 4.21.1+.
- [kustomize](https://sigs.k8s.io/kustomize/docs/INSTALL.md) version 3.2.0+

