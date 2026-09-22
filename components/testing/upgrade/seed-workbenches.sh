#!/usr/bin/env bash

set -Eeuo pipefail

WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE:-upgrade-notebooks}"
AUTH_NOTEBOOK_NAME="${AUTH_NOTEBOOK_NAME:-upgrade-auth-running}"
RUNNING_NOTEBOOK_NAME="${RUNNING_NOTEBOOK_NAME:-upgrade-running}"
STOPPED_NOTEBOOK_NAME="${STOPPED_NOTEBOOK_NAME:-upgrade-stopped}"
NOTEBOOK_IMAGE="${NOTEBOOK_IMAGE:-quay.io/thoth-station/s2i-minimal-notebook:v0.3.0}"
MODE="${MODE:-kind}"

require_seed_dependencies() {
  local deps=("kubectl")
  for dep in "${deps[@]}"; do
    if ! command -v "${dep}" >/dev/null 2>&1; then
      echo "Missing required dependency: ${dep}" >&2
      exit 1
    fi
  done
}

ensure_workload_namespace() {
  if ! kubectl get namespace "${WORKLOAD_NAMESPACE}" >/dev/null 2>&1; then
    kubectl create namespace "${WORKLOAD_NAMESPACE}"
  fi
}

create_auth_tls_secret_for_kind() {
  if [[ "${MODE}" != "kind" ]]; then
    return 0
  fi

  local cert_dir
  cert_dir="$(mktemp -d "${TMPDIR:-/tmp}/nb-auth-cert-XXXXXX")"
  openssl req -nodes -x509 -newkey rsa:4096 -sha256 -days 365 \
    -keyout "${cert_dir}/tls.key" \
    -out "${cert_dir}/tls.crt" \
    -subj "/CN=${AUTH_NOTEBOOK_NAME}" >/dev/null 2>&1

  kubectl -n "${WORKLOAD_NAMESPACE}" delete secret "${AUTH_NOTEBOOK_NAME}-kube-rbac-proxy-tls" --ignore-not-found=true
  kubectl -n "${WORKLOAD_NAMESPACE}" create secret tls "${AUTH_NOTEBOOK_NAME}-kube-rbac-proxy-tls" \
    --cert="${cert_dir}/tls.crt" \
    --key="${cert_dir}/tls.key"

  rm -rf "${cert_dir}"
}

seed_workbenches() {
  require_seed_dependencies
  ensure_workload_namespace
  create_auth_tls_secret_for_kind

  kubectl apply -f - <<EOF
---
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: ${RUNNING_NOTEBOOK_NAME}
  namespace: ${WORKLOAD_NAMESPACE}
spec:
  template:
    spec:
      containers:
        - name: ${RUNNING_NOTEBOOK_NAME}
          image: ${NOTEBOOK_IMAGE}
          imagePullPolicy: IfNotPresent
          workingDir: /opt/app-root/src
          env:
            - name: NOTEBOOK_ARGS
              value: |
                --ServerApp.port=8888
                --ServerApp.token=''
                --ServerApp.password=''
                --ServerApp.base_url=/notebook/${WORKLOAD_NAMESPACE}/${RUNNING_NOTEBOOK_NAME}
          ports:
            - name: notebook-port
              containerPort: 8888
              protocol: TCP
          resources:
            requests:
              cpu: "100m"
              memory: "256Mi"
            limits:
              cpu: "500m"
              memory: "1Gi"
---
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: ${AUTH_NOTEBOOK_NAME}
  namespace: ${WORKLOAD_NAMESPACE}
  annotations:
    notebooks.opendatahub.io/inject-auth: "true"
spec:
  template:
    spec:
      containers:
        - name: ${AUTH_NOTEBOOK_NAME}
          image: ${NOTEBOOK_IMAGE}
          imagePullPolicy: IfNotPresent
          workingDir: /opt/app-root/src
          env:
            - name: NOTEBOOK_ARGS
              value: |
                --ServerApp.port=8888
                --ServerApp.token=''
                --ServerApp.password=''
                --ServerApp.base_url=/notebook/${WORKLOAD_NAMESPACE}/${AUTH_NOTEBOOK_NAME}
          ports:
            - name: notebook-port
              containerPort: 8888
              protocol: TCP
          resources:
            requests:
              cpu: "100m"
              memory: "256Mi"
            limits:
              cpu: "500m"
              memory: "1Gi"
---
apiVersion: kubeflow.org/v1
kind: Notebook
metadata:
  name: ${STOPPED_NOTEBOOK_NAME}
  namespace: ${WORKLOAD_NAMESPACE}
  annotations:
    kubeflow-resource-stopped: "true"
spec:
  template:
    spec:
      containers:
        - name: ${STOPPED_NOTEBOOK_NAME}
          image: ${NOTEBOOK_IMAGE}
          imagePullPolicy: IfNotPresent
          workingDir: /opt/app-root/src
          env:
            - name: NOTEBOOK_ARGS
              value: |
                --ServerApp.port=8888
                --ServerApp.token=''
                --ServerApp.password=''
                --ServerApp.base_url=/notebook/${WORKLOAD_NAMESPACE}/${STOPPED_NOTEBOOK_NAME}
          ports:
            - name: notebook-port
              containerPort: 8888
              protocol: TCP
          resources:
            requests:
              cpu: "100m"
              memory: "256Mi"
            limits:
              cpu: "500m"
              memory: "1Gi"
EOF

  # Force deterministic initial states after admission/controller churn:
  # - running notebooks: started (no stop annotation)
  # - stopped notebook: explicitly stopped
  ensure_notebook_started "${RUNNING_NOTEBOOK_NAME}" 60
  ensure_notebook_started "${AUTH_NOTEBOOK_NAME}" 60
  ensure_notebook_stopped "${STOPPED_NOTEBOOK_NAME}" 60

  wait_for_notebook_statefulset_ready "${RUNNING_NOTEBOOK_NAME}" 240
  wait_for_notebook_statefulset_ready "${AUTH_NOTEBOOK_NAME}" 240
  wait_for_notebook_statefulset_stopped "${STOPPED_NOTEBOOK_NAME}" 240
}

ensure_notebook_started() {
  local notebook_name="$1"
  local timeout_secs="${2:-60}"
  local elapsed=0

  while (( elapsed < timeout_secs )); do
    if kubectl -n "${WORKLOAD_NAMESPACE}" patch notebook "${notebook_name}" \
      --type=merge \
      -p '{"metadata":{"annotations":{"kubeflow-resource-stopped":null}}}' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done

  echo "Failed to ensure notebook/${notebook_name} is started" >&2
  return 1
}

ensure_notebook_stopped() {
  local notebook_name="$1"
  local timeout_secs="${2:-60}"
  local elapsed=0

  while (( elapsed < timeout_secs )); do
    if kubectl -n "${WORKLOAD_NAMESPACE}" patch notebook "${notebook_name}" \
      --type=merge \
      -p '{"metadata":{"annotations":{"kubeflow-resource-stopped":"true"}}}' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done

  echo "Failed to ensure notebook/${notebook_name} is stopped" >&2
  return 1
}

wait_for_notebook_statefulset_ready() {
  local notebook_name="$1"
  local timeout_secs="${2:-600}"
  local elapsed=0

  while (( elapsed < timeout_secs )); do
    # Ignore transient NotFound responses until the controller creates the StatefulSet.
    local spec_replicas
    local ready_replicas
    local stopped_annotation
    spec_replicas="$(kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${notebook_name}" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
    ready_replicas="$(kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${notebook_name}" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    stopped_annotation="$(kubectl -n "${WORKLOAD_NAMESPACE}" get notebook "${notebook_name}" -o jsonpath='{.metadata.annotations.kubeflow-resource-stopped}' 2>/dev/null || true)"

    # odh-notebook-controller may temporarily set this lock while coordinating
    # reconciliation with notebook-controller. Do not mutate it in tests; wait.
    if [[ "${stopped_annotation}" == "odh-notebook-controller-lock" ]]; then
      sleep 5
      elapsed=$((elapsed + 5))
      continue
    fi

    if [[ "${spec_replicas}" == "1" && "${ready_replicas}" == "1" ]]; then
      return 0
    fi

    sleep 5
    elapsed=$((elapsed + 5))
  done

  echo "Timed out waiting for statefulset/${notebook_name} to become ready" >&2
  echo "Last observed kubeflow-resource-stopped annotation value: ${stopped_annotation:-<empty>}" >&2
  kubectl -n "${WORKLOAD_NAMESPACE}" get notebook "${notebook_name}" -o yaml || true
  kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${notebook_name}" -o yaml || true
  kubectl -n "${WORKLOAD_NAMESPACE}" get pods -o wide || true
  return 1
}

wait_for_notebook_statefulset_stopped() {
  local notebook_name="$1"
  local timeout_secs="${2:-240}"
  local elapsed=0

  while (( elapsed < timeout_secs )); do
    local spec_replicas
    local stopped_annotation
    spec_replicas="$(kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${notebook_name}" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)"
    stopped_annotation="$(kubectl -n "${WORKLOAD_NAMESPACE}" get notebook "${notebook_name}" -o jsonpath='{.metadata.annotations.kubeflow-resource-stopped}' 2>/dev/null || true)"

    if [[ "${spec_replicas}" == "0" && -n "${stopped_annotation}" ]]; then
      if ! kubectl -n "${WORKLOAD_NAMESPACE}" get pod "${notebook_name}-0" >/dev/null 2>&1; then
        return 0
      fi
    fi

    sleep 5
    elapsed=$((elapsed + 5))
  done

  echo "Timed out waiting for statefulset/${notebook_name} to remain stopped" >&2
  kubectl -n "${WORKLOAD_NAMESPACE}" get notebook "${notebook_name}" -o yaml || true
  kubectl -n "${WORKLOAD_NAMESPACE}" get statefulset "${notebook_name}" -o yaml || true
  kubectl -n "${WORKLOAD_NAMESPACE}" get pods -o wide || true
  return 1
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  seed_workbenches
fi
