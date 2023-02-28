#!/usr/bin/env bash

TEST_NAMESPACE="odh-notebook-controller-system"

# setup and deploy the controller
oc new-project $TEST_NAMESPACE -n $TEST_NAMESPACE --skip-config-write

IFS=':' read -r -a CTRL_IMG <<< "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"
export IMG="${CTRL_IMG[0]}"
export TAG="${CTRL_IMG[1]}"
IFS=':' read -r -a KF_NBC_IMG <<< "${KF_NOTEBOOK_CONTROLLER}"
export KF_IMG="${KF_NBC_IMG[0]}"
export KF_TAG="${KF_NBC_IMG[1]}"
export K8S_NAMESPACE=$TEST_NAMESPACE

make deploy

# run e2e tests
make e2e-test

# cleanup deployment
make undeploy
oc delete project $TEST_NAMESPACE -n $TEST_NAMESPACE
