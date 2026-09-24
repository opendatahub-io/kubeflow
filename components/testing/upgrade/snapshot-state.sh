#!/usr/bin/env bash

set -Eeuo pipefail

WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE:-upgrade-notebooks}"
SNAPSHOT_NAME="${SNAPSHOT_NAME:-snapshot}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-$(pwd)/artifacts}"
NOTEBOOKS_CSV="${NOTEBOOKS_CSV:-upgrade-running,upgrade-auth-running,upgrade-stopped}"
STOPPED_NOTEBOOK_NAME="${STOPPED_NOTEBOOK_NAME:-upgrade-stopped}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-opendatahub}"

snapshot_state() {
  local snapshot_dir="${ARTIFACTS_DIR}/${SNAPSHOT_NAME}"
  mkdir -p "${snapshot_dir}"

  kubectl -n "${WORKLOAD_NAMESPACE}" get notebooks -o json > "${snapshot_dir}/notebooks.json"
  kubectl -n "${WORKLOAD_NAMESPACE}" get statefulsets -o json > "${snapshot_dir}/statefulsets.json"
  kubectl -n "${WORKLOAD_NAMESPACE}" get pods -o json > "${snapshot_dir}/pods.json"
  kubectl -n "${WORKLOAD_NAMESPACE}" get networkpolicies -o json > "${snapshot_dir}/networkpolicies.json"
  kubectl -n "${CONTROLLER_NAMESPACE}" get httproutes.gateway.networking.k8s.io -o json > "${snapshot_dir}/httproutes.json" || true
  kubectl -n opendatahub get deploy -o json > "${snapshot_dir}/opendatahub-deployments.json"
  kubectl -n opendatahub logs -l app=odh-notebook-controller --tail=-1 > "${snapshot_dir}/odh-controller.log" || true
  if kubectl get namespace kubeflow >/dev/null 2>&1; then
    kubectl logs -n kubeflow -l app=notebook-controller --tail=-1 > "${snapshot_dir}/kf-controller.log" || true
  else
    kubectl logs -n opendatahub -l app=notebook-controller --tail=-1 > "${snapshot_dir}/kf-controller.log" || true
  fi
  kubectl get events -A --sort-by=.metadata.creationTimestamp > "${snapshot_dir}/events.txt" || true

  local pod_snapshot="${snapshot_dir}/pod-invariants.tsv"
  : > "${pod_snapshot}"

  IFS=',' read -r -a notebooks <<< "${NOTEBOOKS_CSV}"
  for nb in "${notebooks[@]}"; do
    local pod_name="${nb}-0"
    local line
    line="$(kubectl -n "${WORKLOAD_NAMESPACE}" get pod "${pod_name}" -o jsonpath="{.metadata.name}{'\t'}{.metadata.uid}{'\t'}{.status.startTime}{'\t'}{range .status.containerStatuses[*]}{.name}:{.restartCount};{end}" 2>/dev/null || true)"
    if [[ -z "${line}" ]]; then
      printf "%s\t%s\t%s\t%s\n" "${pod_name}" "MISSING" "MISSING" "MISSING" >> "${pod_snapshot}"
    else
      printf "%s\n" "${line}" >> "${pod_snapshot}"
    fi
  done

  kubectl -n "${WORKLOAD_NAMESPACE}" get notebook "${STOPPED_NOTEBOOK_NAME}" -o jsonpath='{.metadata.annotations.kubeflow-resource-stopped}' > "${snapshot_dir}/stopped-annotation.txt" || true
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  snapshot_state
fi
