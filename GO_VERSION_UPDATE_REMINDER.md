# Go 1.24 Upgrade - Step 6 Action Required

## Action Required: Update openshift/release Repository

Per DEPENDENCIES.md step 6, you need to manually update the Go version in the openshift/release repository:

### Files to Update
1. **Main branch config**: 
   - File: `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-main.yaml`
   - Line: ~10 (go version specification)
   - Change: Update Go version from current version to 1.24

2. **v1.10-branch config**:
   - File: `ci-operator/config/opendatahub-io/kubeflow/opendatahub-io-kubeflow-v1.10-branch.yaml`
   - Line: ~10 (go version specification)
   - Change: Update Go version from current version to 1.24

### Steps
1. Fork/clone the openshift/release repository
2. Update the Go version in both configuration files
3. Create a PR in the openshift/release repository
4. Follow openshift/release PR guidelines for CI configuration changes

### Repository
<https://github.com/openshift/release>

### Current PR Context
This Go 1.24 upgrade PR in opendatahub-io/kubeflow requires corresponding CI configuration updates in openshift/release.

### Note
This step cannot be automated as it requires changes to an external repository (openshift/release) where this automation doesn't have write access.