#!/usr/bin/env bash
set -euo pipefail

if ! command -v jq >/dev/null 2>&1; then
  echo "jq is required but was not found in PATH." >&2
  exit 1
fi

readonly GO_RELEASES_URL="https://go.dev/dl/?mode=json&include=all"
readonly GO_TOOLSET_IMAGE_REPO="registry.access.redhat.com/ubi9/go-toolset"

go_mod_files="$(git ls-files '**/go.mod')"
if [[ -z "${go_mod_files}" ]]; then
  echo "No go.mod files found."
  exit 0
fi

go_toolset_tag_exists() {
  local toolset_tag="$1"
  local image_ref="${GO_TOOLSET_IMAGE_REPO}:${toolset_tag}"

  echo "Checking go-toolset image availability for ${image_ref}..."

  if command -v docker >/dev/null 2>&1; then
    if docker manifest inspect "${image_ref}" >/dev/null 2>&1; then
      echo "go-toolset image tag is available: ${image_ref}"
      return 0
    fi
  fi

  if command -v podman >/dev/null 2>&1; then
    if podman manifest inspect "docker://${image_ref}" >/dev/null 2>&1; then
      echo "go-toolset image tag is available: ${image_ref}"
      return 0
    fi
  fi

  if curl -fsSI \
    -H "Accept: application/vnd.oci.image.index.v1+json" \
    "https://registry.access.redhat.com/v2/ubi9/go-toolset/manifests/${toolset_tag}" \
    >/dev/null 2>&1; then
    echo "go-toolset image tag appears available via registry API: ${image_ref}"
    return 0
  fi

  echo "go-toolset image tag is not available: ${image_ref}" >&2
  return 1
}

version_gt() {
  local v1="$1"
  local v2="$2"
  awk -v a="$v1" -v b="$v2" '
    BEGIN {
      split(a, aa, ".");
      split(b, bb, ".");
      for (i = 1; i <= 3; i++) {
        if ((aa[i] + 0) > (bb[i] + 0)) { print 1; exit }
        if ((aa[i] + 0) < (bb[i] + 0)) { print 0; exit }
      }
      print 0
    }
  '
}

release_json="$(curl -fsSL "${GO_RELEASES_URL}")"
updated_any="false"
declare -a updated_module_dirs=()
declare -A checked_toolset_tags=()

while IFS= read -r go_mod; do
  [[ -z "${go_mod}" ]] && continue

  current_version="$(awk '/^go /{print $2; exit}' "${go_mod}")"
  if [[ -z "${current_version}" ]]; then
    echo "Skipping ${go_mod}: no go directive."
    continue
  fi

  major_minor="$(awk -F. '{print $1"."$2}' <<< "${current_version}")"

  latest_patch="$(
    jq -r --arg mm "${major_minor}" '
      [
        .[].version
        | sub("^go"; "")
        | select(startswith($mm + "."))
      ]
      | map(split(".") | map(tonumber))
      | sort
      | last
      | if . == null then empty else join(".") end
    ' <<< "${release_json}"
  )"

  if [[ -z "${latest_patch}" ]]; then
    echo "Skipping ${go_mod}: no releases found for ${major_minor}.x."
    continue
  fi

  if [[ "${current_version}" == "${latest_patch}" ]]; then
    echo "${go_mod}: already at latest Go patch ${latest_patch}."
    continue
  fi

  target_patch=""
  while IFS= read -r candidate_patch; do
    [[ -z "${candidate_patch}" ]] && continue
    if [[ "$(version_gt "${candidate_patch}" "${current_version}")" != "1" ]]; then
      continue
    fi

    if [[ -z "${checked_toolset_tags[$candidate_patch]+x}" ]]; then
      if go_toolset_tag_exists "${candidate_patch}"; then
        checked_toolset_tags["$candidate_patch"]="ok"
      else
        checked_toolset_tags["$candidate_patch"]="missing"
      fi
    fi

    if [[ "${checked_toolset_tags[$candidate_patch]}" == "ok" ]]; then
      target_patch="${candidate_patch}"
      break
    fi
  done < <(
    jq -r --arg mm "${major_minor}" '
      [
        .[].version
        | sub("^go"; "")
        | select(startswith($mm + "."))
      ]
      | map(split(".") | map(tonumber))
      | sort
      | reverse
      | .[]
      | join(".")
    ' <<< "${release_json}"
  )

  if [[ -z "${target_patch}" ]]; then
    echo "${go_mod}: no newer ${major_minor}.x Go patch has matching go-toolset tag; keeping ${current_version}."
    continue
  fi

  sed -i -E "0,/^go [0-9]+\.[0-9]+\.[0-9]+$/s//go ${target_patch}/" "${go_mod}"
  echo "${go_mod}: updated ${current_version} -> ${target_patch}"
  updated_any="true"
  updated_module_dirs+=("$(dirname "${go_mod}")")
done <<< "${go_mod_files}"

if [[ "${updated_any}" == "true" ]]; then
  printf "%s\n" "${updated_module_dirs[@]}" | sort -u | while IFS= read -r module_dir; do
    [[ -z "${module_dir}" ]] && continue
    if [[ ! -f "${module_dir}/go.sum" ]]; then
      echo "Skipping go mod tidy in ${module_dir}: no go.sum present."
      continue
    fi
    echo "Running go mod tidy in ${module_dir}"
    (
      cd "${module_dir}"
      go_cache_root="$(mktemp -d)"
      trap 'rm -rf "${go_cache_root}"' EXIT
      mkdir -p "${go_cache_root}/gopath" "${go_cache_root}/build-cache"
      export GOPATH="${go_cache_root}/gopath"
      export GOCACHE="${go_cache_root}/build-cache"
      export GOMODCACHE="${GOPATH}/pkg/mod"
      GOTOOLCHAIN=auto go mod tidy
    )
  done
fi

if [[ "${updated_any}" == "true" ]]; then
  echo "updated=true" >> "${GITHUB_OUTPUT:-/dev/null}"
else
  echo "updated=false" >> "${GITHUB_OUTPUT:-/dev/null}"
fi
