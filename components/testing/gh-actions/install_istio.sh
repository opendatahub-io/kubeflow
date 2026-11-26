#!/bin/bash

set -euo pipefail

ISTIO_VERSION="1.27.1"
ISTIO_URL="https://istio.io/downloadIstio"

echo "Installing Istio ${ISTIO_VERSION} ..."
# Prefer an ephemeral dir so local runs do not leave istio_tmp/ in the repo.
ISTIO_TMP="$(mktemp -d "${TMPDIR:-/tmp}/istio-install-XXXXXX")"
cleanup() {
  rm -rf "${ISTIO_TMP}"
}
trap cleanup EXIT

pushd "${ISTIO_TMP}" >/dev/null
  curl -sL "$ISTIO_URL" | ISTIO_VERSION=${ISTIO_VERSION} sh -
  cd "istio-${ISTIO_VERSION}"
  export PATH=$PWD/bin:$PATH
  istioctl install -y
popd >/dev/null
