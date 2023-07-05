#!/bin/bash

##########################################################
#
# Functions for managing Kiali installs.
#
##########################################################

set -u

install_kiali_operator() {
  local kiali_subscription_source="${1}"

  echo "Installing the Kiali Operator"
  cat <<EOM | ${OC} apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kiali-ossm
  namespace: openshift-operators
spec:
  channel: stable
  installPlanApproval: Automatic
  name: kiali-ossm
  source: ${kiali_subscription_source}
  sourceNamespace: openshift-marketplace
EOM
}

install_kiali_cr() {
  local control_plane_namespace="${1}"
  echo -n "Installing the minimal Kiali CR after CRD has been established..."
  while ! ${OC} get crd kialis.kiali.io >& /dev/null ; do echo -n '.'; sleep 1; done
  ${OC} wait --for condition=established crd/kialis.kiali.io

  if ! ${OC} get namespace ${control_plane_namespace} >& /dev/null; then
    echo "Creating control plane namespace: ${control_plane_namespace}"
    ${OC} create namespace ${control_plane_namespace}
  fi

  ${OC} apply -n ${control_plane_namespace} -f https://raw.githubusercontent.com/kiali/kiali-operator/master/deploy/kiali/kiali_cr_minimal.yaml
}

delete_kiali_operator() {
  local abort_operation="false"
  for cr in \
    $(${OC} get kiali --all-namespaces -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' )
  do
    abort_operation="true"
    local res_namespace=$(echo ${cr} | cut -d: -f1)
    local res_name=$(echo ${cr} | cut -d: -f2)
    echo "A Kiali CR named [${res_name}] in namespace [${res_namespace}] still exists."
  done
  if [ "${abort_operation}" == "true" ]; then
    echo "Aborting"
    exit 1
  fi

  echo "Unsubscribing from the Kiali Operator"
  ${OC} delete subscription --namespace openshift-operators kiali-ossm

  echo "Deleting OLM CSVs which uninstalled the Kiali Operator and its related resources"
  for csv in $(${OC} get csv --all-namespaces --no-headers -o custom-columns=NS:.metadata.namespace,N:.metadata.name | sed 's/  */:/g' | grep kiali-operator)
  do
    ${OC} delete csv -n $(echo -n $csv | cut -d: -f1) $(echo -n $csv | cut -d: -f2)
  done

  echo "Delete Kiali CRDs"
  ${OC} get crds -o name | grep '.*\.kiali\.io' | xargs -r -n 1 ${OC} delete
}

delete_kiali_cr() {
  echo "Deleting all Kiali CRs in the cluster"
  for cr in $(${OC} get kiali --all-namespaces -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers | sed 's/  */:/g' )
  do
    local res_namespace=$(echo ${cr} | cut -d: -f1)
    local res_name=$(echo ${cr} | cut -d: -f2)
    ${OC} delete -n ${res_namespace} kiali ${res_name}
  done
}

status_kiali_operator() {
  echo
  echo "===== KIALI OPERATOR SUBSCRIPTION"
  if ${OC} get subscription --namespace openshift-operators kiali-ossm >& /dev/null ; then
    echo "A Subscription exists for the Kiali Operator"
    ${OC} get subscription --namespace openshift-operators kiali-ossm
    echo
    echo "===== KIALI OPERATOR POD"
    local op_name="$(${OC} get pod --namespace openshift-operators -o name | grep kiali)"
    [ ! -z "${op_name}" ] && ${OC} get --namespace openshift-operators ${op_name} || echo "There is no pod"
  else
    echo "There is no Subscription for the Kiali Operator"
  fi
}

status_kiali_cr() {
  echo
  echo "===== KIALI CRs"
  if [ "$(${OC} get kiali --all-namespaces 2> /dev/null | wc -l)" -gt "0" ] ; then
    echo "One or more Kiali CRs exist in the cluster"
    ${OC} get kiali --all-namespaces
  else
    echo "There are no Kiali CRs in the cluster"
  fi
}
