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
