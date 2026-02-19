#!/usr/bin/env bash

# https://vaneyckt.io/posts/safer_bash_scripts_with_set_euxo_pipefail/
set -Eeuxo pipefail

echo "Running the ${0} setup"

TEST_NAMESPACE="odh-notebook-controller-system"

# Following variables are optional - if not set, the default values in relevant params.env
# will be used for both images. As such, if you want to run tests against your custom changes,
# be sure to perform a docker build and set these variables accordingly!
ODH_NOTEBOOK_CONTROLLER_IMAGE="${ODH_NOTEBOOK_CONTROLLER_IMAGE:-}"
KF_NOTEBOOK_CONTROLLER="${KF_NOTEBOOK_CONTROLLER:-}"

echo "ODH_NOTEBOOK_CONTROLLER_IMAGE=${ODH_NOTEBOOK_CONTROLLER_IMAGE}"
echo "KF_NOTEBOOK_CONTROLLER=${KF_NOTEBOOK_CONTROLLER}"

if test -n "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"; then
    IFS=':' read -r -a CTRL_IMG <<< "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"
    export IMG="${CTRL_IMG[0]}"
    export TAG="${CTRL_IMG[1]}"
    echo "Using custom ODH notebook controller: IMG=${IMG}, TAG=${TAG}"
else
    echo "Using default ODH notebook controller image from params.env"
fi

if test -n "${KF_NOTEBOOK_CONTROLLER}"; then
    IFS=':' read -r -a KF_NBC_IMG <<< "${KF_NOTEBOOK_CONTROLLER}"
    export KF_IMG="${KF_NBC_IMG[0]}"
    export KF_TAG="${KF_NBC_IMG[1]}"
    echo "Using custom KF notebook controller: KF_IMG=${KF_IMG}, KF_TAG=${KF_TAG}"
else
    echo "Using default KF notebook controller image from params.env"
fi

export K8S_NAMESPACE="${TEST_NAMESPACE}"

# From now on we want to be sure that undeploy and testing project deletion are called

function cleanup() {
    local ret_code=0

    echo "Performing deployment cleanup of the ${0}"
    make undeploy || {
        echo "Warning [cleanup]: make undeploy failed, continuing with project deletion!"
        ret_code=1
    }
    oc delete --wait=true --ignore-not-found=true project "${TEST_NAMESPACE}" || {
        echo "Warning [cleanup]: project deletion failed!"
        ret_code=1
    }
    # Clean up e2e test specific RBAC resources
    oc delete --ignore-not-found=true clusterrolebinding odh-notebook-controller-e2e-rolebinding || {
        echo "Warning [cleanup]: e2e ClusterRoleBinding deletion failed!"
    }
    oc delete --ignore-not-found=true clusterrole odh-notebook-controller-e2e-role || {
        echo "Warning [cleanup]: e2e ClusterRole deletion failed!"
    }
    return ${ret_code}
}
trap cleanup EXIT

# assure that the project is deleted on the cluster before running the tests
# Note: We only delete the project here, not calling cleanup() to avoid unnecessary make undeploy
oc delete --wait=true --ignore-not-found=true project "${TEST_NAMESPACE}" || echo "Warning [pre-test-cleanup]: project deletion failed!"

# Wait for the project to be fully deleted before creating a new one
echo "Waiting for project ${TEST_NAMESPACE} to be fully deleted..."
while oc get project "${TEST_NAMESPACE}" &>/dev/null; do
    echo "Project still exists, waiting..."
    sleep 5
done

# setup and deploy the controller
oc new-project "${TEST_NAMESPACE}"

# deploy and run e2e tests
make deploy

# In e2e test scenario, the ODH operator may be managing the ClusterRole and will
# reconcile away any patches we make. Create a separate ClusterRole with all required
# permissions for e2e testing, and bind to it instead.
echo "Creating e2e test ClusterRole with full permissions..."
cat <<EOF | oc apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: odh-notebook-controller-e2e-role
rules:
- apiGroups: [""]
  resources: [configmaps, secrets, serviceaccounts, services]
  verbs: [create, get, list, patch, update, watch]
- apiGroups: [authentication.k8s.io]
  resources: [tokenreviews]
  verbs: [create]
- apiGroups: [authorization.k8s.io]
  resources: [subjectaccessreviews]
  verbs: [create]
- apiGroups: [config.openshift.io]
  resources: [proxies]
  verbs: [get, list, watch]
- apiGroups: [datasciencepipelinesapplications.opendatahub.io]
  resources: [datasciencepipelinesapplications]
  verbs: [get, list, watch]
- apiGroups: [datasciencepipelinesapplications.opendatahub.io]
  resources: [datasciencepipelinesapplications/api]
  verbs: [create, delete, get, patch, update]
- apiGroups: [gateway.networking.k8s.io]
  resources: [gateways]
  verbs: [get, list, watch]
- apiGroups: [gateway.networking.k8s.io]
  resources: [httproutes, referencegrants]
  verbs: [create, delete, get, list, patch, update, watch]
- apiGroups: [image.openshift.io]
  resources: [imagestreams]
  verbs: [get, list, watch]
- apiGroups: [kubeflow.org]
  resources: [notebooks]
  verbs: [get, list, patch, update, watch]
- apiGroups: [kubeflow.org]
  resources: [notebooks/finalizers]
  verbs: [patch, update]
- apiGroups: [kubeflow.org]
  resources: [notebooks/status]
  verbs: [get]
- apiGroups: [networking.k8s.io]
  resources: [networkpolicies]
  verbs: [create, get, list, patch, update, watch]
- apiGroups: [networking.k8s.io]
  resources: [networkpolicies/finalizers]
  verbs: [patch, update]
- apiGroups: [oauth.openshift.io]
  resources: [oauthclients]
  verbs: [delete, get, list, patch, update, watch]
- apiGroups: [rbac.authorization.k8s.io]
  resources: [clusterrolebindings, rolebindings]
  verbs: [create, delete, get, list, patch, update, watch]
- apiGroups: [rbac.authorization.k8s.io]
  resources: [roles]
  verbs: [create, get, list, patch, update, watch]
- apiGroups: [route.openshift.io]
  resources: [routes]
  verbs: [get, list, watch]
EOF

# Create a ClusterRoleBinding for the test namespace's ServiceAccount.
echo "Creating ClusterRoleBinding for e2e test namespace..."
cat <<EOF | oc apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: odh-notebook-controller-e2e-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: odh-notebook-controller-e2e-role
subjects:
- kind: ServiceAccount
  name: odh-notebook-controller-manager
  namespace: ${TEST_NAMESPACE}
EOF

# Restart the controller to pick up the new RBAC permissions
echo "Restarting odh-notebook-controller to apply RBAC changes..."
oc rollout restart deployment/odh-notebook-controller-manager -n "${TEST_NAMESPACE}"

# Wait for the rollout to complete (this ensures old pods are terminated)
echo "Waiting for odh-notebook-controller rollout to complete..."
oc rollout status deployment/odh-notebook-controller-manager -n "${TEST_NAMESPACE}" --timeout=120s

# Wait for the controller to be ready
echo "Waiting for odh-notebook-controller deployment to be available..."
oc wait --for=condition=available --timeout=120s deployment/odh-notebook-controller-manager -n "${TEST_NAMESPACE}"

echo "Waiting for odh-notebook-controller pod to be ready..."
oc wait --for=condition=Ready --timeout=120s pod -l app=odh-notebook-controller -n "${TEST_NAMESPACE}"

make e2e-test
