#!/usr/bin/env bash

set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-odh-upgrade}"
# Default to single-node: multi-node kind+rootless Podman often fails at worker join.
KIND_CONFIG="${KIND_CONFIG:-${SCRIPT_DIR}/kind-1-32-single.yaml}"
# Empty until configure_kind_provider selects docker or podman (honor an explicit override).
CONTAINER_ENGINE="${CONTAINER_ENGINE:-}"

_set_container_engine_if_unset() {
  local engine="$1"
  if [[ -z "${CONTAINER_ENGINE}" ]]; then
    CONTAINER_ENGINE="${engine}"
  fi
}

# KinD defaults to Docker. Local Fedora/RHEL setups (and this repo's CI image builds)
# typically use Podman with no docker.sock — select the provider once for all kind calls.
configure_kind_provider() {
  if [[ -n "${KIND_EXPERIMENTAL_PROVIDER:-}" ]]; then
    export KIND_EXPERIMENTAL_PROVIDER
    _set_container_engine_if_unset "${KIND_EXPERIMENTAL_PROVIDER}"
    echo "[kind] Using provider from KIND_EXPERIMENTAL_PROVIDER=${KIND_EXPERIMENTAL_PROVIDER} (engine=${CONTAINER_ENGINE})"
    return 0
  fi

  if docker info >/dev/null 2>&1; then
    _set_container_engine_if_unset "docker"
    echo "[kind] Using docker provider (engine=${CONTAINER_ENGINE})"
    return 0
  fi

  if ! command -v podman >/dev/null 2>&1; then
    cat >&2 <<'EOF'
[kind] Neither a working Docker daemon nor Podman is available.
Start Docker, or install Podman and re-run with:
  export KIND_EXPERIMENTAL_PROVIDER=podman
EOF
    exit 1
  fi

  if ! podman info >/dev/null 2>&1; then
    cat >&2 <<'EOF'
[kind] Podman is installed but not usable (daemon/socket down?).
Try:
  systemctl --user enable --now podman.socket
  # or for rootful: sudo systemctl enable --now podman.socket
Then re-run with:
  export KIND_EXPERIMENTAL_PROVIDER=podman
EOF
    exit 1
  fi

  export KIND_EXPERIMENTAL_PROVIDER=podman
  _set_container_engine_if_unset "podman"
  echo "[kind] Docker unavailable; using KIND_EXPERIMENTAL_PROVIDER=podman (engine=${CONTAINER_ENGINE})"
}

require_kind_dependencies() {
  if [[ -z "${CONTAINER_ENGINE}" ]]; then
    echo "CONTAINER_ENGINE is unset; call configure_kind_provider first" >&2
    exit 1
  fi
  local deps=("kind" "kubectl" "kustomize" "openssl" "${CONTAINER_ENGINE}")
  for dep in "${deps[@]}"; do
    if ! command -v "${dep}" >/dev/null 2>&1; then
      echo "Missing required dependency: ${dep}" >&2
      exit 1
    fi
  done
}

kind_cluster_exists() {
  configure_kind_provider
  kind get clusters 2>/dev/null | grep -Fx "${KIND_CLUSTER_NAME}" >/dev/null 2>&1
}

ensure_kind_cluster() {
  configure_kind_provider
  require_kind_dependencies

  if kind_cluster_exists; then
    echo "[kind] Reusing existing cluster '${KIND_CLUSTER_NAME}'"
  else
    echo "[kind] Creating cluster '${KIND_CLUSTER_NAME}'"
    kind create cluster --name "${KIND_CLUSTER_NAME}" --config "${KIND_CONFIG}"
  fi
}

load_image_into_kind_if_local() {
  local image_ref="$1"
  if [[ -z "${image_ref}" ]]; then
    return 0
  fi

  # kind can pull remote images directly; local ones must be loaded.
  if [[ "${image_ref}" != localhost/* ]]; then
    return 0
  fi

  local tmp_tar
  tmp_tar="$(mktemp "${TMPDIR:-/tmp}/kind-image-XXXXXX.tar")"
  echo "[kind] Loading local image '${image_ref}'"
  configure_kind_provider
  "${CONTAINER_ENGINE}" save -o "${tmp_tar}" "${image_ref}"
  kind load image-archive --name "${KIND_CLUSTER_NAME}" "${tmp_tar}"
  rm -f "${tmp_tar}"
}

istio_already_healthy() {
  kubectl get ns istio-system >/dev/null 2>&1 || return 1
  kubectl -n istio-system get deploy istiod >/dev/null 2>&1 || return 1
  kubectl -n istio-system rollout status deploy/istiod --timeout=30s >/dev/null 2>&1
}

ensure_istio() {
  if [[ "${FORCE_ISTIO_INSTALL:-false}" == "true" ]]; then
    echo "[kind] FORCE_ISTIO_INSTALL=true; (re)installing Istio"
    "${REPO_ROOT}/components/testing/gh-actions/install_istio.sh"
    return 0
  fi

  if istio_already_healthy; then
    echo "[kind] Istio already present (istiod ready); skipping install"
    return 0
  fi

  echo "[kind] Installing Istio"
  "${REPO_ROOT}/components/testing/gh-actions/install_istio.sh"
}

install_kind_prereqs() {
  ensure_istio

  # ServiceMonitor is included in the odh overlay; install the CRD for KinD.
  echo "[kind] Installing ServiceMonitor CRD"
  kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.73.2/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml

  echo "[kind] Installing fake OpenShift CRDs"
  kubectl apply -f - <<'EOF'
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  annotations:
    crd/fake: "true"
  name: imagestreams.image.openshift.io
spec:
  group: image.openshift.io
  names:
    kind: ImageStream
    listKind: ImageStreamList
    singular: imagestream
    plural: imagestreams
  scope: Namespaced
  versions:
    - name: v1
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
      served: true
      storage: true
EOF

  echo "[kind] Installing Gateway API CRDs"
  kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml"
}
