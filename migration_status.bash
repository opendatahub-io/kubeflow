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
