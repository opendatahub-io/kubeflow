#!/usr/bin/env bash
#
# Konflux build simulation for opendatahub-io/kubeflow.
#
# Simulates, at PR time, what the two Konflux pull-request PipelineRuns do to
# a change, plus the packaging contracts their images must satisfy:
#
#   .tekton/odh-notebook-controller-pull-request.yaml
#     -> image quay.io/opendatahub/odh-notebook-controller:odh-pr
#        dockerfile odh-notebook-controller/Dockerfile, context components
#   .tekton/odh-kf-notebook-controller-pull-request.yaml
#     -> image quay.io/opendatahub/kubeflow-notebook-controller:odh-pr
#        dockerfile notebook-controller/Dockerfile, context components
#
# Commands (run from the repository root):
#   validate-build.sh static             Dockerfile / PipelineRun / go-version contracts
#   validate-build.sh hermetic <target>  offline module + build proof for one Go module
#   validate-build.sh image <target>     build the Konflux image and smoke its entrypoint
#   validate-build.sh manifests           render overlays + kubeconform + manifest contracts
#   validate-build.sh flag-contract <target>  overlay flags vs binary --help
#   validate-build.sh kind                deploy the PR images into Kind, assert reconcile evidence
#
# Targets: odh (odh-notebook-controller), kf (notebook-controller).
#
# Environment overrides (all prefixed SIM_ so they cannot collide with the
# components' own Makefile variables):
#   SIM_CONTAINER_TOOL   docker | podman (default: first found on PATH)
#   SIM_KUSTOMIZE_BIN    kustomize binary (default: kustomize from PATH)
#   SIM_KUBECONFORM_BIN  kubeconform binary (default: downloaded, pinned)
#   SIM_KIND_BIN         kind binary (default: kind)
#   SIM_KUBECTL_BIN      kubectl binary (default: kubectl)
#   SIM_KIND_CLUSTER     Kind cluster name (default: kind-kubeflow-sim)
#   SIM_BUILD_DIR        scratch dir for renders and logs (default: mktemp -d)
#   SIM_GOOS / SIM_GOARCH  platform for the hermetic build proof
#                          (default: linux/amd64 on Linux, host platform elsewhere)
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

SIM_CONTAINER_TOOL="${SIM_CONTAINER_TOOL:-$(command -v docker >/dev/null 2>&1 && echo docker || echo podman)}"
SIM_KUSTOMIZE_BIN="${SIM_KUSTOMIZE_BIN:-$(command -v kustomize >/dev/null 2>&1 && echo kustomize || true)}"
SIM_KIND_BIN="${SIM_KIND_BIN:-kind}"
SIM_KUBECTL_BIN="${SIM_KUBECTL_BIN:-kubectl}"
SIM_KIND_CLUSTER="${SIM_KIND_CLUSTER:-kind-kubeflow-sim}"
if [ "$(uname -s)" = Linux ]; then
  SIM_GOOS="${SIM_GOOS:-linux}"
else
  SIM_GOOS="${SIM_GOOS:-$(go env GOOS)}"
fi
SIM_GOARCH="${SIM_GOARCH:-$(go env GOARCH)}"

# Pinned kubeconform used when SIM_KUBECONFORM_BIN is not provided.
KUBECONFORM_VERSION="v0.6.7"
KUBECONFORM_SHA256_LINUX_AMD64="95f14e87aa28c09d5941f11bd024c1d02fdc0303ccaa23f61cef67bc92619d73"
KUBECONFORM_SHA256_DARWIN_ARM64="cbb47d938a8d18eb5f79cb33663b2cecdee0c8ac0bf562ebcfca903df5f0802f"

# Gateway API CRD release matching sigs.k8s.io/gateway-api in the components'
# go.mod files. The odh controller watches HTTPRoute and ReferenceGrant, so
# its manager cannot start on a cluster without these CRDs.
GATEWAY_API_CRDS="https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml"

if [ -z "${SIM_BUILD_DIR:-}" ]; then
  SIM_BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/kubeflow-sim.XXXXXX")"
  cleanup_build_dir() { rm -rf "$SIM_BUILD_DIR"; }
  trap cleanup_build_dir EXIT
fi
mkdir -p "$SIM_BUILD_DIR"
echo "Simulator scratch dir: $SIM_BUILD_DIR" >&2

# ---------------------------------------------------------------------------
# Targets
# ---------------------------------------------------------------------------
target_info() {
  case "$1" in
    odh)
      TARGET=odh
      PIPELINERUN=.tekton/odh-notebook-controller-pull-request.yaml
      COMPONENT_DIR=components/odh-notebook-controller
      DOCKERFILE_REL=odh-notebook-controller/Dockerfile
      BUILD_CONTEXT=components
      IMAGE_NAME=odh-notebook-controller
      LOCAL_TAG="localhost/kubeflow-sim/odh-notebook-controller:pr"
      OVERLAYS=("config/overlays/odh" "config/overlays/rhoai" "config/base")
      ;;
    kf)
      TARGET=kf
      PIPELINERUN=.tekton/odh-kf-notebook-controller-pull-request.yaml
      COMPONENT_DIR=components/notebook-controller
      DOCKERFILE_REL=notebook-controller/Dockerfile
      BUILD_CONTEXT=components
      IMAGE_NAME=kubeflow-notebook-controller
      LOCAL_TAG="localhost/kubeflow-sim/kubeflow-notebook-controller:pr"
      OVERLAYS=("config/overlays/kubeflow" "config/overlays/openshift" "config/overlays/standalone")
      ;;
    *) echo "unknown target '$1' (use odh or kf)" >&2; exit 2 ;;
  esac
  export TARGET PIPELINERUN COMPONENT_DIR DOCKERFILE_REL BUILD_CONTEXT IMAGE_NAME LOCAL_TAG
}

require_pyyaml() {
  python3 -c "import yaml" 2>/dev/null || {
    echo "❌ PyYAML is required: python3 -m pip install --user pyyaml" >&2
    exit 1
  }
}

log()    { echo "::group::$*"; }
endlog() { echo "::endgroup::"; }
ok()   { echo "✅ $*"; }
bad()  { echo "❌ $*" >&2; }

# ---------------------------------------------------------------------------
# static: PipelineRun -> build input contracts
# ---------------------------------------------------------------------------
pipeline_param() {
  # pipeline_param <pipelinerun.yaml> <param name> -> value on stdout
  require_pyyaml
  python3 - "$1" "$2" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
for p in doc["spec"]["params"]:
    if p["name"] == sys.argv[2]:
        print(p["value"])
        sys.exit(0)
sys.exit(1)
PY
}

cmd_static() {
  local failures=0
  local target dockerfile context dockerfile_path

  for target in odh kf; do
    target_info "$target"
    log "static: $PIPELINERUN build inputs"
    dockerfile="$(pipeline_param "$PIPELINERUN" dockerfile)"
    context="$(pipeline_param "$PIPELINERUN" path-context)"
    if [ -z "$dockerfile" ] || [ -z "$context" ]; then
      bad "$PIPELINERUN: could not read dockerfile/path-context params"
      failures=$((failures + 1))
      endlog; continue
    fi
    dockerfile_path="$REPO_ROOT/$context/$dockerfile"
    if [ -f "$dockerfile_path" ]; then
      ok "$PIPELINERUN builds $dockerfile with context $context"
    else
      bad "$PIPELINERUN points at $context/$dockerfile which does not exist"
      failures=$((failures + 1))
    fi
    endlog

    log "static: COPY sources of $IMAGE_NAME Dockerfile"
    if python3 scripts/manifest_contracts.py copy-sources "$dockerfile_path" "$REPO_ROOT/$context"; then :; else
      failures=$((failures + 1))
    fi
    endlog
  done

  log "static: go version contract (go.mod vs builder image)"
  local component go_mod_go dockerfile_go
  for component in components/notebook-controller components/odh-notebook-controller; do
    go_mod_go="$(awk '/^go [0-9]/ {print $2; exit}' "$component/go.mod")"
    dockerfile_go="$(sed -n 's/^ARG GOLANG_VERSION=\([0-9.]*\)$/\1/p' "$component/Dockerfile")"
    if [ -z "$go_mod_go" ] || [ -z "$dockerfile_go" ]; then
      bad "$component: could not read go directive or GOLANG_VERSION"
      failures=$((failures + 1))
      continue
    fi
    local mod_major mod_minor ts_major ts_minor
    mod_major="${go_mod_go%%.*}";      mod_minor="$(echo "${go_mod_go#*.}" | cut -d. -f1)"
    ts_major="${dockerfile_go%%.*}";   ts_minor="$(echo "${dockerfile_go#*.}" | cut -d. -f1)"
    if [ "$mod_major" != "$ts_major" ] || [ "$mod_minor" != "$ts_minor" ] ||
       [ "$mod_minor" -gt "$ts_minor" ]; then
      bad "$component: go.mod requires go $go_mod_go but the Dockerfile builds with go-toolset:$dockerfile_go (RHOAIENG-95421 class: go.mod newer than the builder image)"
      failures=$((failures + 1))
    elif [ "$go_mod_go" != "$dockerfile_go" ]; then
      echo "⚠️  $component: go.mod requires go $go_mod_go, go-toolset:$dockerfile_go floats at build time — a toolset older than $go_mod_go fails the Konflux build with GOTOOLCHAIN=local"
    else
      ok "$component: go.mod ($go_mod_go) and go-toolset ($dockerfile_go) agree"
    fi
  done
  endlog

  log "static: Konflux pipeline defaults"
  echo "⚠️  both PipelineRuns resolve the shared odh-konflux-central multi-arch-container-build pipeline with hermetic=false (default): Konflux builds of this repo are NOT hermetic today; the hermetic subcommand proves the repo could be"
  echo "⚠️  neither PipelineRun sets build-platforms: the shared default builds linux/x86_64 only (RHOAIENG-79767 class multi-arch gap, e.g. ppc64le ImagePullBackOff)"
  endlog

  if [ "$failures" -gt 0 ]; then
    bad "static: $failures contract failure(s)"
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# hermetic <target>: prove the module set is complete offline
# ---------------------------------------------------------------------------
cmd_hermetic() (
  target_info "$1"
  local workdir
  workdir="$(mktemp -d "${TMPDIR:-/tmp}/kubeflow-sim-hermetic.XXXXXX")"
  # The Go module cache is written read-only; chmod before rm on exit.
  cleanup_hermetic() { chmod -R u+w "$workdir" 2>/dev/null || true; rm -rf "$workdir"; }
  trap cleanup_hermetic EXIT

  log "hermetic: $COMPONENT_DIR network phase (populate isolated module cache)"
  (
    cd "$REPO_ROOT/$COMPONENT_DIR"
    GOMODCACHE="$workdir/modcache" GOTOOLCHAIN=auto go mod download
  )
  endlog

  log "hermetic: $COMPONENT_DIR offline phase (GOPROXY=off, the exact Dockerfile build)"
  (
    cd "$REPO_ROOT/$COMPONENT_DIR"
    GOPROXY=off GONOPROXY=none GOSUMDB=off GOFLAGS=-mod=readonly \
      GOMODCACHE="$workdir/modcache" GOTOOLCHAIN=auto \
      CGO_ENABLED=1 GOOS="$SIM_GOOS" GOARCH="$SIM_GOARCH" \
      go build -tags strictfipsruntime -o "$workdir/manager" main.go
  )
  endlog

  if [ -s "$workdir/manager" ]; then
    ok "hermetic: $COMPONENT_DIR builds with the network switched off"
  else
    bad "hermetic: no binary produced"
    exit 1
  fi
)

# ---------------------------------------------------------------------------
# image <target>: build the Konflux image, smoke its entrypoint
# ---------------------------------------------------------------------------
cmd_image() {
  target_info "$1"
  local dockerfile_path="$REPO_ROOT/$BUILD_CONTEXT/$DOCKERFILE_REL"
  local help_file="$SIM_BUILD_DIR/$TARGET-help.txt"

  log "image: build $IMAGE_NAME from $BUILD_CONTEXT/$DOCKERFILE_REL"
  "$SIM_CONTAINER_TOOL" build -f "$dockerfile_path" -t "$LOCAL_TAG" "$REPO_ROOT/$BUILD_CONTEXT"
  endlog

  log "image: entrypoint smoke ($LOCAL_TAG --help)"
  # A controller prints its flag usage and exits non-zero on --help; assert
  # the output, not the status.
  local help_output
  help_output="$("$SIM_CONTAINER_TOOL" run --rm --entrypoint /manager "$LOCAL_TAG" --help 2>&1 || true)"
  printf '%s\n' "$help_output" >"$help_file"
  if printf '%s\n' "$help_output" | grep -Eq '^\s+-{1,2}[A-Za-z]'; then
    ok "image: /manager entrypoint runs and prints its flags ($help_file)"
  else
    bad "image: /manager --help produced no flag output — entrypoint or binary broken"
    printf '%s\n' "$help_output" >&2
    exit 1
  fi
  endlog
}

# ---------------------------------------------------------------------------
# manifests: render + kubeconform + contract checks
# ---------------------------------------------------------------------------
ensure_kubeconform() {
  if [ -n "${SIM_KUBECONFORM_BIN:-}" ]; then
    return
  fi
  local bin="$SIM_BUILD_DIR/bin/kubeconform"
  if [ -x "$bin" ]; then
    SIM_KUBECONFORM_BIN="$bin"
    return
  fi
  local os arch url sha
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "unsupported OS for kubeconform download" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported arch for kubeconform download" >&2; exit 1 ;;
  esac
  case "$os-$arch" in
    linux-amd64)  sha="$KUBECONFORM_SHA256_LINUX_AMD64" ;;
    darwin-arm64) sha="$KUBECONFORM_SHA256_DARWIN_ARM64" ;;
    *) echo "no pinned kubeconform checksum for $os-$arch; set SIM_KUBECONFORM_BIN" >&2; exit 1 ;;
  esac
  url="https://github.com/yannh/kubeconform/releases/download/${KUBECONFORM_VERSION}/kubeconform-${os}-${arch}.tar.gz"
  mkdir -p "$SIM_BUILD_DIR/bin"
  curl -fsSL "$url" -o "$SIM_BUILD_DIR/bin/kubeconform.tar.gz"
  echo "$sha  $SIM_BUILD_DIR/bin/kubeconform.tar.gz" | shasum -a 256 --check --strict
  tar -xzf "$SIM_BUILD_DIR/bin/kubeconform.tar.gz" -C "$SIM_BUILD_DIR/bin" kubeconform
  chmod +x "$bin"
  SIM_KUBECONFORM_BIN="$bin"
}

# Fail on statusInvalid/statusError, and on any skipped resource that claims
# to be a Kubernetes built-in kind: -ignore-missing-schemas counts a
# misspelled built-in ("Deploymnet") as "no schema found", so the skip list
# has to be audited, not trusted (a kubeconform || true would be fail-open).
validate_schema() {
  local render="$1" report="$2"
  set +e
  "$SIM_KUBECONFORM_BIN" -ignore-missing-schemas -verbose -output json -summary \
    "$render" >"$report" 2>"$report.stderr"
  local rc=$?
  set -e
  python3 - "$render" "$report" "$rc" <<'PY'
import json, sys

render, report, rc = sys.argv[1], sys.argv[2], int(sys.argv[3])
problems = []
with open(report) as f:
    content = f.read()
records = []
try:
    doc = json.loads(content)
    if isinstance(doc, dict):
        records = doc.get("resources", [])
    elif isinstance(doc, list):
        records = doc
except json.JSONDecodeError:
    # Older/newer kubeconform may emit JSON lines; handle that too.
    for lineno, line in enumerate(content.splitlines(), 1):
        line = line.strip()
        if not line:
            continue
        try:
            records.append(json.loads(line))
        except json.JSONDecodeError:
            problems.append(f"unparseable kubeconform output at {report}:{lineno}: {line[:120]}")

BUILTIN_GROUPS = ("apps", "batch", "policy", "autoscaling", "extensions")
for rec in records:
    status = rec.get("status")
    if status == "statusValid":
        continue
    kind = rec.get("kind", "?")
    version = rec.get("version", "")
    # kubeconform reports core resources as bare "v1" (no group prefix) and
    # grouped ones as "apps/v1"; partition alone would call "v1" a group.
    group = version.partition("/")[0] if "/" in version else ""
    where = f"{group or 'core'}/{kind} {rec.get('name', '?')}"
    if status == "statusInvalid":
        problems.append(f"{where}: schema-invalid ({rec.get('msg', '')[:160]})")
    elif status == "statusError":
        problems.append(f"{where}: schema lookup error ({rec.get('msg', '')[:160]})")
    elif status == "statusSkipped":
        is_crd = group == "apiextensions.k8s.io" and kind == "CustomResourceDefinition"
        builtin = group == "" or group.endswith(".k8s.io") or group in BUILTIN_GROUPS
        if is_crd:
            continue  # kubeconform ships no CRD schema; CRDs are allowed to skip
        if builtin:
            problems.append(
                f"{where}: built-in kind skipped by kubeconform — misspelled kind or removed API version?"
            )
    else:
        problems.append(f"{where}: unexpected kubeconform status {status}")

if rc != 0 and not problems:
    problems.append(f"kubeconform exited {rc} but no invalid/error records were parsed — check {report}.stderr")
if not records:
    problems.append(f"kubeconform produced no validation records for {render}")
if problems:
    print(f"❌ kubeconform: {render}", file=sys.stderr)
    for p in problems:
        print(f"  {p}", file=sys.stderr)
    sys.exit(1)
print(f"✅ kubeconform: {render} ({len(records)} resources)")
PY
}

render_overlay() {
  local name dir out
  name="$1"
  dir="$2"
  out="$SIM_BUILD_DIR/render-$name.yaml"
  if ! "$SIM_KUSTOMIZE_BIN" build "$REPO_ROOT/$dir" >"$out" 2>"$out.stderr"; then
    bad "kustomize build $dir failed:"
    cat "$out.stderr" >&2
    exit 1
  fi
  if [ -s "$out.stderr" ]; then
    bad "kustomize build $dir produced warnings:"
    cat "$out.stderr" >&2
    exit 1
  fi
  echo "rendered $dir -> $out"
}

cmd_manifests() {
  require_pyyaml
  [ -n "$SIM_KUSTOMIZE_BIN" ] || { echo "❌ kustomize not found; set SIM_KUSTOMIZE_BIN" >&2; exit 1; }
  ensure_kubeconform

  log "manifests: render every overlay the operators ship"
  local odh_dir=components/odh-notebook-controller
  local kf_dir=components/notebook-controller
  render_overlay odh-odh    "$odh_dir/config/overlays/odh"
  render_overlay odh-rhoai  "$odh_dir/config/overlays/rhoai"
  render_overlay odh-base   "$odh_dir/config/base"
  render_overlay kf-kubeflow  "$kf_dir/config/overlays/kubeflow"
  render_overlay kf-openshift "$kf_dir/config/overlays/openshift"
  render_overlay kf-standalone "$kf_dir/config/overlays/standalone"
  endlog

  log "manifests: kubeconform (typo guard)"
  local render
  for render in "$SIM_BUILD_DIR"/render-*.yaml; do
    validate_schema "$render" "$render.kubeconform.json"
  done
  endlog

  log "manifests: RBAC covers every watched kind (RHOAIENG-100911)"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$odh_dir" "$SIM_BUILD_DIR/render-odh-odh.yaml"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$odh_dir" "$SIM_BUILD_DIR/render-odh-rhoai.yaml"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$odh_dir" "$SIM_BUILD_DIR/render-odh-base.yaml"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$kf_dir" "$SIM_BUILD_DIR/render-kf-kubeflow.yaml"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$kf_dir" "$SIM_BUILD_DIR/render-kf-openshift.yaml"
  python3 scripts/manifest_contracts.py rbac-watch-coverage \
    "$kf_dir" "$SIM_BUILD_DIR/render-kf-standalone.yaml"
  endlog

  log "manifests: RBAC identities resolve (RHOAIENG-100514 class)"
  for render in "$SIM_BUILD_DIR"/render-*.yaml; do
    python3 scripts/manifest_contracts.py rbac-identity "$render"
  done
  endlog

  log "manifests: webhook contract (RHOAIENG-99636, RHOAIENG-84738)"
  python3 scripts/manifest_contracts.py webhook-contract \
    "$SIM_BUILD_DIR/render-odh-odh.yaml"
  python3 scripts/manifest_contracts.py webhook-contract \
    "$SIM_BUILD_DIR/render-odh-rhoai.yaml"
  endlog

  log "manifests: duplicate env vars (RHOAIENG-57579 class)"
  for render in "$SIM_BUILD_DIR"/render-*.yaml; do
    python3 scripts/manifest_contracts.py duplicate-env "$render"
  done
  endlog

  log "manifests: workload ConfigMap/Secret references resolve"
  # Externally provided objects, documented:
  #   notebook-controller-culler-config is created at runtime by the
  #     odh-dashboard/workbenches backend cluster-settings API
  #   kf-notebook-controller-metrics-tls and the odh
  #     *-webhook-cert / *-metrics-tls secrets are created by the OpenShift
  #     service-ca injector from Service annotations
  for render in "$SIM_BUILD_DIR"/render-kf-*.yaml; do
    python3 scripts/manifest_contracts.py config-refs "$render" \
      --allow notebook-controller-culler-config \
      --allow kf-notebook-controller-metrics-tls
  done
  for render in "$SIM_BUILD_DIR"/render-odh-*.yaml; do
    python3 scripts/manifest_contracts.py config-refs "$render" \
      --allow odh-notebook-controller-webhook-cert \
      --allow odh-notebook-controller-metrics-tls
  done
  endlog

  log "manifests: params.env values reach the rendered workloads"
  python3 scripts/manifest_contracts.py image-params "$SIM_BUILD_DIR/render-odh-odh.yaml" \
    --image-param "odh-notebook-controller-image=$odh_dir/config/overlays/odh/params.env" \
    --arg-param    "kube-rbac-proxy=$odh_dir/config/overlays/odh/params.env" \
    --env-param    "gateway-url=GATEWAY_URL:$odh_dir/config/overlays/odh/params.env" \
    --env-param    "mlflow-enabled=MLFLOW_ENABLED:$odh_dir/config/overlays/odh/params.env"
  python3 scripts/manifest_contracts.py image-params "$SIM_BUILD_DIR/render-odh-rhoai.yaml" \
    --image-param "odh-notebook-controller-image=$odh_dir/config/overlays/rhoai/params.env" \
    --arg-param    "kube-rbac-proxy=$odh_dir/config/overlays/rhoai/params.env" \
    --env-param    "gateway-url=GATEWAY_URL:$odh_dir/config/overlays/rhoai/params.env" \
    --env-param    "mlflow-enabled=MLFLOW_ENABLED:$odh_dir/config/overlays/rhoai/params.env"
  python3 scripts/manifest_contracts.py image-params "$SIM_BUILD_DIR/render-kf-openshift.yaml" \
    --image-param "odh-kf-notebook-controller-image=$kf_dir/config/overlays/openshift/params.env"
  endlog

  ok "manifests: all contract checks passed"
}

# ---------------------------------------------------------------------------
# flag-contract <target>: every flag the overlays pass must exist in --help
# ---------------------------------------------------------------------------
cmd_flag_contract() {
  target_info "$1"
  require_pyyaml
  local help_file="$SIM_BUILD_DIR/$TARGET-help.txt"
  if [ ! -s "$help_file" ]; then
    bad "no --help capture at $help_file — run '$0 image $TARGET' first"
    exit 1
  fi

  local used overlay
  local render_args=()
  for overlay in "${OVERLAYS[@]}"; do
    local name
    name="${TARGET}-$(basename "$overlay")"
    render_args+=("$SIM_BUILD_DIR/render-$name.yaml")
  done

  log "flag-contract: $IMAGE_NAME overlay flags vs /manager --help (RHOAIENG-100779 class)"
  local used
  used="$(python3 scripts/manifest_contracts.py manager-flags "${render_args[@]}")"
  if [ "$used" = "## no flags" ]; then
    ok "flag-contract: the $IMAGE_NAME overlays pass no flags to the manager; nothing to compare"
    endlog
    return
  fi
  local missing
  missing="$(comm -23 <(printf '%s\n' "$used" | sort) \
                      <(python3 scripts/manifest_contracts.py help-flags "$help_file" | sort))"
  if [ -n "$missing" ]; then
    bad "the $IMAGE_NAME overlays pass flags the binary does not define (container crash-loops during flag parsing):"
    printf '%s\n' "$missing" | sed 's/^/  /' >&2
    exit 1
  fi
  ok "flag-contract: every overlay flag is defined by the built $IMAGE_NAME binary"
  endlog
}

# ---------------------------------------------------------------------------
# kind: deploy the PR images, assert reconcile evidence
# ---------------------------------------------------------------------------
cmd_kind() (
  require_pyyaml
  command -v "$SIM_KIND_BIN" >/dev/null 2>&1 || { echo "❌ kind not found" >&2; exit 1; }
  command -v openssl >/dev/null 2>&1 || { echo "❌ openssl not found" >&2; exit 1; }

  if [ "$SIM_CONTAINER_TOOL" = podman ]; then
    export KIND_EXPERIMENTAL_PROVIDER=podman
  fi

  cmd_image odh
  cmd_image kf

  local created_cluster=false
  if ! "$SIM_KIND_BIN" get clusters 2>/dev/null | grep -qx "$SIM_KIND_CLUSTER"; then
    "$SIM_KIND_BIN" create cluster --name "$SIM_KIND_CLUSTER" --wait 60s
    created_cluster=true
  else
    echo "reusing existing cluster $SIM_KIND_CLUSTER" >&2
  fi
  cleanup_kind() {
    if [ "$created_cluster" = true ]; then
      "$SIM_KIND_BIN" delete cluster --name "$SIM_KIND_CLUSTER" || true
    fi
  }
  trap cleanup_kind EXIT

  local kubeconfig="$SIM_BUILD_DIR/kubeconfig"
  "$SIM_KIND_BIN" export kubeconfig --name "$SIM_KIND_CLUSTER" --kubeconfig "$kubeconfig"
  K() { "$SIM_KUBECTL_BIN" --kubeconfig "$kubeconfig" --context "kind-$SIM_KIND_CLUSTER" "$@"; }

  log "kind: install gateway-api CRDs (odh manager watches HTTPRoute/ReferenceGrant)"
  K apply --server-side -f "$GATEWAY_API_CRDS"
  endlog

  log "kind: load the PR images (nothing is pulled from a registry)"
  # image-archive, not docker-image: the archive path works with both the
  # docker and the podman providers (kind load docker-image cannot resolve
  # podman-local image names).
  local tag archive
  for tag in \
    localhost/kubeflow-sim/odh-notebook-controller:pr \
    localhost/kubeflow-sim/kubeflow-notebook-controller:pr; do
    archive="$SIM_BUILD_DIR/$(echo "$tag" | tr '/:' '--').tar"
    "$SIM_CONTAINER_TOOL" save -o "$archive" "$tag"
    "$SIM_KIND_BIN" load image-archive --name "$SIM_KIND_CLUSTER" "$archive"
  done
  endlog

  # Render from a copy of the tree so the checkout is never mutated.
  local work
  work="$(mktemp -d "${TMPDIR:-/tmp}/kubeflow-sim-kind.XXXXXX")"
  cleanup_work() { rm -rf "$work"; }
  trap 'cleanup_work; cleanup_kind' EXIT
  cp -R "$REPO_ROOT/components" "$work/components"

  log "kind: render the overlays against the local PR tags"
  sed -i.bak -e 's,^odh-notebook-controller-image=.*,odh-notebook-controller-image=localhost/kubeflow-sim/odh-notebook-controller:pr,' \
    "$work/components/odh-notebook-controller/config/overlays/odh/params.env"
  sed -i.bak -e 's,newName: .*,newName: localhost/kubeflow-sim/kubeflow-notebook-controller,' \
             -e 's,newTag: .*,newTag: pr,' \
    "$work/components/notebook-controller/config/base/kustomization.yaml"

  "$SIM_KUSTOMIZE_BIN" build "$work/components/odh-notebook-controller/config/overlays/odh" \
    >"$SIM_BUILD_DIR/kind-odh-raw.yaml"
  "$SIM_KUSTOMIZE_BIN" build "$work/components/notebook-controller/config/overlays/standalone" \
    >"$SIM_BUILD_DIR/kind-kf-raw.yaml"
  python3 scripts/manifest_contracts.py prepare-kind-manifests \
    "$SIM_BUILD_DIR/kind-odh-raw.yaml" "$SIM_BUILD_DIR/kind-odh.yaml" --drop-kind ServiceMonitor
  python3 scripts/manifest_contracts.py prepare-kind-manifests \
    "$SIM_BUILD_DIR/kind-kf-raw.yaml" "$SIM_BUILD_DIR/kind-kf.yaml"
  endlog

  log "kind: mock the OpenShift-serving TLS material"
  # Both namespaces are created here rather than left to `kubectl apply`
  # below: the mocked culler ConfigMap must exist before the kf Deployment
  # starts, and apply of an existing namespace is a no-op.
  K create namespace opendatahub
  K create namespace notebook-controller-system
  local certs="$SIM_BUILD_DIR/certs"
  mkdir -p "$certs"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout "$certs/ca.key" -out "$certs/ca.crt" -subj "/CN=kubeflow-sim-ca" >/dev/null 2>&1
  openssl req -newkey rsa:2048 -nodes \
    -keyout "$certs/tls.key" -out "$certs/tls.csr" \
    -subj "/CN=odh-notebook-controller-webhook-service.opendatahub.svc" >/dev/null 2>&1
  printf 'subjectAltName=DNS:odh-notebook-controller-webhook-service.opendatahub.svc,DNS:odh-notebook-controller-webhook-service.opendatahub.svc.cluster.local,DNS:odh-notebook-controller-service.opendatahub.svc\n' \
    >"$certs/san.ext"
  openssl x509 -req -in "$certs/tls.csr" -CA "$certs/ca.crt" -CAkey "$certs/ca.key" \
    -CAcreateserial -days 2 -extfile "$certs/san.ext" -out "$certs/tls.crt" >/dev/null 2>&1
  K -n opendatahub create secret tls odh-notebook-controller-webhook-cert \
    --cert="$certs/tls.crt" --key="$certs/tls.key"
  K -n opendatahub create secret tls odh-notebook-controller-metrics-tls \
    --cert="$certs/tls.crt" --key="$certs/tls.key"
  # The culler ConfigMap is created at runtime by the odh-dashboard
  # workbenches backend (cluster-settings); the dashboard is not deployed
  # here, so provide its documented defaults.
  K -n notebook-controller-system create configmap notebook-controller-culler-config \
    --from-literal=ENABLE_CULLING=false \
    --from-literal=CULL_IDLE_TIME=31536000 \
    --from-literal=IDLENESS_CHECK_PERIOD=1
  endlog

  log "kind: grant the TLS-profile read the kf manager fail-closes without"
  # REAL FINDING (caught by this very phase, 2026-11): the standalone overlay
  # does not ship the TLS ClusterRole that only config/overlays/openshift
  # adds, and the kf manager resolves the OpenShift APIServer TLS profile
  # with a dynamic client. kube-apiserver authorizes before it routes, so on
  # a cluster without the config.openshift.io API the read is Forbidden (not
  # NoMatch/NotFound), the manager fail-closes and crash-loops:
  #   unable to resolve TLS profile: apiservers.config.openshift.io "cluster"
  #   is forbidden: User "system:serviceaccount:notebook-controller-system:
  #   notebook-controller-service-account" cannot get resource "apiservers"
  # Applying the openshift overlay's own TLS RBAC lets the read through
  # authorization; the API server then answers 404 (group not served), which
  # the manager treats as "not found -> hardened defaults" and starts.
  # Note: the platform operator only renders overlays/openshift, so this
  # only affects the (unsupported) standalone path.
  # The ClusterRole applies as-is; the binding is created with kubectl
  # because the raw file's subject carries no namespace (the openshift
  # overlay's kustomize namespace transformer injects it at render time).
  K apply -f "$work/components/notebook-controller/config/overlays/openshift/rbac/tls_clusterrole.yaml"
  K create clusterrolebinding notebook-controller-openshift-config-reader-binding \
    --clusterrole=notebook-controller-openshift-config-reader \
    --serviceaccount=notebook-controller-system:notebook-controller-service-account
  endlog

  log "kind: apply both controllers"
  K apply -f "$SIM_BUILD_DIR/kind-kf.yaml"
  K apply -f "$SIM_BUILD_DIR/kind-odh.yaml"
  # OpenShift's service-ca injects the caBundle via annotation; in Kind we do
  # it ourselves so the API server trusts the self-signed webhook cert.
  local ca_bundle
  ca_bundle="$(base64 <"$certs/ca.crt" | tr -d '\n')"
  local cfg
  for cfg in mutating validating; do
    K patch "${cfg}"webhookconfiguration \
      "odh-notebook-controller-${cfg}-webhook-configuration" --type=json \
      -p="[{\"op\":\"add\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"$ca_bundle\"}]"
  done
  endlog

  log "kind: wait for both managers (diagnostics inline on failure)"
  if ! K -n notebook-controller-system rollout status \
        deployment/notebook-controller-deployment --timeout=300s; then
    K -n notebook-controller-system get pods -o wide || true
    K -n notebook-controller-system describe deployment/notebook-controller-deployment || true
    K -n notebook-controller-system logs deployment/notebook-controller-deployment --tail=80 || true
    exit 1
  fi
  if ! K -n opendatahub rollout status \
        deployment/odh-notebook-controller-manager --timeout=300s; then
    K -n opendatahub get pods -o wide || true
    K -n opendatahub describe deployment/odh-notebook-controller-manager || true
    K -n opendatahub logs deployment/odh-notebook-controller-manager --tail=80 || true
    exit 1
  fi
  endlog

  log "kind: assert the running images are the PR images"
  local ns deploy want image
  while read -r ns deploy want; do
    image="$(K -n "$ns" get "$deploy" -o jsonpath='{.spec.template.spec.containers[0].image}')"
    if [ "$image" != "$want" ]; then
      bad "$ns/$deploy runs $image, expected $want"
      exit 1
    fi
  done <<IMAGES
opendatahub deployment/odh-notebook-controller-manager localhost/kubeflow-sim/odh-notebook-controller:pr
notebook-controller-system deployment/notebook-controller-deployment localhost/kubeflow-sim/kubeflow-notebook-controller:pr
IMAGES
  ok "both managers run the PR images"
  endlog

  log "kind: reconcile evidence — a Notebook must produce its StatefulSet"
  K create namespace kubeflow-sim-notebooks
  K -n kubeflow-sim-notebooks apply -f \
    "$work/components/odh-notebook-controller/config/samples/_v1_notebook.yaml"
  local deadline=$((SECONDS + 180)) statefulset
  while true; do
    statefulset="$(K -n kubeflow-sim-notebooks get statefulset -o name 2>/dev/null | head -1 || true)"
    if [ -n "$statefulset" ]; then
      echo "created $statefulset"
      break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      bad "no StatefulSet appeared in kubeflow-sim-notebooks within 3m — the kf controller did not reconcile the Notebook"
      K -n kubeflow-sim-notebooks get events --sort-by=.lastTimestamp 2>&1 | tail -20 >&2
      K -n notebook-controller-system logs deployment/notebook-controller-deployment --tail=60 >&2 || true
      exit 1
    fi
    sleep 5
  done
  endlog

  log "kind: assert neither manager crash-looped"
  local restarts
  restarts="$(K -n opendatahub get pods -l app=odh-notebook-controller -o json \
    | python3 -c 'import json,sys
pods = json.load(sys.stdin)["items"]
if not pods: sys.exit(1)
total = sum(cs.get("restartCount", 0) for p in pods for cs in p["status"].get("containerStatuses", []))
print(total)')"
  if [ -z "$restarts" ] || [ "$restarts" != 0 ]; then
    bad "odh-notebook-controller-manager restarted ${restarts:-?} time(s)"
    K -n opendatahub logs deployment/odh-notebook-controller-manager --previous --tail=40 >&2 || true
    exit 1
  fi
  restarts="$(K -n notebook-controller-system get pods -l app=notebook-controller -o json \
    | python3 -c 'import json,sys
pods = json.load(sys.stdin)["items"]
if not pods: sys.exit(1)
total = sum(cs.get("restartCount", 0) for p in pods for cs in p["status"].get("containerStatuses", []))
print(total)')"
  if [ -z "$restarts" ] || [ "$restarts" != 0 ]; then
    bad "notebook-controller-deployment restarted ${restarts:-?} time(s)"
    K -n notebook-controller-system logs deployment/notebook-controller-deployment --previous --tail=40 >&2 || true
    exit 1
  fi
  ok "both managers run with zero restarts"
  endlog

  ok "kind: both PR images deploy and reconcile a Notebook"
)

usage() {
  awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"
}

main() {
  if [ $# -lt 1 ]; then
    usage >&2
    exit 2
  fi
  local command="$1"
  shift
  case "$command" in
    static)        cmd_static "$@" ;;
    hermetic)      [ $# -eq 1 ] || { echo "usage: $0 hermetic <odh|kf>" >&2; exit 2; }
                   cmd_hermetic "$@" ;;
    image)         [ $# -eq 1 ] || { echo "usage: $0 image <odh|kf>" >&2; exit 2; }
                   cmd_image "$@" ;;
    manifests)     cmd_manifests "$@" ;;
    flag-contract) [ $# -eq 1 ] || { echo "usage: $0 flag-contract <odh|kf>" >&2; exit 2; }
                   cmd_flag_contract "$@" ;;
    kind)          cmd_kind "$@" ;;
    *)             usage >&2; exit 2 ;;
  esac
}

main "$@"
