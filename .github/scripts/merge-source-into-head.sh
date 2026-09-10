#!/usr/bin/env bash
# Merge origin/$SOURCE_BRANCH into the current branch (expected to be based on
# the sync target), apply .tekton and known-file conflict policy, then commit.
#
# Environment:
#   SOURCE_BRANCH — branch to merge from (required)
#   TARGET_BRANCH — logical target name for the commit message only (required)
#
# Exit codes:
#   0 — merge committed
#   10 — nothing to commit (already up to date)
#   1 — unresolved conflicts or other failure
set -euo pipefail

: "${SOURCE_BRANCH:?SOURCE_BRANCH is required}"
: "${TARGET_BRANCH:?TARGET_BRANCH is required}"

git fetch origin "${SOURCE_BRANCH}:${SOURCE_BRANCH}"
git merge --no-commit "origin/${SOURCE_BRANCH}" || true

#########################################################################
# Always keep target branch state for .tekton
# (do not propagate any .tekton changes from source branch).
# Caveats:
# - This applies to any source/target pair using this script.
# - If target does not contain .tekton, any merged .tekton content from
#   source will be removed.
# - This guard exists only in this automation; manual merges can still
#   propagate .tekton unless handled similarly.

# Reset .tekton completely to target (HEAD) state
git rm -r -f --cached --ignore-unmatch -- ':/.tekton'
rm -rf -- "$(git rev-parse --show-toplevel)/.tekton"

# Re-checkout .tekton only if it exists on target
if git rev-parse --verify HEAD:.tekton >/dev/null 2>&1; then
  echo "Keeping target branch version of .tekton."
  git checkout HEAD -- ':/.tekton'
  git add -A -- ':/.tekton'
fi
#########################################################################

FILES=(
  "components/notebook-controller/config/overlays/openshift/params.env"
  "components/odh-notebook-controller/config/base/params.env"
  "components/odh-notebook-controller/makefile-vars.mk"
)

for FILE in "${FILES[@]}"; do
  if [[ -f "$FILE" && -n "$(git ls-files -u -- "$FILE")" ]]; then
    echo "Resolving conflict for $FILE by keeping target branch version."
    git checkout --ours "$FILE"
    git add "$FILE"
  fi
done

if [[ -n "$(git ls-files -u)" ]]; then
  echo "Unresolved conflicts detected in the following files:"
  git ls-files -u
  echo "Aborting merge due to unexpected conflicts."
  exit 1
fi

if [[ -n "$(git status --porcelain)" ]]; then
  git commit -m "Merge ${SOURCE_BRANCH} into ${TARGET_BRANCH} with known resolved conflicts"
  exit 0
fi

echo "No changes to commit."
exit 10
