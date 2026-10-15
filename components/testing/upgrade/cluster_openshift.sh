#!/usr/bin/env bash

set -Eeuo pipefail

require_openshift_dependencies() {
  local deps=("oc" "kubectl" "kustomize")
  for dep in "${deps[@]}"; do
    if ! command -v "${dep}" >/dev/null 2>&1; then
      echo "Missing required dependency: ${dep}" >&2
      exit 1
    fi
  done
}

ensure_logged_in_openshift() {
  require_openshift_dependencies

  if ! oc whoami >/dev/null 2>&1; then
    echo "OpenShift mode requested, but 'oc whoami' failed. Please login first." >&2
    exit 1
  fi
}

ensure_namespace() {
  local namespace="$1"
  if ! kubectl get namespace "${namespace}" >/dev/null 2>&1; then
    kubectl create namespace "${namespace}"
  fi
}
