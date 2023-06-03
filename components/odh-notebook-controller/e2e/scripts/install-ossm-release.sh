#!/bin/bash

##########################################################
#
# This installs the latest public release of Service Mesh an OpenShift cluster.
#
# The operators are installed through OLM.
#
##########################################################

set -u

# Change to the directory where this script is
SCRIPT_ROOT="$( cd "$(dirname "$0")" ; pwd -P )"
cd ${SCRIPT_ROOT}

# get function definitions
source ${SCRIPT_ROOT}/func-sm.sh
source ${SCRIPT_ROOT}/func-kiali.sh

DEFAULT_CONTROL_PLANE_NAMESPACE="istio-system"
DEFAULT_ENABLE_KIALI="false"
DEFAULT_OC="oc"
DEFAULT_SMCP_VERSION="v2.3"

_CMD=""
while [[ $# -gt 0 ]]; do
  key="$1"
  case $key in

    # COMMANDS

    install-operators) _CMD="install-operators" ; shift ;;
    install-smcp)      _CMD="install-smcp"      ; shift ;;
    delete-operators)  _CMD="delete-operators"  ; shift ;;
    delete-smcp)       _CMD="delete-smcp"  ; shift ;;
    status)            _CMD="status"            ; shift ;;

    # OPTIONS

    -c|--client)                    OC="${2}"                      ; shift;shift ;;
    -cpn|--control-plane-namespace) CONTROL_PLANE_NAMESPACE="${2}" ; shift;shift ;;
    -ek|--enable-kiali)             ENABLE_KIALI="${2}"            ; shift;shift ;;
    -smcpv|--smcp-version)          SMCP_VERSION="${2}"            ; shift;shift ;;

    # HELP

    -h|--help)
      cat <<HELPMSG

$0 [option...] command

Installs the latest release of Service Mesh and Kiali from the public Red Hat catalog.
Can also install an SMCP in order to create a control plane.

Valid options:

  -c|--client <path to 'oc' client>
      A filename or path to the 'oc' client.
      Default: ${DEFAULT_OC}

  -cpn|--control-plane-namespace <name>
      The name of the control plane namespace if the SMCP is to be installed.
      Default: ${DEFAULT_CONTROL_PLANE_NAMESPACE}

  -ek|--enable-kiali <true|false>
      If true, and you elect to install-operators, the Kiali operator is installed
      with the rest of the Service Mesh operators.
      If true, and you elect to install-smcp, the SMCP will be configured to tell
      Service Mesh to create and manage its own Kiali CR.
      This is ignored when deleting operators (i.e. regardless of this setting, all
      operators are deleted, Kiali operator included).
      Default: ${DEFAULT_ENABLE_KIALI}

  -smcpv|--smcp-version
      The version of the SMCP that will be created.
      This defines the version of the control plane that will be installed.
      This is only used with the "install-smcp" command.
      Default: ${DEFAULT_SMCP_VERSION}
The command must be one of:

  * install-operators: Install the Service Mesh operators and (if --enable-kiali is "true") the Kiali operator.
  * install-smcp: Install the SMCP (you must first have installed the operators)
  * delete-operators: Delete the Service Mesh and Kiali operators (you must first delete all SMCPs and Kiali CRs manually).
  * delete-smcp: Delete the Service Mesh custom resources.
  * status: Provides details about resources that have been installed.

HELPMSG
      exit 1
      ;;
    *)
      echo "ERROR: Unknown argument [$key]. Aborting."
      exit 1
      ;;
  esac
done

# Setup environment

CONTROL_PLANE_NAMESPACE="${CONTROL_PLANE_NAMESPACE:-${DEFAULT_CONTROL_PLANE_NAMESPACE}}"
ENABLE_KIALI="${ENABLE_KIALI:-${DEFAULT_ENABLE_KIALI}}"
OC="${OC:-${DEFAULT_OC}}"
SMCP_VERSION="${SMCP_VERSION:-${DEFAULT_SMCP_VERSION}}"

echo CONTROL_PLANE_NAMESPACE=$CONTROL_PLANE_NAMESPACE
echo ENABLE_KIALI=$ENABLE_KIALI
echo OC=${OC}
echo SMCP_VERSION=${SMCP_VERSION}

# Make sure we are logged in
if ! which ${OC} >& /dev/null; then
  echo "ERROR: The client is not valid [${OC}]. Use --client to specify a valid path to 'oc'."
  exit 1
fi
if ! ${OC} whoami >& /dev/null; then
  echo "ERROR: You are not logged into the OpenShift cluster. Use '${OC} login' to log into a cluster and then retry."
  exit 1
fi

# Process the command
if [ "${_CMD}" == "install-operators" ]; then

  if [ "${ENABLE_KIALI}" == "true" ]; then
    install_kiali_operator "redhat-operators"
  fi
  install_servicemesh_operators "redhat-operators"

elif [ "${_CMD}" == "install-smcp" ]; then

  if [ "${ENABLE_KIALI}" == "true" ] && ! ${OC} get crd kialis.kiali.io >& /dev/null; then
    echo "Cannot install the SMCP with Kiali enabled because Kiali Operator is not installed."
    exit 1
  fi

  install_smcp "${CONTROL_PLANE_NAMESPACE}" "${ENABLE_KIALI}" "${SMCP_VERSION}" ""

elif [ "${_CMD}" == "delete-operators" ]; then

  delete_servicemesh_operators
  delete_kiali_operator

elif [ "${_CMD}" == "delete-smcp" ]; then

  delete_smcp

elif [ "${_CMD}" == "status" ]; then

  status_servicemesh_operators
  status_kiali_operator
  status_smcp
  status_kiali_cr

else
  echo "ERROR: Missing or unknown command. See --help for usage."
  exit 1
fi
