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
