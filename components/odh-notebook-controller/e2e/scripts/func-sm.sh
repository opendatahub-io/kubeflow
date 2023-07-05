#!/bin/bash

##########################################################
#
# Functions for managing Service Mesh installs.
#
##########################################################

set -u

install_servicemesh_operators() {
  local servicemesh_subscription_source="${1}"
  local elasticsearch_subscription_channel="stable"

  echo "Installing the Service Mesh Operators"
  cat <<EOM | ${OC} apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: elasticsearch-operator
  namespace: openshift-operators
spec:
  channel: "${elasticsearch_subscription_channel}"
  installPlanApproval: Automatic
  name: elasticsearch-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: jaeger-product
  namespace: openshift-operators
spec:
  channel: stable
  installPlanApproval: Automatic
  name: jaeger-product
  source: redhat-operators
  sourceNamespace: openshift-marketplace
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: servicemeshoperator
  namespace: openshift-operators
spec:
  channel: stable
  installPlanApproval: Automatic
  name: servicemeshoperator
  source: ${servicemesh_subscription_source}
  sourceNamespace: openshift-marketplace
EOM
}

install_smcp() {
  local control_plane_namespace="${1}"
  local enable_kiali="${2}"
  local smcp_version="${3}"
  local smcp_yaml_file="${4:-}"

  echo "Waiting for CRDs to be established."
  for crd in servicemeshcontrolplanes.maistra.io servicemeshmemberrolls.maistra.io jaegers.jaegertracing.io
  do
    echo -n "Waiting for CRD [${crd}]..."
    while ! ${OC} get crd ${crd} >& /dev/null ; do echo -n '.'; sleep 1; done
    ${OC} wait --for condition=established crd/${crd}
  done

  echo -n "Waiting for Service Mesh operator deployment to be created..."
  while ! ${OC} get deployment -n openshift-operators -o name | grep istio >& /dev/null ; do echo -n '.'; sleep 1; done
  echo "done."
  servicemesh_deployment="$(${OC} get deployment -n openshift-operators -o name | grep istio)"

  echo -n "Waiting for Jaeger operator deployment to be created..."
  while ! ${OC} get deployment -n openshift-operators -o name | grep jaeger >& /dev/null ; do echo -n '.'; sleep 1; done
  echo "done."
  jaeger_deployment="$(${OC} get deployment -n openshift-operators -o name | grep jaeger)"

  echo "Waiting for operator deployments to start..."
  for op in ${servicemesh_deployment} ${jaeger_deployment}
  do
    echo -n "Waiting for ${op} to be ready..."
    readyReplicas="0"
    while [ "$?" != "0" -o "$readyReplicas" == "0" ]
    do
      sleep 1
      echo -n '.'
      readyReplicas="$(${OC} get ${op} -n openshift-operators -o jsonpath='{.status.readyReplicas}' 2> /dev/null)"
    done
    echo "done."
  done

  echo "Wait for the servicemesh operator to be Ready."
  ${OC} wait --for condition=Ready $(${OC} get pod -n openshift-operators -l name=istio-operator -o name) --timeout 300s -n openshift-operators
  echo "done."

  echo "Wait for the servicemesh validating webhook to be created."
  while [ "$(${OC} get validatingwebhookconfigurations -o name | grep servicemesh)" == "" ]; do echo -n '.'; sleep 5; done
  echo "done."

  echo "Wait for the servicemesh mutating webhook to be created."
  while [ "$(${OC} get mutatingwebhookconfigurations -o name | grep servicemesh)" == "" ]; do echo -n '.'; sleep 5; done
  echo "done."

  if ! ${OC} get namespace ${control_plane_namespace} >& /dev/null; then
    echo "Creating control plane namespace: ${control_plane_namespace}"
    ${OC} create namespace ${control_plane_namespace}
  fi

  echo "Installing SMCP"
  if [ "${smcp_yaml_file}" == "" ]; then
    smcp_yaml_file="/tmp/maistra-smcp.yaml"
    cat <<EOM > ${smcp_yaml_file}
apiVersion: maistra.io/v2
kind: ServiceMeshControlPlane
metadata:
  name: custom
spec:
  version: ${smcp_version}
  proxy:
    injection:
      autoInject: true
  gateways:
    openshiftRoute:
      enabled: false
  tracing:
    type: Jaeger
  addons:
    jaeger:
      name: jaeger
      install: {}
    grafana:
      enabled: true
      install: {}
    kiali:
      name: kiali
      enabled: ${enable_kiali}
      install: {}
    prometheus:
      enabled: true
---
apiVersion: maistra.io/v1
kind: ServiceMeshMemberRoll
metadata:
  name: default
spec:
  members: []
EOM
  fi

  while ! ${OC} apply -n ${control_plane_namespace} -f ${smcp_yaml_file}
  do
    echo "WARNING: Failed to apply [${smcp_yaml_file}] to namespace [${control_plane_namespace}] - will retry in 5 seconds to see if the error condition clears up..."
    sleep 5
  done
  echo "[${smcp_yaml_file}] has been successfully applied to namespace [${control_plane_namespace}]."
}

delete_servicemesh_operators() {
  local abort_operation="false"
  for cr in \
    $(${OC} get smm --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' ) \
    $(${OC} get smmr --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' ) \
    $(${OC} get smcp --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' )
  do
    abort_operation="true"
    local res_kind=$(echo ${cr} | cut -d: -f1)
    local res_namespace=$(echo ${cr} | cut -d: -f2)
    local res_name=$(echo ${cr} | cut -d: -f3)
    echo "A [${res_kind}] resource named [${res_name}] in namespace [${res_namespace}] still exists. You must delete it first."
  done
  if [ "${abort_operation}" == "true" ]; then
    echo "Aborting"
    exit 1
  fi

  echo "Unsubscribing from the Service Mesh operators"
  for sub in $(${OC} get subscriptions -n openshift-operators -o name | grep -E 'servicemesh|jaeger|elasticsearch')
  do
    ${OC} delete -n openshift-operators ${sub}
  done

  echo "Deleting OLM CSVs which uninstalls the operators and their related resources"
  for csv in $(${OC} get csv --all-namespaces --no-headers -o custom-columns=NS:.metadata.namespace,N:.metadata.name | sed 's/  */:/g' | grep -E 'servicemesh|jaeger|elasticsearch')
  do
    ${OC} delete csv -n $(echo -n $csv | cut -d: -f1) $(echo -n $csv | cut -d: -f2)
  done

  echo "Deleting any Istio clusterroles/bindings that are getting left behind"
  for r in \
    $(${OC} get clusterrolebindings -o name | grep -E 'istio') \
    $(${OC} get clusterroles -o name | grep -E 'istio')
  do
    ${OC} delete ${r}
  done

  echo "Delete Istio service accounts, configmaps, secrets that are getting left behind"
  for r in \
    $(${OC} get sa -n openshift-operators -o name | grep -E 'istio') \
    $(${OC} get configmaps -n openshift-operators -o name | grep -E 'istio') \
    $(${OC} get secrets -n openshift-operators -o name | grep -E 'istio')
  do
    ${OC} delete -n openshift-operators ${r}
  done

  # See: https://docs.openshift.com/container-platform/4.7/service_mesh/v2x/removing-ossm.html
  echo "Clean up validating webhooks"
  ${OC} delete validatingwebhookconfiguration/openshift-operators.servicemesh-resources.maistra.io
  #${OC} delete validatingwebhookconfiguration/istiod-istio-system
  echo "Clean up mutating webhooks"
  ${OC} delete mutatingwebhookconfigurations/openshift-operators.servicemesh-resources.maistra.io
  #${OC} delete mutatingwebhookconfigurations/istio-sidecar-injector
  echo "Clean up services"
  ${OC} delete -n openshift-operators svc maistra-admission-controller
  echo "Clean up deamonsets"
  ${OC} delete -n openshift-operators daemonset/istio-node
  #${OC} delete -n kube-system daemonset/istio-cni-node
  echo "Clean up some more clusterroles/bindings"
  ${OC} delete clusterrole/istio-admin clusterrole/istio-cni clusterrolebinding/istio-cni
  ${OC} delete clusterrole istio-view istio-edit
  echo "Clean up some security related things from the operator"
  ${OC} delete -n openshift-operators configmap/maistra-operator-cabundle
  ${OC} delete -n openshift-operators secret/maistra-operator-serving-cert
  echo "Clean up Jaeger"
  ${OC} delete clusterrole jaegers.jaegertracing.io-v1-admin jaegers.jaegertracing.io-v1-crdview jaegers.jaegertracing.io-v1-edit jaegers.jaegertracing.io-v1-view
  echo "Clean up CNI"
  ${OC} delete cm -n openshift-operators istio-cni-config
  ${OC} delete sa -n openshift-operators istio-cni
  echo "Delete the CRDs"
  ${OC} get crds -o name | grep '.*\.istio\.io' | xargs -r -n 1 ${OC} delete
  ${OC} get crds -o name | grep '.*\.maistra\.io' | xargs -r -n 1 ${OC} delete
  ${OC} get crds -o name | grep '.*\.jaegertracing\.io' | xargs -r -n 1 ${OC} delete
  #${OC} get crds -o name | grep '.*\.logging\.openshift\.io' | xargs -r -n 1 ${OC} delete
}

delete_smcp() {
  echo "Deleting all SMCP, SMMR, and SMM CRs (if they exist) which uninstalls all the Service Mesh components"
  local doomed_namespaces=""
  for cr in \
    $(${OC} get smm --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' ) \
    $(${OC} get smmr --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' ) \
    $(${OC} get smcp --all-namespaces -o custom-columns=K:.kind,NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' )
  do
    local res_kind=$(echo ${cr} | cut -d: -f1)
    local res_namespace=$(echo ${cr} | cut -d: -f2)
    local res_name=$(echo ${cr} | cut -d: -f3)
    ${OC} delete -n ${res_namespace} ${res_kind} ${res_name}
    doomed_namespaces="$(echo ${res_namespace} ${doomed_namespaces} | tr ' ' '\n' | sort -u)"
  done

  echo "Deleting the control plane namespaces"
  for ns in ${doomed_namespaces}
  do
    ${OC} delete namespace ${ns}
  done
}

status_servicemesh_operators() {
  echo
  echo "===== SERVICEMESH OPERATORS SUBSCRIPTIONS"
  local all_subs="$(${OC} get subscriptions -n openshift-operators -o name | grep -E 'servicemesh|jaeger|elasticsearch')"
  if [ ! -z "${all_subs}" ]; then
    ${OC} get --namespace openshift-operators ${all_subs}
    echo
    echo "===== SERVICEMESH OPERATORS PODS"
    local all_pods="$(${OC} get pods -n openshift-operators -o name | grep -E 'istio|jaeger|elasticsearch')"
    [ ! -z "${all_pods}" ] && ${OC} get --namespace openshift-operators ${all_pods} || echo "There are no pods"
  else
    echo "There are no Subscriptions for the Service Mesh Operators"
  fi
}

status_smcp() {
  echo
  echo "===== SMCPs"
  if [ "$(${OC} get smcp --all-namespaces 2> /dev/null | wc -l)" -gt "0" ] ; then
    echo "One or more SMCPs exist in the cluster"
    ${OC} get smcp --all-namespaces
    echo
    for cr in \
      $(${OC} get smcp --all-namespaces -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' )
    do
      local res_namespace=$(echo ${cr} | cut -d: -f1)
      local res_name=$(echo ${cr} | cut -d: -f2)
      echo -n "SMCP [${res_name}] in namespace [${res_namespace}]: "
      if [ "$(${OC} get smcp ${res_name} -n ${res_namespace} -o jsonpath='{.spec.addons.kiali.enabled}')" == "true" ]; then
        echo "Kiali is enabled"
      else
        echo "Kiali is NOT enabled"
      fi
    done
  else
    echo "There are no SMCPs in the cluster"
  fi
}
