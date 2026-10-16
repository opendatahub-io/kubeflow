#!/usr/bin/env bash

# https://vaneyckt.io/posts/safer_bash_scripts_with_set_euxo_pipefail/
set -Eeuxo pipefail

echo "Running the ${0} setup"

TEST_NAMESPACE="odh-notebook-controller-system"

# Following variables are optional - if not set, the default values in relevant params.env
# will be used for both images. As such, if you want to run tests against your custom changes,
# be sure to perform a docker build and set these variables accordingly!
ODH_NOTEBOOK_CONTROLLER_IMAGE="${ODH_NOTEBOOK_CONTROLLER_IMAGE:-}"
KF_NOTEBOOK_CONTROLLER="${KF_NOTEBOOK_CONTROLLER:-}"


if test -n "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"; then
    IFS=':' read -r -a CTRL_IMG <<< "${ODH_NOTEBOOK_CONTROLLER_IMAGE}"
    export IMG="${CTRL_IMG[0]}"
    export TAG="${CTRL_IMG[1]}"
fi

if test -n "${KF_NOTEBOOK_CONTROLLER}"; then
    IFS=':' read -r -a KF_NBC_IMG <<< "${KF_NOTEBOOK_CONTROLLER}"
    export KF_IMG="${KF_NBC_IMG[0]}"
    export KF_TAG="${KF_NBC_IMG[1]}"
fi

export K8S_NAMESPACE="${TEST_NAMESPACE}"

# From now on we want to be sure that undeploy and testing project deletion are called

# Directory for collecting logs - created early so streaming can start
LOGS_DIR="${ARTIFACT_DIR:-/tmp}/e2e-logs-$(date +%Y%m%d_%H%M%S)"

# Arrays to track background log streaming processes
LOG_STREAM_PIDS=()

# Start streaming logs from a deployment to a file
# This captures logs continuously, surviving pod restarts
function start_log_streaming() {
    local deployment_name="$1"
    local log_file="$2"

    echo "Starting log streaming for ${deployment_name}..."
    # Use 'oc logs -f' with --ignore-errors to continue streaming even when pods restart
    # The loop ensures we reconnect if the stream breaks due to pod deletion
    (
        while true; do
            oc logs -n "${TEST_NAMESPACE}" "deployment/${deployment_name}" \
                --all-containers=true --timestamps -f 2>&1 || true
            # Small delay before reconnecting to avoid tight loop if deployment doesn't exist
            sleep 2
        done
    ) >> "${log_file}" 2>&1 &

    LOG_STREAM_PIDS+=($!)
    echo "Started log streaming for ${deployment_name} (PID: $!)"
}

# Start streaming logs from all notebook pods (watches for new pods)
function start_notebook_log_streaming() {
    local log_dir="$1"

    echo "Starting notebook pod log streaming..."
    (
        # Track pod name -> streaming process PID
        declare -A streaming_pods

        while true; do
            # Get current notebook pods
            for pod in $(oc get pods -n "${TEST_NAMESPACE}" -l notebook-name -o name 2>/dev/null); do
                local podname
                podname=$(basename "$pod")

                # Get existing streaming PID for this pod (if any)
                local existing_pid="${streaming_pods[$podname]:-}"

                # Start streaming if no existing process, or if existing process died
                # (process dies when pod is deleted; we need to restart when pod is recreated)
                if [[ -z "$existing_pid" ]] || ! kill -0 "$existing_pid" 2>/dev/null; then
                    # Start new streaming process for this pod
                    (
                        oc logs -n "${TEST_NAMESPACE}" "$pod" \
                            --all-containers=true --timestamps -f 2>&1 || true
                    ) >> "${log_dir}/${podname}.log" 2>&1 &

                    # Track the streaming process PID
                    streaming_pods[$podname]=$!
                fi
            done
            sleep 5
        done
    ) &

    LOG_STREAM_PIDS+=($!)
    echo "Started notebook pod log streaming (PID: $!)"
}

# Stop all log streaming processes
function stop_log_streaming() {
    echo "Stopping log streaming processes..."
    for pid in "${LOG_STREAM_PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            # Stop child log streams spawned by the watcher (if any)
            # This prevents orphaned processes that would continue running after parent is killed
            if command -v pkill >/dev/null 2>&1; then
                pkill -P "$pid" 2>/dev/null || true
            elif command -v pgrep >/dev/null 2>&1; then
                child_pids=$(pgrep -P "$pid" || true)
                [ -n "$child_pids" ] && kill $child_pids 2>/dev/null || true
            fi
            # Then kill the parent process
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    LOG_STREAM_PIDS=()
}

# Collect final snapshot of logs and cluster state
function collect_final_logs() {
    echo "Collecting final log snapshot and cluster state..."

    # Ensure log directories exist (may not exist if failure occurred before streaming started)
    mkdir -p "${LOGS_DIR}/controllers"
    mkdir -p "${LOGS_DIR}/notebooks"

    # Collect any remaining logs from current pods (in case streaming missed something)
    echo "Collecting final controller logs..."
    oc logs -n "${TEST_NAMESPACE}" deployment/notebook-controller-deployment \
        --all-containers=true --timestamps \
        >> "${LOGS_DIR}/controllers/notebook-controller.log" 2>&1 || true

    oc logs -n "${TEST_NAMESPACE}" deployment/odh-notebook-controller-manager \
        --all-containers=true --timestamps \
        >> "${LOGS_DIR}/controllers/odh-notebook-controller.log" 2>&1 || true

    # Collect final notebook pod logs
    echo "Collecting final notebook pod logs..."
    for pod in $(oc get pods -n "${TEST_NAMESPACE}" -l notebook-name -o name 2>/dev/null); do
        local podname
        podname=$(basename "$pod")
        oc logs -n "${TEST_NAMESPACE}" "$pod" --all-containers=true --timestamps \
            >> "${LOGS_DIR}/notebooks/${podname}.log" 2>&1 || true
    done

    # Describe all pods - shows status, conditions, and events
    echo "Collecting pod descriptions..."
    oc describe pods -n "${TEST_NAMESPACE}" \
        > "${LOGS_DIR}/pod-descriptions.txt" 2>&1 || true

    # Get namespace events sorted by time - includes events for deleted resources
    echo "Collecting namespace events..."
    oc get events -n "${TEST_NAMESPACE}" --sort-by='.lastTimestamp' \
        > "${LOGS_DIR}/namespace-events.txt" 2>&1 || true

    # Get StatefulSet status for debugging
    echo "Collecting StatefulSet status..."
    oc get statefulsets -n "${TEST_NAMESPACE}" -o yaml \
        > "${LOGS_DIR}/statefulsets.yaml" 2>&1 || true

    echo "=============================================="
    echo "Logs collected in: ${LOGS_DIR}"
    echo "Contents:"
    ls -la "${LOGS_DIR}/" 2>/dev/null || true
    ls -la "${LOGS_DIR}/controllers/" 2>/dev/null || true
    ls -la "${LOGS_DIR}/notebooks/" 2>/dev/null || true
    echo "=============================================="
}

function cleanup() {
    # Capture the exit code from the test run
    local test_exit_code=$?
    local ret_code=0

    # Stop log streaming processes first
    stop_log_streaming

    # Collect final logs before cleanup if tests failed (pods still exist at this point)
    if [ ${test_exit_code} -ne 0 ]; then
        echo "Tests failed (exit code: ${test_exit_code}), collecting final logs for debugging..."
        collect_final_logs
    fi

    echo "Performing deployment cleanup of the ${0}"
    make undeploy || {
        echo "Warning [cleanup]: make undeploy failed, continuing with project deletion!"
        ret_code=1
    }
    oc delete --wait=true --ignore-not-found=true project "${TEST_NAMESPACE}" || {
        echo "Warning [cleanup]: project deletion failed!"
        ret_code=1
    }
    return ${ret_code}
}
trap cleanup EXIT

# assure that the project is deleted on the cluster before running the tests
# Note: We only delete the project here, not calling cleanup() to avoid unnecessary make undeploy
oc delete --wait=true --ignore-not-found=true project "${TEST_NAMESPACE}" || echo "Warning [pre-test-cleanup]: project deletion failed!"

# setup and deploy the controller
oc new-project "${TEST_NAMESPACE}"

# deploy the controllers
make deploy

# Create log directories and start streaming logs before running tests
# This ensures we capture logs even when pods are restarted during tests
mkdir -p "${LOGS_DIR}/controllers"
mkdir -p "${LOGS_DIR}/notebooks"

echo "Starting continuous log streaming to capture logs across pod restarts..."
start_log_streaming "notebook-controller-deployment" "${LOGS_DIR}/controllers/notebook-controller.log"
start_log_streaming "odh-notebook-controller-manager" "${LOGS_DIR}/controllers/odh-notebook-controller.log"
start_notebook_log_streaming "${LOGS_DIR}/notebooks"

# Give log streaming a moment to start
sleep 2

# run e2e tests
make e2e-test
