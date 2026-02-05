# RHOAI 2.25 → 3.3 Upgrade Investigation Knowledge Base

**Date:** February 5, 2026  
**Investigator:** Jiri Daněk  
**Cluster:** <CLUSTER_DOMAIN>  
**Upgrade Path:** rhods-operator.2.25.1 → rhods-operator.3.3.0

---

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Architecture Changes](#architecture-changes)
3. [Investigation Findings](#investigation-findings)
4. [Security Analysis](#security-analysis)
5. [Upgrade Scenarios](#upgrade-scenarios)
6. [Known Issues](#known-issues)
7. [Migration Guide](#migration-guide)
8. [Orphaned Resources Reference](#orphaned-resources-reference)
9. [Appendix: Commands Reference](#appendix-commands-reference)

---

## Executive Summary

The upgrade from RHOAI 2.25 to 3.3 involves a fundamental change in the authentication and routing architecture for workbenches:

| Aspect | RHOAI 2.x | RHOAI 3.x |
|--------|-----------|-----------|
| Auth Proxy | `oauth-proxy` sidecar | `kube-rbac-proxy` sidecar |
| Routing | OpenShift Routes (per-namespace) | Gateway API HTTPRoutes (centralized) |
| Auth Annotation | `inject-oauth: "true"` | `inject-auth: "true"` |
| OAuth Client | Per-workbench OAuthClient CR | Kubernetes native RBAC (SubjectAccessReview) |
| Service | `<notebook>-tls` on oauth-proxy port | `<notebook>-kube-rbac-proxy` on port 8443 |

### Critical Finding

**Unmigrated notebooks create a security vulnerability**: After upgrade, notebooks with only `inject-oauth: true` (no `inject-auth: true`) will have:
- An **unauthenticated HTTPRoute** created by the new controller (pointing to port 8888)
- The **old authenticated OpenShift Route** still active (pointing to oauth-proxy)

This results in **two active routes**, one authenticated and one unauthenticated.

---

## Architecture Changes

### 2.x Architecture (oauth-proxy)

```
User → OpenShift Route → oauth-proxy sidecar → Notebook (port 8888)
                              ↓
                         OAuthClient CR
                              ↓
                     OpenShift OAuth Server
```

**Components created per notebook:**
- `OAuthClient` CR: `<notebook>-<namespace>-oauth-client`
- Service: `<notebook>-tls` (targets oauth-proxy container)
- OpenShift Route: `<notebook>` (in notebook namespace)
- Secret: TLS certificates for oauth-proxy

### 3.x Architecture (kube-rbac-proxy + Gateway API)

```
User → Gateway (centralized) → HTTPRoute → kube-rbac-proxy sidecar → Notebook (port 8888)
                                                    ↓
                                           SubjectAccessReview
                                                    ↓
                                            Kubernetes API
```

**Components created per notebook:**
- Service: `<notebook>-kube-rbac-proxy` (port 8443)
- HTTPRoute: `nb-<namespace>-<notebook>` (in central namespace)
- ReferenceGrant: `notebook-httproute-access` (in notebook namespace)
- ConfigMap: kube-rbac-proxy configuration
- ClusterRoleBinding: `<notebook>-rbac-<namespace>-auth-delegator`
- ServiceAccount: For kube-rbac-proxy

### Key Code Paths

The controller behavior is determined by `KubeRbacProxyInjectionIsEnabled()`:

```go
// From notebook_controller.go lines 427-482
if KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) {
    // Creates authenticated resources:
    // - kube-rbac-proxy Service (port 8443)
    // - HTTPRoute pointing to kube-rbac-proxy service
    // - ClusterRoleBinding for auth delegation
    // - ConfigMap for kube-rbac-proxy config
} else {
    // Creates UNAUTHENTICATED resources:
    // - HTTPRoute pointing directly to notebook service (port 8888)
    // - Cleans up any existing kube-rbac-proxy resources
}
```

The `KubeRbacProxyInjectionIsEnabled()` function checks for annotation:
```go
notebooks.opendatahub.io/inject-auth: "true"
```

**Critical:** It does NOT recognize the legacy `inject-oauth: "true"` annotation!

---

## Investigation Findings

### 1. Notebook Annotation State After Upgrade

| Notebook | Namespace | inject-auth | inject-oauth | Status |
|----------|-----------|-------------|--------------|--------|
| codeserver251 | aadmin-created-namespace | `<none>` | `true` | **NOT MIGRATED** |
| codeserver252 | aadmin-created-namespace | `<none>` | `true` | **NOT MIGRATED** |
| jupyterdatascience251 | aadmin-created-namespace | `<none>` | `true` | **NOT MIGRATED** |
| jupyterdatascience252 | aadmin-created-namespace | `<none>` | `true` | **NOT MIGRATED** |
| medium-pytorch-gpu-later | auser-created-project | `<none>` | `true` | **NOT MIGRATED** |
| rstudioon225 | auser-created-project | `<none>` | `true` | **NOT MIGRATED** |
| min | eee | `true` | `<none>` | Migrated |
| helloooo | hello-namespace | `true` | `<none>` | Migrated |
| q1 | hello | `true` | `<none>` | Migrated |
| test-01 | hello | `true` | `<none>` | Migrated |
| jdanekcustomercase | jdanek | `true` | `<none>` | Migrated |
| jdsupportreallybroken | jdanek | `true` | `<none>` | Migrated |
| myminimaljupyter251 | jdanek | `true` | `<none>` | Migrated |
| wb-32rc3pytorch | jdanek | `true` | `<none>` | Migrated |
| wb-32rc3tensorflow | jdanek | `true` | `<none>` | Migrated |

### 2. HTTPRoute Configuration Comparison

**Unmigrated notebook (codeserver251):**
```yaml
spec:
  rules:
  - backendRefs:
    - name: codeserver251          # Direct to notebook service
      namespace: aadmin-created-namespace
      port: 8888                    # NO AUTHENTICATION
    matches:
    - path:
        type: PathPrefix
        value: /notebook/aadmin-created-namespace/codeserver251
```

**Migrated notebook (min):**
```yaml
spec:
  rules:
  - backendRefs:
    - name: min-kube-rbac-proxy    # Through auth proxy
      namespace: eee
      port: 8443                    # AUTHENTICATED
    matches:
    - path:
        type: PathPrefix
        value: /notebook/eee/min
```

### 3. Dual-Route Situation (Security Issue)

For unmigrated notebooks, BOTH routes are active:

| Route Type | Service | Port | Authentication |
|------------|---------|------|----------------|
| Old OpenShift Route | `<notebook>-tls` | oauth-proxy | **YES** (OAuth) |
| New HTTPRoute | `<notebook>` | 8888 | **NO** |

**Example URLs for `codeserver251`:**
- Old (authenticated): `https://codeserver251-aadmin-created-namespace.apps.<CLUSTER_DOMAIN>/`
- New (unauthenticated): `https://<gateway-host>/notebook/aadmin-created-namespace/codeserver251/`

### 4. Controller RBAC Limitations

The ODH notebook controller **cannot delete OpenShift Routes**:

```yaml
# From config/rbac/role.yaml
- apiGroups:
  - route.openshift.io
  resources:
  - routes
  verbs:
  - get
  - list
  - watch    # NO delete, create, update!
```

This is why old Routes persist after upgrade.

### 5. OAuthClient Cleanup

The controller includes legacy cleanup code for OAuthClient CRs:

```go
// From notebook_controller.go lines 192-214
if notebook.DeletionTimestamp != nil {
    if r.hasOAuthClientFinalizer(notebook) {
        log.Info("Cleaning up OAuthClient before notebook deletion")
        err := r.deleteOAuthClient(notebook, ctx)
        // ... removes finalizer after cleanup
    }
}
```

OAuthClients are only cleaned up when the **notebook is deleted**, not during normal reconciliation.

### 6. Port Mismatch Issue (RHOAIENG-39253)

There's a potential bug in the unauthenticated HTTPRoute:

- HTTPRoute points to service port **8888**
- Upstream Kubeflow service exposes port **80** (targetPort 8888)

```go
// From upstream notebook_controller.go
Ports: []corev1.ServicePort{
    {
        Name:       "http-notebook",
        Port:       80,              // Service port
        TargetPort: intstr.FromInt(8888),  // Container port
    },
}
```

This mismatch may actually **prevent** the unauthenticated HTTPRoute from working, ironically providing some protection.

### 7. Real-World Upgrade Behavior (Confirmed)

Based on live testing on cluster `<CLUSTER_DOMAIN>`:

**New Gateway URLs (shown in Dashboard) → 500 Error:**
```
https://data-science-gateway.apps.<CLUSTER_DOMAIN>/notebook/aadmin-created-namespace/codeserver252
→ HTTP 500 Internal Server Error
```

The HTTPRoute status shows "Accepted" and "ResolvedRefs: True" but traffic fails due to the port mismatch.

**Old OpenShift Route URLs → Still Work:**

| Workbench Type | URL Pattern | Works? |
|----------------|-------------|--------|
| Code-server | `https://<notebook>-<namespace>.apps.../` | **YES** (no path needed) |
| RStudio | `https://<notebook>-<namespace>.apps.../` | **YES** (no path needed) |
| JupyterLab | `https://<notebook>-<namespace>.apps.../` | **NO** (404 without path) |
| JupyterLab | `https://<notebook>-<namespace>.apps.../notebook/<namespace>/<notebook>` | **YES** (path required) |

**Examples:**
```bash
# Works (Code-server/RStudio - no path needed)
https://rstudioon225-auser-created-project.apps.<CLUSTER_DOMAIN>/

# Fails (JupyterLab without path)
https://medium-pytorch-gpu-later-auser-created-project.apps.<CLUSTER_DOMAIN>/
→ 404 Not Found

# Works (JupyterLab with path)
https://medium-pytorch-gpu-later-auser-created-project.apps.<CLUSTER_DOMAIN>/notebook/auser-created-project/medium-pytorch-gpu-later
```

**Dashboard UX Issue:**
- Dashboard now shows new Gateway URLs (which don't work for unmigrated notebooks)
- Old working URLs are hidden - must go to OpenShift Console → Routes to find them
- For JupyterLab, even the Routes tab shows URL without path, requiring manual path append

---

## Security Analysis

### Pre-Upgrade (RHOAI 2.x)

- **Authentication:** OAuth-based via oauth-proxy sidecar
- **Authorization:** OAuthClient CR per workbench
- **Routing:** OpenShift Routes in user namespaces
- **Vulnerability:** HTTPRoute hijacking possible (RHOAIENG-38009) - users could create HTTPRoutes pointing to other users' services

### During Upgrade

- **Risk Window:** When operator is updating but notebooks haven't been migrated
- **Issue:** New controller creates unauthenticated HTTPRoutes for old notebooks
- **Mitigation:** Stop all notebooks before upgrade

### Post-Upgrade (RHOAI 3.x)

**For migrated notebooks (`inject-auth: true`):**
- Authentication via kube-rbac-proxy using Kubernetes RBAC
- HTTPRoutes centralized in `redhat-ods-applications` namespace
- ReferenceGrants control cross-namespace access

**For unmigrated notebooks (`inject-oauth: true` only):**
- **CRITICAL:** Unauthenticated HTTPRoute created
- Old OAuth route may still work (if oauth-proxy container still in pod)
- Two active routes with different auth levels

### ReferenceGrant Security (RHOAIENG-38217)

Current ReferenceGrant allows **any** service in the namespace:

```yaml
spec:
  to:
  - group: ""
    kind: "Service"
    # name: is NOT specified - allows ALL services
```

This is overly permissive but scoped by namespace.

---

## Upgrade Scenarios

### Scenario 1: Proper Migration (Recommended)

1. Stop all notebooks
2. Upgrade operator
3. Apply migration patch to each notebook
4. Delete old OpenShift Routes
5. Clean up orphaned resources
6. Restart notebooks

**Result:** Authenticated access via kube-rbac-proxy

### Scenario 2: Careless Upgrade (No Migration)

1. Upgrade operator without stopping notebooks
2. Don't apply migration patches

**Result:**
- Old oauth-proxy containers remain in pods
- New unauthenticated HTTPRoutes created
- Both old Route and new HTTPRoute active
- **Security vulnerability: unauthenticated access possible**

### Scenario 3: Partial Migration

1. Upgrade operator
2. Migrate some notebooks but not all

**Result:** Mixed state with some authenticated, some not. Requires tracking.

---

## Known Issues

### 1. HardwareProfile Migration Issue

**Symptom:** Operator CrashLoopBackOff  
**Error:**
```
failed to set HardwareProfile annotation for notebook medium-pytorch-gpu-later: 
admission webhook "hardwareprofile-notebook-injector.opendatahub.io" denied the request: 
hardware profile 'migrated-gpu-notebooks' not found
```

**Cause:** Notebook references legacy accelerator profile that doesn't exist as HardwareProfile  
**Fix:** Create missing HardwareProfile or update notebook annotation

### 2. Image Tag Ambiguity (RHAIRFE-1251)

The `2025.2` image tag was used across multiple RHOAI releases:
- RHOAI 2.25.0
- RHOAI 3.0
- RHOAI 3.2
- RHOAI 3.3

**Critical:** 2025.2 images from RHOAI 2.25.0 are NOT compatible with 3.x due to missing Gateway API path-based routing changes.

**Related Issues:**
- RHOAIENG-31693: Commit hash display is not working correctly
- RHAIRFE-1251: RFE for admin compatibility checking tool

### 3. OAuth Token Refresh

After upgrade, OAuth tokens may fail to refresh because:
- OAuthClient CR is no longer used for new auth flow
- Old oauth-proxy may attempt token refresh against non-existent OAuthClient
- Cluster OAuth server configuration may not match

### 4. Pending Updates on Running Workbenches

After upgrade, the controller may want to apply changes to existing workbenches but holds back to avoid restarting running pods.

**Symptom:** Annotation `notebooks.opendatahub.io/update-pending` appears on notebooks:

```
notebooks.opendatahub.io/update-pending: '{v1.PodSpec}.Containers[0].Env[7->?]: 
  {Name:KF_PIPELINES_SSL_SA_CERTS Value:/etc/pki/tls/custom-certs/ca-bundle.crt ValueFrom:nil} 
  != <invalid reflect.Value>'
```

**What is `KF_PIPELINES_SSL_SA_CERTS`?**

This is a **compatibility addition**, not new functionality. Added in commit `4afe7d18e` (Oct 13, 2025):

```diff
- envVars := []string{"PIP_CERT", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "PIPELINES_SSL_SA_CERTS", "GIT_SSL_CAINFO"}
+ envVars := []string{"PIP_CERT", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "PIPELINES_SSL_SA_CERTS", "GIT_SSL_CAINFO", "KF_PIPELINES_SSL_SA_CERTS"}
```

**Why it was added:**
- Kubeflow Pipelines SDK / Elyra started looking for `KF_PIPELINES_SSL_SA_CERTS` (in addition to `PIPELINES_SSL_SA_CERTS`)
- Both env vars point to the same CA bundle path: `/etc/pki/tls/custom-certs/ca-bundle.crt`
- This ensures SSL certificates work correctly with pipelines in workbenches

**Impact:**
- Old workbenches (from 2.25) only have `PIPELINES_SSL_SA_CERTS`
- New controller wants to also add `KF_PIPELINES_SSL_SA_CERTS`
- It's holding back to avoid restarting running pods
- Will be applied on next restart - not a breaking change

**When update is applied:**
- When notebook is stopped and started
- When user manually restarts workbench
- After migration (delete/recreate or patch)

**Check for pending updates:**
```bash
oc get notebooks -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: {.metadata.annotations.notebooks\.opendatahub\.io/update-pending}{"\n"}{end}' | grep -v ": $"
```

This is a safety mechanism, not a bug.

### 5. Dashboard Shows Broken Links (RHOAIENG-48747)

After upgrade, the RHOAI Dashboard shows Gateway-based URLs that don't work for unmigrated notebooks:

**Symptom:**
- Clicking "Open" in Dashboard → 500 error
- Dashboard URL: `https://data-science-gateway.apps.../notebook/<namespace>/<notebook>`

**Workaround for users:**
1. Go to OpenShift Console → Networking → Routes
2. Find the route for your notebook in your namespace
3. Click the route URL
4. For JupyterLab: append `/notebook/<namespace>/<notebook>` to the URL

**Workaround for admins:**
Document the old URL patterns for users:
```
# Code-server / RStudio (no path needed)
https://<notebook>-<namespace>.apps.<cluster>/

# JupyterLab (path required)
https://<notebook>-<namespace>.apps.<cluster>/notebook/<namespace>/<notebook>
```

---

## Migration Guide

### Pre-Upgrade Checklist

- [ ] Verify all workbench images are 2025.2 from RHOAI 3.0+
- [ ] Ensure custom images support path-based routing (`${NB_PREFIX}`)
- [ ] Stop all notebooks in all namespaces
- [ ] Document current notebook state

### Migration Commands

**Option 1: Delete and Recreate (Recommended)**

```bash
# Delete notebook (PVC preserved)
oc delete notebook <NAME> -n <NAMESPACE>

# Recreate via Dashboard or CLI with same PVC
```

**Option 2: In-Place Patch (Original - RISKY)**

```bash
oc patch notebook <NAME> -n <NAMESPACE> --type='json' -p='[
  {"op":"add","path":"/metadata/annotations/notebooks.opendatahub.io~1inject-auth","value":"true"},
  {"op":"remove","path":"/metadata/annotations/notebooks.opendatahub.io~1inject-oauth"},
  {"op":"remove","path":"/metadata/annotations/notebooks.opendatahub.io~1oauth-logout-url"},
  {"op":"remove","path":"/spec/template/spec/containers/1"},
  {"op":"remove","path":"/metadata/finalizers/0"}
]'
```

**WARNING:** The patch uses hardcoded array indices. Problems:
1. **Container index `containers/1`**: Assumes oauth-proxy is always at index 1. If additional sidecars exist or order differs, this removes the wrong container.
2. **Finalizer index `finalizers/0`**: Blindly removes first finalizer. The OAuth finalizer (`notebook-oauth-client-finalizer.opendatahub.io`) may not be at index 0.
3. **Annotations may not exist**: If annotations don't exist, the `remove` operation fails.

**Option 3: In-Place Patch (IMPROVED - Safer)**

```bash
# Step 1: Verify what you're working with
oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.spec.template.spec.containers[*].name}'
oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.metadata.finalizers}'

# Step 2: Update annotations using strategic merge (more forgiving, nulls are ignored if missing)
oc patch notebook <NAME> -n <NAMESPACE> --type='merge' -p='
metadata:
  annotations:
    notebooks.opendatahub.io/inject-auth: "true"
    notebooks.opendatahub.io/inject-oauth: null
    notebooks.opendatahub.io/oauth-logout-url: null
'

# Step 3: Remove oauth-proxy container by name (uses test to verify before removing)
# First find the correct index
OAUTH_INDEX=$(oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}' | grep -n oauth-proxy | cut -d: -f1)
OAUTH_INDEX=$((OAUTH_INDEX - 1))  # Convert to 0-based index

oc patch notebook <NAME> -n <NAMESPACE> --type='json' -p="[
  {\"op\":\"test\",\"path\":\"/spec/template/spec/containers/${OAUTH_INDEX}/name\",\"value\":\"oauth-proxy\"},
  {\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/${OAUTH_INDEX}\"}
]"

# Step 4: Remove the specific finalizer by finding its index
FINALIZER="notebook-oauth-client-finalizer.opendatahub.io"
FINALIZER_INDEX=$(oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.metadata.finalizers}' | tr -d '[]"' | tr ',' '\n' | grep -n "$FINALIZER" | cut -d: -f1)
FINALIZER_INDEX=$((FINALIZER_INDEX - 1))  # Convert to 0-based index

oc patch notebook <NAME> -n <NAMESPACE> --type='json' -p="[
  {\"op\":\"test\",\"path\":\"/metadata/finalizers/${FINALIZER_INDEX}\",\"value\":\"${FINALIZER}\"},
  {\"op\":\"remove\",\"path\":\"/metadata/finalizers/${FINALIZER_INDEX}\"}
]"
```

**Why this is safer:**
- Uses `test` operation to verify before removing (fails safely if wrong)
- Finds container/finalizer index dynamically by name/value
- Strategic merge patch ignores missing annotations instead of failing
- Each step can be verified before proceeding

### Cluster-Wide Migration Scripts (Simplified Workflow)

This workflow takes advantage of a key insight: after updating annotations (Phase 1), workbenches will **self-migrate** when users naturally restart them. The webhook injects kube-rbac-proxy on pod creation.

**Understanding the workflow:**

| Phase | Scope | Safe While Running? | When to Run |
|-------|-------|---------------------|-------------|
| **Phase 1** | Cluster-wide | ✅ Yes | Immediately after upgrade |
| **Phase 2** | Optional cleanup | ✅ Yes (metadata only) | Anytime after Phase 1 |
| **Phase 3** | Per-namespace | ✅ Yes | After users restart |

**What happens after Phase 1 when user restarts:**

| Component | State |
|-----------|-------|
| oauth-proxy container | Still present (from stored spec) |
| kube-rbac-proxy container | **Injected** (by webhook on restart) |
| Pod containers | **3 total** (notebook + oauth-proxy + kube-rbac-proxy) |
| New Gateway route | **Works** (via kube-rbac-proxy) |
| Old OpenShift route | **Works** (via oauth-proxy, if not deleted) |

**This is a valid intermediate state** - both auth mechanisms work simultaneously until cleanup.

---

#### Phase 1: Cluster-Wide Annotation Update (SAFE - Run Immediately)

Run this once after upgrade. Safe for hundreds of workbenches - no restarts triggered.

```bash
#!/bin/bash
# phase1_cluster_wide_migration.sh
# SAFE to run on ALL workbenches cluster-wide - no restarts triggered
# Run this immediately after RHOAI 3.3 upgrade

set -euo pipefail

LOG_FILE="migration_phase1_$(date +%Y%m%d_%H%M%S).log"

echo "================================================================" | tee -a "$LOG_FILE"
echo "RHOAI 3.3 Migration - Phase 1: Cluster-Wide Annotation Update" | tee -a "$LOG_FILE"
echo "Started: $(date)" | tee -a "$LOG_FILE"
echo "================================================================" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"
echo "This phase is SAFE - no workbenches will restart" | tee -a "$LOG_FILE"
echo "Workbenches will self-migrate when users restart them" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"

# Count totals
TOTAL=0
MIGRATED=0
ALREADY_DONE=0
ERRORS=0

# Get all notebooks with inject-oauth annotation (unmigrated 2.x workbenches)
echo "Scanning for unmigrated workbenches..." | tee -a "$LOG_FILE"

# Process all notebooks
oc get notebooks -A -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)"' | while read NB; do
    NAMESPACE=$(echo "$NB" | cut -d'/' -f1)
    NAME=$(echo "$NB" | cut -d'/' -f2)
    
    # Check current state
    HAS_INJECT_OAUTH=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.annotations.notebooks\.opendatahub\.io/inject-oauth}' 2>/dev/null || echo "")
    HAS_INJECT_AUTH=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.annotations.notebooks\.opendatahub\.io/inject-auth}' 2>/dev/null || echo "")
    
    # Skip if already migrated
    if [ "$HAS_INJECT_AUTH" = "true" ] && [ -z "$HAS_INJECT_OAUTH" ]; then
        echo "SKIP: $NAMESPACE/$NAME (already migrated)" | tee -a "$LOG_FILE"
        continue
    fi
    
    # Skip if no oauth annotation (not a 2.x workbench)
    if [ -z "$HAS_INJECT_OAUTH" ] && [ -z "$HAS_INJECT_AUTH" ]; then
        echo "SKIP: $NAMESPACE/$NAME (no auth annotations - custom workbench?)" | tee -a "$LOG_FILE"
        continue
    fi
    
    echo "MIGRATE: $NAMESPACE/$NAME" | tee -a "$LOG_FILE"
    
    # Check if running
    REPLICAS=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "${REPLICAS:-0}" != "0" ]; then
        echo "  Status: RUNNING (will self-migrate on next restart)" | tee -a "$LOG_FILE"
    else
        echo "  Status: STOPPED (will migrate on next start)" | tee -a "$LOG_FILE"
    fi
    
    # Add inject-auth annotation
    if oc annotate notebook "$NAME" -n "$NAMESPACE" \
        notebooks.opendatahub.io/inject-auth="true" \
        --overwrite 2>>"$LOG_FILE"; then
        echo "  ✓ Added inject-auth annotation" | tee -a "$LOG_FILE"
    else
        echo "  ✗ Failed to add inject-auth annotation" | tee -a "$LOG_FILE"
    fi
    
    # Remove old annotations (ignore errors if not present)
    oc annotate notebook "$NAME" -n "$NAMESPACE" \
        notebooks.opendatahub.io/inject-oauth- \
        notebooks.opendatahub.io/oauth-logout-url- \
        2>/dev/null || true
    echo "  ✓ Removed legacy annotations" | tee -a "$LOG_FILE"
    
    # Remove OAuth finalizer if present
    FINALIZERS=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.finalizers}' 2>/dev/null || echo "")
    if echo "$FINALIZERS" | grep -q "notebook-oauth-client-finalizer"; then
        # Find finalizer index
        FINALIZER_JSON=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.finalizers}')
        # Use jq to find and remove the specific finalizer
        NEW_FINALIZERS=$(echo "$FINALIZER_JSON" | jq -c '[.[] | select(. != "notebook-oauth-client-finalizer.opendatahub.io")]')
        
        if oc patch notebook "$NAME" -n "$NAMESPACE" --type='merge' \
            -p "{\"metadata\":{\"finalizers\":$NEW_FINALIZERS}}" 2>>"$LOG_FILE"; then
            echo "  ✓ Removed OAuth finalizer" | tee -a "$LOG_FILE"
        else
            echo "  ✗ Failed to remove OAuth finalizer" | tee -a "$LOG_FILE"
        fi
    fi
    
    echo "" | tee -a "$LOG_FILE"
done

echo "================================================================" | tee -a "$LOG_FILE"
echo "Phase 1 Complete: $(date)" | tee -a "$LOG_FILE"
echo "Log saved to: $LOG_FILE" | tee -a "$LOG_FILE"
echo "================================================================" | tee -a "$LOG_FILE"
echo "" | tee -a "$LOG_FILE"
echo "NEXT STEPS:" | tee -a "$LOG_FILE"
echo "1. Notify users that workbenches will auto-upgrade on restart" | tee -a "$LOG_FILE"
echo "2. Users can continue working - no immediate action required" | tee -a "$LOG_FILE"
echo "3. When users restart, both old and new URLs will work" | tee -a "$LOG_FILE"
echo "4. Run Phase 2 (optional) to remove legacy oauth-proxy containers" | tee -a "$LOG_FILE"
echo "5. Run Phase 3 to cleanup orphaned resources" | tee -a "$LOG_FILE"
```

---

#### Phase 2: Remove Legacy oauth-proxy Container (OPTIONAL)

This is **optional** - workbenches function correctly with both proxies. Run this to reduce resource usage. Only works on stopped workbenches.

```bash
#!/bin/bash
# phase2_remove_oauth_proxy.sh
# OPTIONAL: Remove legacy oauth-proxy container to reduce resource usage
# Only works on STOPPED workbenches

set -euo pipefail

MODE="${1:-single}"  # "single" for one workbench, "all-stopped" for all stopped workbenches

if [ "$MODE" = "single" ]; then
    NAMESPACE=$2
    NAME=$3
    if [ -z "$NAMESPACE" ] || [ -z "$NAME" ]; then
        echo "Usage: $0 single <namespace> <notebook-name>"
        echo "   or: $0 all-stopped"
        exit 1
    fi
    NOTEBOOKS="$NAMESPACE/$NAME"
elif [ "$MODE" = "all-stopped" ]; then
    echo "Finding all stopped workbenches with oauth-proxy container..."
    NOTEBOOKS=$(oc get notebooks -A -o json | jq -r '
        .items[] | 
        select(.status.readyReplicas == 0 or .status.readyReplicas == null) |
        select(.spec.template.spec.containers | map(.name) | any(. == "oauth-proxy")) |
        "\(.metadata.namespace)/\(.metadata.name)"
    ')
    if [ -z "$NOTEBOOKS" ]; then
        echo "No stopped workbenches with oauth-proxy found."
        exit 0
    fi
    echo "Found workbenches:"
    echo "$NOTEBOOKS"
    echo ""
    read -p "Remove oauth-proxy from all these workbenches? (y/N) " confirm
    if [ "$confirm" != "y" ]; then
        echo "Aborted."
        exit 0
    fi
else
    echo "Usage: $0 single <namespace> <notebook-name>"
    echo "   or: $0 all-stopped"
    exit 1
fi

for NB in $NOTEBOOKS; do
    NAMESPACE=$(echo "$NB" | cut -d'/' -f1)
    NAME=$(echo "$NB" | cut -d'/' -f2)
    
    echo "Processing: $NAMESPACE/$NAME"
    
    # Verify workbench is stopped
    REPLICAS=$(oc get notebook "$NAME" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "${REPLICAS:-0}" != "0" ]; then
        echo "  SKIP: Workbench is running (replicas=$REPLICAS)"
        continue
    fi
    
    # Find oauth-proxy container index
    OAUTH_INDEX=$(oc get notebook "$NAME" -n "$NAMESPACE" -o json | \
        jq '.spec.template.spec.containers | to_entries | .[] | select(.value.name == "oauth-proxy") | .key')
    
    if [ -z "$OAUTH_INDEX" ]; then
        echo "  SKIP: No oauth-proxy container found"
        continue
    fi
    
    echo "  Found oauth-proxy at index $OAUTH_INDEX"
    
    # Remove oauth-proxy container using test+remove
    if oc patch notebook "$NAME" -n "$NAMESPACE" --type='json' -p="[
      {\"op\":\"test\",\"path\":\"/spec/template/spec/containers/$OAUTH_INDEX/name\",\"value\":\"oauth-proxy\"},
      {\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/$OAUTH_INDEX\"}
    ]" 2>/dev/null; then
        echo "  ✓ Removed oauth-proxy container"
    else
        echo "  ✗ Failed to remove oauth-proxy container"
    fi
done

echo ""
echo "Phase 2 complete. Workbenches will start with only kube-rbac-proxy."
```

---

#### Phase 3: Cleanup Orphaned Resources (Cluster-Wide)

Run this after workbenches have been restarted to remove orphaned OAuth resources.

```bash
#!/bin/bash
# phase3_cleanup_orphaned_resources.sh
# Cleanup orphaned OAuth resources cluster-wide
# Safe to run anytime - only removes resources for migrated workbenches

set -euo pipefail

MODE="${1:-report}"  # "report" to show what would be deleted, "delete" to actually delete

LOG_FILE="migration_phase3_$(date +%Y%m%d_%H%M%S).log"

echo "================================================================" | tee -a "$LOG_FILE"
echo "RHOAI 3.3 Migration - Phase 3: Cleanup Orphaned Resources" | tee -a "$LOG_FILE"
echo "Mode: $MODE" | tee -a "$LOG_FILE"
echo "Started: $(date)" | tee -a "$LOG_FILE"
echo "================================================================" | tee -a "$LOG_FILE"

# Find all migrated notebooks (have inject-auth, don't have inject-oauth)
echo "" | tee -a "$LOG_FILE"
echo "Finding migrated workbenches..." | tee -a "$LOG_FILE"

oc get notebooks -A -o json | jq -r '
    .items[] |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-auth"] == "true") |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == null or 
           .metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == "") |
    "\(.metadata.namespace)/\(.metadata.name)"
' | while read NB; do
    NAMESPACE=$(echo "$NB" | cut -d'/' -f1)
    NAME=$(echo "$NB" | cut -d'/' -f2)
    
    echo "" | tee -a "$LOG_FILE"
    echo "Checking: $NAMESPACE/$NAME" | tee -a "$LOG_FILE"
    
    # Check for orphaned resources
    ROUTE_EXISTS=$(oc get route "$NAME" -n "$NAMESPACE" 2>/dev/null && echo "yes" || echo "no")
    TLS_SVC_EXISTS=$(oc get svc "${NAME}-tls" -n "$NAMESPACE" 2>/dev/null && echo "yes" || echo "no")
    OAUTH_CLIENT_EXISTS=$(oc get oauthclient "${NAME}-${NAMESPACE}-oauth-client" 2>/dev/null && echo "yes" || echo "no")
    OAUTH_SECRET_EXISTS=$(oc get secret "${NAME}-oauth-client" -n "$NAMESPACE" 2>/dev/null && echo "yes" || echo "no")
    
    if [ "$ROUTE_EXISTS" = "no" ] && [ "$TLS_SVC_EXISTS" = "no" ] && \
       [ "$OAUTH_CLIENT_EXISTS" = "no" ] && [ "$OAUTH_SECRET_EXISTS" = "no" ]; then
        echo "  No orphaned resources found" | tee -a "$LOG_FILE"
        continue
    fi
    
    echo "  Orphaned resources found:" | tee -a "$LOG_FILE"
    [ "$ROUTE_EXISTS" = "yes" ] && echo "    - Route: $NAME" | tee -a "$LOG_FILE"
    [ "$TLS_SVC_EXISTS" = "yes" ] && echo "    - Service: ${NAME}-tls" | tee -a "$LOG_FILE"
    [ "$OAUTH_CLIENT_EXISTS" = "yes" ] && echo "    - OAuthClient: ${NAME}-${NAMESPACE}-oauth-client" | tee -a "$LOG_FILE"
    [ "$OAUTH_SECRET_EXISTS" = "yes" ] && echo "    - Secret: ${NAME}-oauth-client" | tee -a "$LOG_FILE"
    
    if [ "$MODE" = "delete" ]; then
        echo "  Deleting..." | tee -a "$LOG_FILE"
        
        [ "$ROUTE_EXISTS" = "yes" ] && \
            oc delete route "$NAME" -n "$NAMESPACE" --ignore-not-found && \
            echo "    ✓ Deleted Route" | tee -a "$LOG_FILE"
        
        [ "$TLS_SVC_EXISTS" = "yes" ] && \
            oc delete svc "${NAME}-tls" -n "$NAMESPACE" --ignore-not-found && \
            echo "    ✓ Deleted TLS Service" | tee -a "$LOG_FILE"
        
        [ "$OAUTH_CLIENT_EXISTS" = "yes" ] && \
            oc delete oauthclient "${NAME}-${NAMESPACE}-oauth-client" --ignore-not-found && \
            echo "    ✓ Deleted OAuthClient" | tee -a "$LOG_FILE"
        
        # Delete all oauth-related secrets
        oc delete secret "${NAME}-oauth-client" "${NAME}-oauth-config" "${NAME}-tls" \
            -n "$NAMESPACE" --ignore-not-found 2>/dev/null && \
            echo "    ✓ Deleted OAuth secrets" | tee -a "$LOG_FILE"
    fi
done

echo "" | tee -a "$LOG_FILE"
echo "================================================================" | tee -a "$LOG_FILE"
echo "Phase 3 Complete: $(date)" | tee -a "$LOG_FILE"
if [ "$MODE" = "report" ]; then
    echo "This was a DRY RUN. To actually delete, run:" | tee -a "$LOG_FILE"
    echo "  $0 delete" | tee -a "$LOG_FILE"
fi
echo "Log saved to: $LOG_FILE" | tee -a "$LOG_FILE"
echo "================================================================" | tee -a "$LOG_FILE"
```

---

#### Migration Status Report Script

Use this to check migration status across the cluster.

```bash
#!/bin/bash
# migration_status_report.sh
# Generate a report of migration status across the cluster

echo "================================================================"
echo "RHOAI 3.3 Migration Status Report"
echo "Generated: $(date)"
echo "================================================================"
echo ""

# Count by status
echo "=== Summary ==="
TOTAL=$(oc get notebooks -A --no-headers 2>/dev/null | wc -l)
MIGRATED=$(oc get notebooks -A -o json | jq '[.items[] | select(.metadata.annotations["notebooks.opendatahub.io/inject-auth"] == "true") | select(.metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == null or .metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == "")] | length')
PHASE1_DONE=$(oc get notebooks -A -o json | jq '[.items[] | select(.metadata.annotations["notebooks.opendatahub.io/inject-auth"] == "true")] | length')
UNMIGRATED=$(oc get notebooks -A -o json | jq '[.items[] | select(.metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == "true")] | length')

echo "Total workbenches: $TOTAL"
echo "Fully migrated (Phase 1 done, no inject-oauth): $MIGRATED"
echo "Phase 1 complete (inject-auth set): $PHASE1_DONE"
echo "Unmigrated (still have inject-oauth): $UNMIGRATED"
echo ""

echo "=== Unmigrated Workbenches (need Phase 1) ==="
oc get notebooks -A -o json | jq -r '
    .items[] |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == "true") |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-auth"] != "true") |
    "\(.metadata.namespace)\t\(.metadata.name)\t\(.status.readyReplicas // 0) replicas"
' | column -t -s $'\t'
echo ""

echo "=== Phase 1 Done, Awaiting Restart ==="
oc get notebooks -A -o json | jq -r '
    .items[] |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-auth"] == "true") |
    select(.metadata.annotations["notebooks.opendatahub.io/inject-oauth"] == "true") |
    "\(.metadata.namespace)\t\(.metadata.name)\t\(.status.readyReplicas // 0) replicas"
' | column -t -s $'\t'
echo ""

echo "=== Workbenches with oauth-proxy container (optional cleanup) ==="
oc get notebooks -A -o json | jq -r '
    .items[] |
    select(.spec.template.spec.containers | map(.name) | any(. == "oauth-proxy")) |
    "\(.metadata.namespace)\t\(.metadata.name)\t\(.status.readyReplicas // 0) replicas"
' | column -t -s $'\t'
echo ""

echo "=== Orphaned OAuthClients ==="
oc get oauthclients -o name 2>/dev/null | grep -E ".*-oauth-client$" || echo "None found"
echo ""

echo "=== Orphaned Routes (in user namespaces) ==="
# Find routes that point to -tls services (legacy oauth pattern)
oc get routes -A -o json 2>/dev/null | jq -r '
    .items[] |
    select(.spec.to.name | endswith("-tls")) |
    "\(.metadata.namespace)\t\(.metadata.name)\t\(.spec.to.name)"
' | column -t -s $'\t' || echo "None found"
```

---

#### User Communication Template (Updated)

```
Subject: RHOAI 3.3 Upgrade Complete - Workbench Auto-Migration

Hello,

We have completed the RHOAI 3.3 upgrade. Your workbenches will automatically 
migrate to the new authentication system.

WHAT YOU NEED TO KNOW:
- Your workbenches continue to work normally
- When you next restart your workbench, it will automatically upgrade
- Both old and new access URLs will work during the transition
- Your data is safe - only the authentication method changes

WHAT YOU NEED TO DO:
- Nothing immediate - continue working normally
- When convenient, restart your workbench to complete the migration
- After restart, use the Dashboard to access your workbench

If you have bookmarked the old URL, it will continue to work until 
we complete the final cleanup phase in [TIMEFRAME].

Questions? Contact [ADMIN_EMAIL]
```

### Post-Migration Cleanup

```bash
# Delete old OpenShift Route
oc delete route <NOTEBOOK_NAME> -n <NAMESPACE>

# Delete orphaned TLS service
oc delete service <NOTEBOOK_NAME>-tls -n <NAMESPACE>

# Delete OAuthClient (if exists)
oc delete oauthclient <NOTEBOOK_NAME>-<NAMESPACE>-oauth-client

# Delete TLS secrets
oc delete secret <NOTEBOOK_NAME>-tls -n <NAMESPACE>
```

---

## Orphaned Resources Reference

After upgrade, unmigrated notebooks leave behind legacy resources that must be manually cleaned up. This section documents what resources exist and which ones should be deleted.

### Resource Inventory Per Unmigrated Notebook

| Resource Type | Naming Pattern | Location | Status |
|---------------|----------------|----------|--------|
| **OAuthClient** | `<notebook>-<namespace>-oauth-client` | Cluster-scoped | **ORPHANED** - delete |
| **TLS Service** | `<notebook>-tls` | Notebook namespace | **ORPHANED** - delete |
| **OpenShift Route** | `<notebook>` | Notebook namespace | **ORPHANED** - delete |
| **OAuth Client Secret** | `<notebook>-oauth-client` | Notebook namespace | **ORPHANED** - delete |
| **OAuth Config Secret** | `<notebook>-oauth-config` | Notebook namespace | **ORPHANED** - delete |
| **TLS Secret** | `<notebook>-tls` | Notebook namespace | **ORPHANED** - delete |
| **Base Service** | `<notebook>` (port 80) | Notebook namespace | **KEEP** - still needed |
| **ServiceAccount** | `<notebook>` | Notebook namespace | **KEEP** - needed for kube-rbac-proxy |
| **dockercfg Secret** | `<notebook>-dockercfg-*` | Notebook namespace | **KEEP** - auto-managed by OpenShift |

### Example: Resources Found in aadmin-created-namespace

**Pods (2/2 containers = oauth-proxy still present!):**
```
pod/codeserver251-0           2/2     Running
pod/codeserver252-0           2/2     Running
pod/jupyterdatascience251-0   2/2     Running
pod/jupyterdatascience252-0   2/2     Running
```

**Services:**
```
service/codeserver251               ClusterIP   80/TCP    # KEEP - base service
service/codeserver251-tls           ClusterIP   443/TCP   # DELETE - legacy oauth
service/codeserver252               ClusterIP   80/TCP    # KEEP
service/codeserver252-tls           ClusterIP   443/TCP   # DELETE
service/jupyterdatascience251       ClusterIP   80/TCP    # KEEP
service/jupyterdatascience251-tls   ClusterIP   443/TCP   # DELETE
service/jupyterdatascience252       ClusterIP   80/TCP    # KEEP
service/jupyterdatascience252-tls   ClusterIP   443/TCP   # DELETE
```

**Routes (all ORPHANED):**
```
route/codeserver251           → codeserver251-tls:oauth-proxy
route/codeserver252           → codeserver252-tls:oauth-proxy
route/jupyterdatascience251   → jupyterdatascience251-tls:oauth-proxy
route/jupyterdatascience252   → jupyterdatascience252-tls:oauth-proxy
```

**Secrets:**
```
codeserver251-oauth-client              Opaque           # DELETE
codeserver251-oauth-config              Opaque           # DELETE
codeserver251-tls                       kubernetes.io/tls # DELETE
codeserver251-dockercfg-zrcfj           kubernetes.io/dockercfg # KEEP - auto-managed
```

**OAuthClients (cluster-scoped):**
```
codeserver251-aadmin-created-namespace-oauth-client    # DELETE
codeserver252-aadmin-created-namespace-oauth-client    # DELETE
jupyterdatascience251-aadmin-created-namespace-oauth-client # DELETE
jupyterdatascience252-aadmin-created-namespace-oauth-client # DELETE
```

### Complete Cleanup Script

```bash
#!/bin/bash
# Cleanup script for orphaned RHOAI 2.x resources after migration
# Run AFTER notebooks have been migrated to inject-auth: true

NAMESPACE="aadmin-created-namespace"
NOTEBOOKS="codeserver251 codeserver252 jupyterdatascience251 jupyterdatascience252"

for NB in $NOTEBOOKS; do
    echo "Cleaning up $NB in $NAMESPACE..."
    
    # Delete OpenShift Route
    oc delete route $NB -n $NAMESPACE --ignore-not-found
    
    # Delete TLS Service
    oc delete svc ${NB}-tls -n $NAMESPACE --ignore-not-found
    
    # Delete OAuth secrets
    oc delete secret ${NB}-oauth-client ${NB}-oauth-config ${NB}-tls -n $NAMESPACE --ignore-not-found
    
    # Delete OAuthClient (cluster-scoped)
    oc delete oauthclient ${NB}-${NAMESPACE}-oauth-client --ignore-not-found
    
    echo "Done with $NB"
done

echo "Cleanup complete!"
```

### Why Controller Can't Clean These Up

1. **OpenShift Routes**: Controller lacks RBAC permissions (only get/list/watch)
2. **TLS Services**: Created by old controller, new controller doesn't know about them
3. **OAuth Secrets**: Same as TLS services
4. **OAuthClients**: Only cleaned up on notebook **deletion** (via finalizer), not during reconciliation

### Verification After Cleanup

```bash
# Should return nothing for each notebook
oc get route <NOTEBOOK> -n <NAMESPACE>
oc get svc <NOTEBOOK>-tls -n <NAMESPACE>
oc get secret <NOTEBOOK>-oauth-client -n <NAMESPACE>
oc get oauthclient <NOTEBOOK>-<NAMESPACE>-oauth-client

# Should still exist
oc get svc <NOTEBOOK> -n <NAMESPACE>           # Base service on port 80
oc get sa <NOTEBOOK> -n <NAMESPACE>            # ServiceAccount
oc get secret <NOTEBOOK>-dockercfg-* -n <NAMESPACE>  # Auto-managed
```

---

## Appendix: Commands Reference

### Operator Status

```bash
# Check CSV status
oc get csv -n redhat-ods-operator

# Check operator pods
oc get pods -n redhat-ods-operator

# Check operator logs
oc logs deployment/rhods-operator -n redhat-ods-operator --tail=100

# Check DSC status
oc get datasciencecluster -A -o wide

# Check DSCI status
oc get dsci -A -o wide
```

### Notebook Investigation

```bash
# List all notebooks with auth annotations
oc get notebooks -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,INJECT-AUTH:.metadata.annotations.notebooks\.opendatahub\.io/inject-auth,INJECT-OAUTH:.metadata.annotations.notebooks\.opendatahub\.io/inject-oauth'

# Get notebook details
oc get notebook <NAME> -n <NAMESPACE> -o yaml

# Check notebook annotations
oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.metadata.annotations}' | jq .

# Check notebook containers
oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.spec.template.spec.containers[*].name}'
```

### Routing Investigation

```bash
# List all HTTPRoutes
oc get httproutes -A

# Check HTTPRoute details
oc get httproute <NAME> -n <NAMESPACE> -o yaml

# List OpenShift Routes in namespace
oc get routes -n <NAMESPACE>

# Compare old Route vs new HTTPRoute
oc get route <NAME> -n <NAMESPACE> -o jsonpath='{.spec.to.name}:{.spec.port.targetPort}'
oc get httproute nb-<NAMESPACE>-<NAME> -n redhat-ods-applications -o jsonpath='{.spec.rules[0].backendRefs[0].name}:{.spec.rules[0].backendRefs[0].port}'
```

### Gateway and ReferenceGrants

```bash
# Check Gateway
oc get gateway -A

# Check ReferenceGrants
oc get referencegrants -A

# Check ReferenceGrant details
oc get referencegrant notebook-httproute-access -n <NAMESPACE> -o yaml
```

### Services Investigation

```bash
# List services in namespace
oc get svc -n <NAMESPACE>

# Check for kube-rbac-proxy service
oc get svc <NOTEBOOK>-kube-rbac-proxy -n <NAMESPACE>

# Check for legacy TLS service
oc get svc <NOTEBOOK>-tls -n <NAMESPACE>
```

### InstallPlan and Upgrade

```bash
# Check InstallPlan
oc get installplan -n redhat-ods-operator

# Get InstallPlan details
oc get installplan <NAME> -n redhat-ods-operator -o yaml

# Check what CSV replaces
oc get csv <NAME> -n redhat-ods-operator -o jsonpath='{.spec.replaces}'
```

### Cleanup Commands

```bash
# Delete old Route (manual - controller can't do this)
oc delete route <NOTEBOOK> -n <NAMESPACE>

# Delete TLS service
oc delete svc <NOTEBOOK>-tls -n <NAMESPACE>

# Delete OAuthClient
oc delete oauthclient <NOTEBOOK>-<NAMESPACE>-oauth-client

# Delete secrets
oc delete secret <NOTEBOOK>-tls -n <NAMESPACE>
oc delete secret <NOTEBOOK>-oauth-config -n <NAMESPACE>
```

### Debugging

```bash
# Check notebook controller logs
oc logs deployment/odh-notebook-controller-manager -n redhat-ods-applications --tail=100

# Check for errors
oc logs deployment/odh-notebook-controller-manager -n redhat-ods-applications --tail=200 | grep -i error

# Check webhook logs
oc logs deployment/notebook-controller-deployment -n redhat-ods-applications --tail=100

# Check pod events
oc describe pod <POD_NAME> -n <NAMESPACE> | tail -40

# Check HardwareProfiles
oc get hardwareprofiles -A
```

### Verification Commands

```bash
# Verify notebook is properly migrated
oc get notebook <NAME> -n <NAMESPACE> -o jsonpath='{.metadata.annotations.notebooks\.opendatahub\.io/inject-auth}'
# Should return: true

# Verify HTTPRoute points to kube-rbac-proxy
oc get httproute nb-<NAMESPACE>-<NAME> -n redhat-ods-applications -o jsonpath='{.spec.rules[0].backendRefs[0].name}'
# Should return: <NAME>-kube-rbac-proxy

# Verify old Route is deleted
oc get route <NAME> -n <NAMESPACE>
# Should return: Error from server (NotFound)

# Verify kube-rbac-proxy service exists
oc get svc <NAME>-kube-rbac-proxy -n <NAMESPACE>
# Should return service details
```

---

## Appendix: Case Study - Live Migration of medium-pytorch-gpu-later

This section documents a real migration performed on cluster `<CLUSTER_DOMAIN>` on 2026-02-03/05.

### Initial State (Pre-Migration)

**Workbench:** `auser-created-project/medium-pytorch-gpu-later`

```bash
$ oc get notebook medium-pytorch-gpu-later -n auser-created-project -o jsonpath='
Annotations:
  inject-auth: {.metadata.annotations.notebooks\.opendatahub\.io/inject-auth}
  inject-oauth: {.metadata.annotations.notebooks\.opendatahub\.io/inject-oauth}
Finalizers: {.metadata.finalizers}
Containers: {.spec.template.spec.containers[*].name}
Replicas: {.status.readyReplicas}
'
```

**Output:**
```
Annotations:
  inject-auth: 
  inject-oauth: true
Finalizers: ["notebook-oauth-client-finalizer.opendatahub.io","notebook.opendatahub.io/httproute-cleanup","notebook.opendatahub.io/referencegrant-cleanup"]
Containers: medium-pytorch-gpu-later oauth-proxy
Replicas: 1
```

**Analysis:**
- `inject-oauth: true` → 2.x auth style
- `inject-auth:` → not set (no 3.x auth)
- 2 containers: notebook + oauth-proxy
- Running (1 replica)
- Has legacy OAuth finalizer plus new 3.x finalizers (added by upgraded controller)

### Phase 1: Update Annotations (No Restart Required)

**Step 1: Add inject-auth annotation**
```bash
$ oc annotate notebook medium-pytorch-gpu-later -n auser-created-project \
    notebooks.opendatahub.io/inject-auth="true" --overwrite
```
**Output:**
```
notebook.kubeflow.org/medium-pytorch-gpu-later annotated
```

**Step 2: Remove legacy annotations**
```bash
$ oc annotate notebook medium-pytorch-gpu-later -n auser-created-project \
    notebooks.opendatahub.io/inject-oauth- \
    notebooks.opendatahub.io/oauth-logout-url-
```
**Output:**
```
notebook.kubeflow.org/medium-pytorch-gpu-later annotated
```

**Step 3: Remove OAuth finalizer (keep others)**
```bash
$ CURRENT_FINALIZERS=$(oc get notebook medium-pytorch-gpu-later -n auser-created-project \
    -o jsonpath='{.metadata.finalizers}')
$ echo "Current: $CURRENT_FINALIZERS"
Current: ["notebook-oauth-client-finalizer.opendatahub.io","notebook.opendatahub.io/httproute-cleanup","notebook.opendatahub.io/referencegrant-cleanup","notebook.opendatahub.io/kube-rbac-proxy-cleanup"]

$ NEW_FINALIZERS=$(echo "$CURRENT_FINALIZERS" | jq -c '[.[] | select(. != "notebook-oauth-client-finalizer.opendatahub.io")]')
$ echo "New: $NEW_FINALIZERS"
New: ["notebook.opendatahub.io/httproute-cleanup","notebook.opendatahub.io/referencegrant-cleanup","notebook.opendatahub.io/kube-rbac-proxy-cleanup"]

$ oc patch notebook medium-pytorch-gpu-later -n auser-created-project \
    --type='merge' -p "{\"metadata\":{\"finalizers\":$NEW_FINALIZERS}}"
```
**Output:**
```
notebook.kubeflow.org/medium-pytorch-gpu-later patched
```

### State After Phase 1

```bash
$ oc get notebook medium-pytorch-gpu-later -n auser-created-project -o jsonpath='
Annotations:
  inject-auth: {.metadata.annotations.notebooks\.opendatahub\.io/inject-auth}
  inject-oauth: {.metadata.annotations.notebooks\.opendatahub\.io/inject-oauth}
Finalizers: {.metadata.finalizers}
Containers: {.spec.template.spec.containers[*].name}
Replicas: {.status.readyReplicas}
'
```
**Output:**
```
Annotations:
  inject-auth: true
  inject-oauth: 
Finalizers: ["notebook.opendatahub.io/httproute-cleanup","notebook.opendatahub.io/referencegrant-cleanup","notebook.opendatahub.io/kube-rbac-proxy-cleanup"]
Containers: medium-pytorch-gpu-later oauth-proxy
Replicas: 1
```

**Controller Immediate Reactions (No Pod Restart):**

The controller detected the annotation change and immediately created resources:

```bash
$ oc get httproute nb-auser-created-project-medium-pytorch-gpu-later -n redhat-ods-applications \
    -o jsonpath='Backend: {.spec.rules[0].backendRefs[0].name}:{.spec.rules[0].backendRefs[0].port}'
```
**Output:**
```
Backend: medium-pytorch-gpu-later-kube-rbac-proxy:8443
```

```bash
$ oc get svc medium-pytorch-gpu-later-kube-rbac-proxy -n auser-created-project
```
**Output:**
```
NAME                                       TYPE        CLUSTER-IP     EXTERNAL-IP   PORT(S)    AGE
medium-pytorch-gpu-later-kube-rbac-proxy   ClusterIP   172.30.17.87   <none>        8443/TCP   56s
```

### Transitional State (Phase 1 Complete, Pending Restart)

| Component | Status | Notes |
|-----------|--------|-------|
| `inject-auth` annotation | ✅ `true` | 3.x auth enabled |
| `inject-oauth` annotation | ✅ removed | Legacy disabled |
| OAuth finalizer | ✅ removed | Won't try cleanup on delete |
| New finalizers | ✅ present | httproute, referencegrant, kube-rbac-proxy |
| kube-rbac-proxy Service | ✅ created | Port 8443 |
| HTTPRoute | ✅ updated | Points to kube-rbac-proxy:8443 |
| **Pod containers** | ⚠️ Still old | `oauth-proxy` only, no `kube-rbac-proxy` |

### URL Behavior in Transitional State

**Gateway URL (shown in Dashboard):**
```
https://data-science-gateway.apps.<CLUSTER_DOMAIN>/notebook/auser-created-project/medium-pytorch-gpu-later
```
**Result:** `no healthy upstream` (changed from HTTP 500 before Phase 1)

**Explanation:** HTTPRoute now correctly points to `kube-rbac-proxy:8443` service, but the pod doesn't have the kube-rbac-proxy container yet. The service has no healthy endpoints to route to.

**Old Route URL:**
```
https://medium-pytorch-gpu-later-auser-created-project.apps.<CLUSTER_DOMAIN>/notebook/auser-created-project/medium-pytorch-gpu-later
```
**Result:** Still works - oauth-proxy container is still running and accepting traffic.

### To Complete Migration: Restart Workbench

```bash
# Stop workbench
oc patch notebook medium-pytorch-gpu-later -n auser-created-project \
  --type='merge' -p '{"metadata":{"annotations":{"kubeflow-resource-stopped":"yes"}}}'

# Wait for pod to terminate
oc wait --for=delete pod -l notebook-name=medium-pytorch-gpu-later -n auser-created-project --timeout=60s

# Start workbench
oc patch notebook medium-pytorch-gpu-later -n auser-created-project \
  --type='merge' -p '{"metadata":{"annotations":{"kubeflow-resource-stopped":null}}}'
```

**After restart:**
- Pod will have 3 containers: `notebook`, `oauth-proxy`, `kube-rbac-proxy`
- Gateway URL will work (kube-rbac-proxy receives traffic)
- Old Route URL will still work (oauth-proxy still present)

### Key Observations

1. **Annotation changes are safe on running workbenches** - no restart triggered
2. **Controller immediately creates kube-rbac-proxy resources** - Service, updates HTTPRoute
3. **Gateway URL transitions from 500 → "no healthy upstream"** - indicates progress (correct routing, missing backend)
4. **Old Route continues to work** - users can keep working during migration
5. **kube-rbac-proxy is added alongside oauth-proxy** - not a replacement, both coexist after restart

---

## Related Jira Issues

**Label:** All migration-related bugs should use label `rhoai-3.3_migration` (no spaces).

| Issue | Summary | Label |
|-------|---------|-------|
| **RHOAIENG-48747** | Dashboard shows broken Gateway URLs for unmigrated 2.x workbenches after upgrade to 3.x | `rhoai-3.3_migration` |
| **RHOAIENG-39253** | Port mismatch in unauthenticated HTTPRoute | |
| **RHOAIENG-38009** | HTTPRoute hijacking vulnerability | |
| **RHOAIENG-38217** | ReferenceGrant too permissive | |
| **RHOAIENG-31693** | Incorrect commit hash display | |
| **RHAIRFE-1251** | RFE for admin compatibility checking tool | |

---

## Document History

| Date       | Author     | Changes                                                                                                                                                                                                 |
|------------|------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 2026-02-05 | Jiri Daněk | Initial investigation and documentation                                                                                                                                                                 |
| 2026-02-05 | Jiri Daněk | Added Orphaned Resources Reference section with cleanup scripts                                                                                                                                         |
| 2026-02-05 | Jiri Daněk | Added real-world upgrade behavior findings (500 errors, working URLs, Dashboard UX issue)                                                                                                               |
| 2026-02-05 | Jiri Daněk | Added Case Study appendix: live migration of medium-pytorch-gpu-later with exact commands and outputs                                                                                                   |
| 2026-02-05 | Jiri Daněk | Sanitized document and git history: replaced internal cluster domain with `<CLUSTER_DOMAIN>` using `git filter-repo --replace-text <(echo "xxxyyyzzz==><CLUSTER_DOMAIN>") --refs HEAD~10..HEAD --force` |
