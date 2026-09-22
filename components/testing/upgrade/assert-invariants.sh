#!/usr/bin/env bash

set -Eeuo pipefail

ARTIFACTS_DIR="${ARTIFACTS_DIR:-$(pwd)/artifacts}"
WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE:-upgrade-notebooks}"
RUNNING_NOTEBOOK_NAME="${RUNNING_NOTEBOOK_NAME:-upgrade-running}"
AUTH_NOTEBOOK_NAME="${AUTH_NOTEBOOK_NAME:-upgrade-auth-running}"
STOPPED_NOTEBOOK_NAME="${STOPPED_NOTEBOOK_NAME:-upgrade-stopped}"
STRICT_LOGS="${STRICT_LOGS:-false}"
CONTROLLER_NAMESPACE="${CONTROLLER_NAMESPACE:-opendatahub}"

fail() {
  echo "Invariant failed: $1" >&2
  return 1
}

read_pod_line() {
  local file="$1"
  local pod_name="$2"
  grep -E "^${pod_name}[[:space:]]" "${file}" | awk 'NR==1 {print $0}'
}

sum_restart_counts() {
  local restart_blob="$1"
  # restart blob format example: "containerA:0;containerB:1;"
  awk -F'[:;]' '{
    total=0
    for (i=2; i<=NF; i+=2) {
      if ($i ~ /^[0-9]+$/) {
        total += $i
      }
    }
    print total
  }' <<< "${restart_blob}"
}

assert_pod_not_restarted() {
  local pod_name="$1"
  local before_file="${ARTIFACTS_DIR}/before/pod-invariants.tsv"
  local after_file="${ARTIFACTS_DIR}/after/pod-invariants.tsv"

  local before_line after_line before_uid before_start after_uid after_start before_restarts after_restarts
  before_line="$(read_pod_line "${before_file}" "${pod_name}")"
  after_line="$(read_pod_line "${after_file}" "${pod_name}")"

  [[ -n "${before_line}" ]] || fail "pre-upgrade pod line missing for ${pod_name}"
  [[ -n "${after_line}" ]] || fail "post-upgrade pod line missing for ${pod_name}"

  before_uid="$(awk '{print $2}' <<< "${before_line}")"
  before_start="$(awk '{print $3}' <<< "${before_line}")"
  before_restarts="$(sum_restart_counts "$(awk '{print $4}' <<< "${before_line}")")"
  after_uid="$(awk '{print $2}' <<< "${after_line}")"
  after_start="$(awk '{print $3}' <<< "${after_line}")"
  after_restarts="$(sum_restart_counts "$(awk '{print $4}' <<< "${after_line}")")"

  [[ "${before_uid}" != "MISSING" ]] || fail "pre-upgrade pod missing for ${pod_name}"
  [[ "${after_uid}" != "MISSING" ]] || fail "post-upgrade pod missing for ${pod_name}"
  [[ "${before_uid}" == "${after_uid}" ]] || fail "pod UID changed for ${pod_name} (${before_uid} -> ${after_uid})"
  [[ "${before_start}" == "${after_start}" ]] || fail "pod startTime changed for ${pod_name} (${before_start} -> ${after_start})"
  (( after_restarts <= before_restarts )) || fail "pod restart count increased for ${pod_name} (${before_restarts} -> ${after_restarts})"
}

assert_stopped_notebook_stayed_stopped() {
  local stopped_before
  local stopped_after
  stopped_before="$(cat "${ARTIFACTS_DIR}/before/stopped-annotation.txt" 2>/dev/null || true)"
  stopped_after="$(cat "${ARTIFACTS_DIR}/after/stopped-annotation.txt" 2>/dev/null || true)"
  [[ -n "${stopped_before}" ]] || fail "stopped notebook annotation missing before upgrade"
  [[ -n "${stopped_after}" ]] || fail "stopped notebook annotation missing after upgrade"

  if kubectl -n "${WORKLOAD_NAMESPACE}" get pod "${STOPPED_NOTEBOOK_NAME}-0" >/dev/null 2>&1; then
    fail "stopped notebook pod ${STOPPED_NOTEBOOK_NAME}-0 exists after upgrade"
  fi
}

assert_resources_present_for_auth_notebook() {
  kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${AUTH_NOTEBOOK_NAME}" >/dev/null
  kubectl -n "${WORKLOAD_NAMESPACE}" get service "${AUTH_NOTEBOOK_NAME}" >/dev/null
  kubectl -n "${WORKLOAD_NAMESPACE}" get networkpolicy "${AUTH_NOTEBOOK_NAME}-ctrl-np" >/dev/null
  # Prefer current inject-auth NetworkPolicy name; fall back to historical -oauth-np.
  if ! kubectl -n "${WORKLOAD_NAMESPACE}" get networkpolicy "${AUTH_NOTEBOOK_NAME}-kube-rbac-proxy-np" >/dev/null 2>&1; then
    kubectl -n "${WORKLOAD_NAMESPACE}" get networkpolicy "${AUTH_NOTEBOOK_NAME}-oauth-np" >/dev/null
  fi
  # ODH controller creates HTTPRoutes in its own (central) namespace, not user namespace.
  local route_count
  route_count="$(kubectl -n "${CONTROLLER_NAMESPACE}" get httproutes.gateway.networking.k8s.io \
    -l "notebook-name=${AUTH_NOTEBOOK_NAME},notebook-namespace=${WORKLOAD_NAMESPACE}" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | awk '{print NF}')"
  route_count="${route_count:-0}"
  [[ "${route_count}" -ge 1 ]] || fail "expected at least one HTTPRoute for ${AUTH_NOTEBOOK_NAME} in ${CONTROLLER_NAMESPACE}"
}

assert_controllers_healthy() {
  kubectl -n opendatahub rollout status deployment/odh-notebook-controller-manager --timeout=120s >/dev/null
  if kubectl -n kubeflow get deployment/notebook-controller-deployment >/dev/null 2>&1; then
    kubectl -n kubeflow rollout status deployment/notebook-controller-deployment --timeout=120s >/dev/null
  else
    kubectl -n opendatahub rollout status deployment/deployment --timeout=120s >/dev/null
  fi
}

assert_logs() {
  local odh_log="${ARTIFACTS_DIR}/after/odh-controller.log"
  local kf_log="${ARTIFACTS_DIR}/after/kf-controller.log"
  local pattern='(panic|fatal|segmentation fault|admission webhook.*error|reconcile.*error)'

  if [[ "${STRICT_LOGS}" == "true" ]]; then
    if [[ -f "${odh_log}" ]] && grep -Eqi "${pattern}" "${odh_log}"; then
      fail "strict log check matched failure pattern in ODH controller log"
    fi
    if [[ -f "${kf_log}" ]] && grep -Eqi "${pattern}" "${kf_log}"; then
      fail "strict log check matched failure pattern in KF controller log"
    fi
  fi
}

assert_upgrade_invariants() {
  assert_pod_not_restarted "${RUNNING_NOTEBOOK_NAME}-0"
  assert_pod_not_restarted "${AUTH_NOTEBOOK_NAME}-0"
  assert_stopped_notebook_stayed_stopped
  assert_resources_present_for_auth_notebook
  assert_controllers_healthy
  assert_logs
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  assert_upgrade_invariants
fi
