#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# Keep in sync with overlays/odh/params.env (KinD cannot pull registry.redhat.io).
DEFAULT_KUBE_RBAC_PROXY_IMAGE="${DEFAULT_KUBE_RBAC_PROXY_IMAGE:-quay.io/opendatahub/odh-kube-auth-proxy@sha256:dcb09fbabd8811f0956ef612a0c9ddd5236804b9bd6548a0647d2b531c9d01b3}"

create_culler_configmap() {
  local namespace="$1"
  kubectl apply -f - <<EOF
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: notebook-controller-culler-config
  namespace: ${namespace}
data:
  ENABLE_CULLING: "true"
  CULL_IDLE_TIME: "60"
  IDLENESS_CHECK_PERIOD: "5"
EOF
}

create_webhook_cert_secrets() {
  local namespace="$1"
  local cert_dir="$2"
  mkdir -p "${cert_dir}"

  # SANs cover webhook service and HTTPS metrics service (odh overlay).
  openssl req -nodes -x509 -newkey rsa:4096 -sha256 -days 3650 \
    -keyout "${cert_dir}/ca-key.pem" \
    -out "${cert_dir}/ca-cert.pem" \
    -subj "/CN=UpgradeTestCA" >/dev/null 2>&1
  openssl req -nodes -newkey rsa:4096 \
    -keyout "${cert_dir}/server-key.pem" \
    -out "${cert_dir}/server-csr.pem" \
    -subj "/CN=UpgradeTestServer" \
    -addext "subjectAltName = DNS:odh-notebook-controller-webhook-service.${namespace}.svc,DNS:odh-notebook-controller-webhook-service.${namespace}.svc.cluster.local,DNS:odh-notebook-controller-service.${namespace}.svc,DNS:odh-notebook-controller-service.${namespace}.svc.cluster.local" >/dev/null 2>&1
  openssl x509 -req \
    -in "${cert_dir}/server-csr.pem" \
    -CA "${cert_dir}/ca-cert.pem" \
    -CAkey "${cert_dir}/ca-key.pem" \
    -out "${cert_dir}/server-cert.pem" \
    -days 365 \
    -copy_extensions copyall \
    -extfile <(printf 'subjectAltName=DNS:odh-notebook-controller-webhook-service.%s.svc,DNS:odh-notebook-controller-webhook-service.%s.svc.cluster.local,DNS:odh-notebook-controller-service.%s.svc,DNS:odh-notebook-controller-service.%s.svc.cluster.local' \
      "${namespace}" "${namespace}" "${namespace}" "${namespace}") >/dev/null 2>&1

  kubectl -n "${namespace}" delete secret odh-notebook-controller-webhook-cert --ignore-not-found=true
  kubectl -n "${namespace}" create secret tls odh-notebook-controller-webhook-cert \
    --cert="${cert_dir}/server-cert.pem" \
    --key="${cert_dir}/server-key.pem"

  kubectl -n "${namespace}" delete secret odh-notebook-controller-metrics-tls --ignore-not-found=true
  kubectl -n "${namespace}" create secret tls odh-notebook-controller-metrics-tls \
    --cert="${cert_dir}/server-cert.pem" \
    --key="${cert_dir}/server-key.pem"

  # Prefer restrictive perms on local private keys (often under /tmp, never uploaded).
  chmod 600 "${cert_dir}/ca-key.pem" "${cert_dir}/server-key.pem" 2>/dev/null || true
}

patch_odh_webhook_cabundle() {
  local cert_dir="$1"
  local ca_bundle
  # Match integration CI encoding (portable base64, no line wraps).
  ca_bundle="$(base64 < "${cert_dir}/ca-cert.pem" | tr -d '\n')"

  # ODH controller uses both mutating and validating webhooks. Patch both
  # CA bundles so kube-apiserver can trust the generated serving certificate.
  kubectl patch mutatingwebhookconfiguration odh-notebook-controller-mutating-webhook-configuration \
    --type=json \
    -p "[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"${ca_bundle}\"}]"
  kubectl patch validatingwebhookconfiguration odh-notebook-controller-validating-webhook-configuration \
    --type=json \
    -p "[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"${ca_bundle}\"}]"
}

restart_odh_manager_for_webhook_certs() {
  # Secret rotation alone does not reload the webhook server or align serving certs
  # with a freshly patched caBundle on an existing cluster.
  kubectl -n opendatahub rollout restart deployment/odh-notebook-controller-manager
}

render_and_apply_kf_kind() {
  local image="$1"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/kf-kind-config-XXXXXX")"
  cp -R "${REPO_ROOT}/components/notebook-controller/config" "${tmp}/config"
  (
    cd "${tmp}/config/overlays/kubeflow"
    kustomize edit set image "docker.io/kubeflownotebookswg/notebook-controller=${image}"
    kustomize build . | sed "s/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g" | kubectl apply -f -
  )
  rm -rf "${tmp}"
}

render_and_apply_kf_openshift() {
  local image="$1"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/kf-ocp-config-XXXXXX")"
  cp -R "${REPO_ROOT}/components/notebook-controller/config" "${tmp}/config"
  sed -i "s|^odh-kf-notebook-controller-image=.*|odh-kf-notebook-controller-image=${image}|" \
    "${tmp}/config/overlays/openshift/params.env"
  (
    cd "${tmp}/config/overlays/openshift"
    kustomize build . | sed "s/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g" | kubectl apply -f -
  )
  rm -rf "${tmp}"
}

render_and_apply_odh() {
  local image="$1"
  local tmp
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/odh-config-XXXXXX")"
  cp -R "${REPO_ROOT}/components/odh-notebook-controller/config" "${tmp}/config"
  # params.env lives in overlays/odh|rhoai (not config/base) after overlay consolidation.
  sed -i "s|^odh-notebook-controller-image=.*|odh-notebook-controller-image=${image}|" \
    "${tmp}/config/overlays/odh/params.env"
  sed -i "s|^kube-rbac-proxy=.*|kube-rbac-proxy=${DEFAULT_KUBE_RBAC_PROXY_IMAGE}|" \
    "${tmp}/config/overlays/odh/params.env"
  (
    cd "${tmp}/config/overlays/odh"
    kustomize build . | \
      sed "s/imagePullPolicy: Always/imagePullPolicy: IfNotPresent/g" | \
      sed "s|registry.redhat.io/openshift4/ose-kube-rbac-proxy.*|${DEFAULT_KUBE_RBAC_PROXY_IMAGE}|g" | \
      kubectl apply -f -
  )
  rm -rf "${tmp}"
}

wait_for_controllers() {
  local mode="$1"
  if [[ "${mode}" == "kind" ]]; then
    kubectl -n kubeflow rollout status deployment/notebook-controller-deployment --timeout=180s
  else
    kubectl -n opendatahub rollout status deployment/deployment --timeout=180s
  fi
  kubectl -n opendatahub rollout status deployment/odh-notebook-controller-manager --timeout=180s
}

deploy_controllers_with_images() {
  local mode="$1"
  local kf_image="$2"
  local odh_image="$3"
  local cert_dir="$4"

  kubectl get namespace opendatahub >/dev/null 2>&1 || kubectl create namespace opendatahub
  create_culler_configmap "opendatahub"

  if [[ "${mode}" == "kind" ]]; then
    kubectl get namespace kubeflow >/dev/null 2>&1 || kubectl create namespace kubeflow
    create_culler_configmap "kubeflow"
    render_and_apply_kf_kind "${kf_image}"
  else
    render_and_apply_kf_openshift "${kf_image}"
  fi

  create_webhook_cert_secrets "opendatahub" "${cert_dir}"
  render_and_apply_odh "${odh_image}"
  patch_odh_webhook_cabundle "${cert_dir}"
  restart_odh_manager_for_webhook_certs
  wait_for_controllers "${mode}"
}
