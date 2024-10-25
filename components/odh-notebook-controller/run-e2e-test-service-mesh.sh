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


if test -n "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"; then
    IFS=':' read -r -a CTRL_IMG <<< "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"
    export IMG="${CTRL_IMG[0]}"
    export TAG="${CTRL_IMG[1]}"
fi

if test -n "${KF_NOTEBOOK_CONTROLLER}"; then
    IFS=':' read -r -a KF_NBC_IMG <<< "${KF_NOTEBOOK_CONTROLLER}"
    export KF_IMG="${KF_NBC_IMG[0]}"
    export KF_TAG="${KF_NBC_IMG[1]}"
fi

export K8S_NAMESPACE="${TEST_NAMESPACE}"

# From now on we want to be sure that undeploy and testing project deletion are called

function cleanup() {
    echo "Performing deployment cleanup of the ${0}"
    make undeploy-with-mesh undeploy-service-mesh && oc delete project "${TEST_NAMESPACE}"
}
trap cleanup EXIT

# setup and deploy the controller
oc new-project "${TEST_NAMESPACE}" --skip-config-write

# deploy and run e2e tests
make deploy-service-mesh deploy-with-mesh
make e2e-test-service-mesh
