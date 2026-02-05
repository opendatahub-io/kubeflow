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
