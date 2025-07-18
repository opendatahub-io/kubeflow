# Go 1.24 Upgrade - Ready-to-Use PR Content for openshift/release

## Files to Update in openshift/release Repository

### 1. Main Branch Configuration
**File**: `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml`

**Change needed:**
```yaml
build_root:
  image_stream_tag:
    name: release
    namespace: openshift
    tag: golang-1.24  # Update from golang-1.23
```

**And:**
```yaml
base_images:
  ubi_minimal:
    name: ubi-minimal
    namespace: ocp
    tag: "9"  # Update from "8"
```

### 2. v1.10-branch Configuration  
**File**: `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml`

**Same changes as above:**
- Update `build_root.image_stream_tag.tag` from `golang-1.23` to `golang-1.24`
- Update `base_images.ubi_minimal.tag` from `"8"` to `"9"`

## Ready-to-Use PR Description for openshift/release

```markdown
# Update Go version to 1.24 for opendatahub-io/kubeflow

This PR updates the CI configuration for opendatahub-io/kubeflow to use Go 1.24 and UBI 9.

## Changes
- Update `golang-1.23` to `golang-1.24` in build_root image_stream_tag
- Update UBI minimal base image from tag "8" to "9"

## Files Modified
- `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml`
- `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml`

## Context
This change supports the Go 1.24 upgrade in opendatahub-io/kubeflow repository (PR #657).

## Testing
- [ ] CI configuration validates successfully
- [ ] Build jobs use correct Go version
- [ ] UBI 9 base images are available
```

## Quick Commands to Create the PR

1. **Fork and clone openshift/release:**
```bash
gh repo fork openshift/release --clone
cd release
git checkout -b update-kubeflow-go-1.24
```

2. **Make the changes:**
```bash
# Update main branch config
sed -i 's/golang-1.23/golang-1.24/g' ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml
sed -i 's/tag: "8"/tag: "9"/g' ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml

# Update v1.10-branch config
sed -i 's/golang-1.23/golang-1.24/g' ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml
sed -i 's/tag: "8"/tag: "9"/g' ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml
```

3. **Commit and push:**
```bash
git add .
git commit -m "Update Go version to 1.24 for opendatahub-io/kubeflow"
git push origin update-kubeflow-go-1.24
```

4. **Create PR:**
```bash
gh pr create --title "Update Go version to 1.24 for opendatahub-io/kubeflow" --body "$(cat <<'EOL'
This PR updates the CI configuration for opendatahub-io/kubeflow to use Go 1.24 and UBI 9.

## Changes
- Update golang-1.23 to golang-1.24 in build_root image_stream_tag
- Update UBI minimal base image from tag "8" to "9"

## Files Modified
- ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml
- ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml

## Context
This change supports the Go 1.24 upgrade in opendatahub-io/kubeflow repository.
EOL
)"
   ```bash

## Repository Links
- **openshift/release**: https://github.com/openshift/release
- **Current kubeflow PR**: https://github.com/opendatahub-io/kubeflow/pull/657

---

*This file now contains actionable steps instead of just reminders.*