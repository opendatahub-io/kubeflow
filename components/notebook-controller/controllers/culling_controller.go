package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/kubeflow/kubeflow/components/notebook-controller/api/v1beta1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/metrics"
)

// The constants with name 'DEFAULT_{ENV_Var}' are the default values to be
// used, if the respective ENV vars are not present.
// All the time numbers correspond to minutes.

const DEFAULT_CULL_IDLE_TIME = "1440" // One day
const DEFAULT_IDLENESS_CHECK_PERIOD = "1"
const DEFAULT_ENABLE_CULLING = "false"
const DEFAULT_CLUSTER_DOMAIN = "cluster.local"
const DEFAULT_DEV = "false"

var CULL_IDLE_TIME = 0
var ENABLE_CULLING = false
var IDLENESS_CHECK_PERIOD = 0
var CLUSTER_DOMAIN = ""
var DEV = false

// When a Resource should be stopped/culled, then the controller should add this
// annotation in the Resource's Metadata. Then, inside the reconcile loop,
// the controller must check if this annotation is set and then apply the
// respective culling logic for that Resource. The value of the annotation will
// be a timestamp of when the Resource was stopped/culled.
//
// In case of Notebooks, the controller will reduce the replicas to 0 if
// this annotation is set. If it's not set, then it will make the replicas 1.
const STOP_ANNOTATION = "kubeflow-resource-stopped"
const LAST_ACTIVITY_ANNOTATION = "notebooks.kubeflow.org/last-activity"
const LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION = "notebooks.kubeflow.org/last_activity_check_timestamp"

const (
	KERNEL_EXECUTION_STATE_IDLE     = "idle"
	KERNEL_EXECUTION_STATE_BUSY     = "busy"
	KERNEL_EXECUTION_STATE_STARTING = "starting"
)

// Each kernel of the Notebook Server has a status.
// KernelStatus struct:
type KernelStatus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	LastActivity   string `json:"last_activity"`
	ExecutionState string `json:"execution_state"`
	Connections    int    `json:"connections"`
}

// CullingReconciler : Type of a reconciler that will be culling idle notebooks
type CullingReconciler struct {
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	Metrics *metrics.Metrics
}

// Each terminal of the Notebook Server has a status.
// TerminalStatus struct:
type TerminalStatus struct {
	Name         string `json:"name"`
	LastActivity string `json:"last_activity"`
}

func (r *CullingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("culler", req.NamespacedName)
	log.Info("Reconciliation loop started")

	instance := &v1beta1.Notebook{}
	err := r.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil && apierrs.IsNotFound(err) {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, err
	}

	// Won't check for culling when a Notebook is being culled/stopped
	// Remove LAST_ACTIVITY_ANNOTATION and LAST_ACTIVITY_TIMESTAMP_CHECK
	// annotations for CR objects
	if StopAnnotationIsSet(instance.ObjectMeta) {
		log.Info("Notebook is already stopping")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
				return err
			}
			removeAnnotations(&instance.ObjectMeta, r.Log)
			return r.Update(ctx, instance)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Ensure that the underlying Notebook Pod exists
	foundPod := &corev1.Pod{}
	err = r.Get(ctx, types.NamespacedName{Name: instance.Name + "-0", Namespace: instance.Namespace}, foundPod)
	if err != nil && apierrs.IsNotFound(err) {
		log.Info("Pod not found...Will remove last-activity annotation...")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
				return err
			}
			removeAnnotations(&instance.ObjectMeta, r.Log)
			return r.Update(ctx, instance)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Initialize culling (last-activity and last-activity-check-timestamp) annotations
	if !annotationsExist(instance) {
		log.Info("No annotations found. Initializing last-activity and last-activity-check-timestamp annotations")
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
				return err
			}
			initializeAnnotations(&instance.ObjectMeta)
			return r.Update(ctx, instance)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if culling period has passed (IDLENESS_CHECK_PERIOD ~ default 1 min)
	if !cullingCheckPeriodHasPassed(instance.ObjectMeta, r.Log) {
		log.Info("Not enough time has passed. Won't check for culling.")
		return ctrl.Result{RequeueAfter: getRequeueTime()}, nil
	}

	// Query notebook activity from kernels/terminals API
	// This is done outside the retry loop as it doesn't depend on the CR state
	nm, ns := instance.Name, instance.Namespace
	log.Info("Updating the last-activity annotation. Checking /api/kernels")
	kernels := getNotebookApiKernels(nm, ns, r.Log)
	log.Info("last-activity annotation exists. Checking /api/terminals")
	terminals := getNotebookApiTerminals(nm, ns, r.Log)

	// Update annotations with retry on conflict to handle concurrent updates
	// This consolidates all annotation updates into a single atomic operation
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch the latest version of the Notebook CR
		if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
			return err
		}

		// Update the LAST_ACTIVITY_ANNOTATION based on kernels/terminals activity
		updateTimestampFromKernelsActivity(&instance.ObjectMeta, kernels, r.Log)
		updateTimestampFromTerminalsActivity(&instance.ObjectMeta, terminals, r.Log)

		// Update the LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION
		updateLastCullingCheckTimestampAnnotation(&instance.ObjectMeta, r.Log)

		// Check if the Notebook needs to be stopped and set stop annotation
		if notebookIsIdle(instance.ObjectMeta, r.Log) {
			log.Info(fmt.Sprintf(
				"Notebook %s/%s needs culling. Updating Notebook CR Annotations...",
				instance.Namespace, instance.Name))

			// Set Stop Annotation to the Notebook CR
			setStopAnnotation(&instance.ObjectMeta, r.Metrics, r.Log)
		}

		// Single consolidated update with all annotation changes
		return r.Update(ctx, instance)
	})

	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: getRequeueTime()}, nil
}

// This function ensures that we run the culling checks every CULLING_CHECK_PERIOD
// even if in the meantime an update/create/delete event occurs for a Notebook CR.
func cullingCheckPeriodHasPassed(meta metav1.ObjectMeta, log logr.Logger) bool {
	if _, ok := meta.GetAnnotations()[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION]; !ok {
		log.Info("No last-activity-check-timestamp found in the CR. Won't check for culling")
		return false
	}
	storedTimestamp, _ := time.Parse(time.RFC3339, meta.Annotations[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION])
	nextCullingCheck := storedTimestamp.Add(getRequeueTime())
	currentTime := time.Now()

	return nextCullingCheck.Before(currentTime)
}

// Culling Logic
func notebookIsIdle(meta metav1.ObjectMeta, log logr.Logger) bool {
	// Being idle means that the Notebook can be culled/stopped
	if meta.GetAnnotations() != nil {
		if StopAnnotationIsSet(meta) {
			log.Info("Notebook is already stopping")
			return false
		}
		// Read the current LAST_ACTIVITY_ANNOTATION
		tempLastActivity := meta.GetAnnotations()[LAST_ACTIVITY_ANNOTATION]
		LastActivity, err := time.Parse(time.RFC3339, tempLastActivity)
		if err != nil {
			log.Error(err, "Error parsing last-activity time")
			return false
		}

		timeCap := LastActivity.Add(time.Duration(CULL_IDLE_TIME) * time.Minute)
		if time.Now().After(timeCap) {
			return true
		}
	}
	return false
}

func getNotebookResourceResponse(nm, ns string, resource string, log logr.Logger) *http.Response {
	client := &http.Client{
		Timeout: time.Second * 10,
	}

	domain := GetEnvDefault("CLUSTER_DOMAIN", DEFAULT_CLUSTER_DOMAIN)
	url := fmt.Sprintf(
		"http://%s.%s.svc.%s/notebook/%s/%s/api/%s",
		nm, ns, domain, ns, nm, resource)
	if GetEnvDefault("DEV", DEFAULT_DEV) != "false" {
		url = fmt.Sprintf(
			"http://localhost:8001/api/v1/namespaces/%s/services/%s:http-%s/proxy/notebook/%s/%s/api/%s",
			ns, nm, nm, ns, nm, resource)
	}

	resp, err := client.Get(url)
	if err != nil {
		log.Error(err, fmt.Sprintf("Error talking to %s", url))
		return nil
	}

	// Decode the body

	if resp.StatusCode != 200 {
		log.Info(fmt.Sprintf(
			"Warning: GET to %s: %d", url, resp.StatusCode))
		return nil
	}

	return resp
}

// Kernels culling logic
func getNotebookApiKernels(nm, ns string, log logr.Logger) []KernelStatus {
	// Get the Kernels' status from the Server's `/api/kernels` endpoint
	resp := getNotebookResourceResponse(nm, ns, "kernels", log)
	if resp == nil {
		return nil
	}

	var kernels []KernelStatus

	defer resp.Body.Close()
	err := json.NewDecoder(resp.Body).Decode(&kernels)
	if err != nil {
		log.Error(err, "Error parsing JSON response for Notebook API Kernels.")
		return nil
	}

	return kernels
}

// Terminals culling logic
func getNotebookApiTerminals(nm, ns string, log logr.Logger) []TerminalStatus {
	// Get the Terminals' status from the Server's `/api/terminals` endpoint
	resp := getNotebookResourceResponse(nm, ns, "terminals", log)
	if resp == nil {
		return nil
	}

	var terminals []TerminalStatus

	defer resp.Body.Close()
	err := json.NewDecoder(resp.Body).Decode(&terminals)
	if err != nil {
		log.Error(err, "Error parsing JSON response for Notebook API terminals.")
		return nil
	}

	return terminals
}

func allKernelsAreIdle(kernels []KernelStatus, log logr.Logger) bool {
	// Iterate on the list of kernels' status.
	// If all kernels are on execution_state=idle then this function returns true.
	log.Info("Examining if all kernels are idle")
	for i := 0; i < len(kernels); i++ {
		if kernels[i].ExecutionState != KERNEL_EXECUTION_STATE_IDLE {
			log.Info("Not all kernels are idle")
			return false
		}
	}
	log.Info("All kernels are idle")
	return true
}

func getNotebookRecentTime(t []string, api string, log logr.Logger) string {
	// Checking for the most recent resource last_activity. The LAST_ACTIVITY_ANNOTATION
	// should be the most recent resource last-activity among the kernels and terminals.
	recentTime, err := time.Parse(time.RFC3339, t[0])
	if err != nil {
		log.Error(err, "Error parsing the last-activity from the /api/kernels")
		return ""
	}

	for i := 1; i < len(t); i++ {
		kernelLastActivity, err := time.Parse(time.RFC3339, t[i])
		if err != nil {
			log.Error(err, "Error parsing the last-activity from the %s", api)
			return ""
		}
		if kernelLastActivity.After(recentTime) {
			recentTime = kernelLastActivity
		}
	}
	return recentTime.Format(time.RFC3339)
}

func compareAnnotationTimeToResource(meta *metav1.ObjectMeta, resourceTime string, log logr.Logger) bool {

	annotationTime, err := time.Parse(time.RFC3339, meta.GetAnnotations()[LAST_ACTIVITY_ANNOTATION])
	if err != nil {
		log.Error(err, "Error parsing the last-activity from the notebook CRD")
		return false
	}
	t, err := time.Parse(time.RFC3339, resourceTime)
	if err != nil {
		log.Error(err, "Error parsing the last-activity from the resource return time")
		return false
	}
	// If the resource time is less than the annotation time on the resource return false
	if annotationTime.After(t) {
		return false
	}

	return true
}

func updateTimestampFromKernelsActivity(meta *metav1.ObjectMeta, kernels []KernelStatus, log logr.Logger) bool {

	if kernels == nil || len(kernels) == 0 {
		log.Info("Notebook has no kernels. Will not update last-activity")
		return false
	}

	if !allKernelsAreIdle(kernels, log) {
		// At least one kernel is "busy" so the last-activity annotation should
		// should be the current time.
		t := createTimestamp()
		log.Info(fmt.Sprintf("Found a busy kernel. Updating the last-activity to %s", t))

		meta.Annotations[LAST_ACTIVITY_ANNOTATION] = t
		return false
	}

	arr := make([]string, len(kernels))
	for i := 0; i < len(kernels); i++ {
		arr[i] = kernels[i].LastActivity
	}

	t := getNotebookRecentTime(arr, "api/kernels", log)
	log.Info(fmt.Sprintf("Comparing api/kernels last_activity time to current notebook annotation time"))
	if t == "" || !compareAnnotationTimeToResource(meta, t, log) {
		return false
	}

	meta.Annotations[LAST_ACTIVITY_ANNOTATION] = t
	log.Info(fmt.Sprintf("Successfully updated last-activity from latest kernel action, %s", t))
	return true
}

func updateTimestampFromTerminalsActivity(meta *metav1.ObjectMeta, terminals []TerminalStatus, log logr.Logger) bool {
	// check the latest_activity timestamp from the `api/terminals` jupyter api. also
	// check this timestamp against the current annotation timestamp to ensure we are not
	// going backwards in time.

	if terminals == nil || len(terminals) == 0 {
		log.Info("Notebook has no terminals. Will not update last-activity")
		return false
	}

	arr := make([]string, len(terminals))
	for i := 0; i < len(terminals); i++ {
		arr[i] = terminals[i].LastActivity
	}

	t := getNotebookRecentTime(arr, "api/terminals", log)
	log.Info(fmt.Sprintf("Comparing api/terminals last_activity time to current notebook annotation time"))
	if t == "" || !compareAnnotationTimeToResource(meta, t, log) {
		return false
	}

	meta.Annotations[LAST_ACTIVITY_ANNOTATION] = t
	log.Info(fmt.Sprintf("Successfully updated last-activity from latest terminal action, %s", t))
	return true
}

func updateLastCullingCheckTimestampAnnotation(meta *metav1.ObjectMeta, log logr.Logger) {
	t := createTimestamp()
	if _, ok := meta.GetAnnotations()[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION]; !ok {
		log.Info("No last-activity-check-timestamp annotation found. Will not update")
	}
	meta.Annotations[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION] = t
	log.Info("Successfully updated last-activity-check-timestamp annotation")

}

func annotationsExist(instance *v1beta1.Notebook) bool {
	meta := instance.ObjectMeta
	if metav1.HasAnnotation(meta, LAST_ACTIVITY_ANNOTATION) &&
		metav1.HasAnnotation(meta, LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION) {
		return true
	}
	return false
}

func initializeAnnotations(meta *metav1.ObjectMeta) {
	if len(meta.GetAnnotations()) == 0 {
		meta.SetAnnotations(map[string]string{})
	}
	t := createTimestamp()
	meta.Annotations[LAST_ACTIVITY_ANNOTATION] = t
	meta.Annotations[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION] = t
}

func removeAnnotations(meta *metav1.ObjectMeta, log logr.Logger) {
	if meta == nil {
		log.Info("Error: Metadata is Nil. Can't remove Annotations")
		return
	}

	if _, ok := meta.GetAnnotations()[LAST_ACTIVITY_ANNOTATION]; ok {
		log.Info("Removing last-activity annotation")
		delete(meta.GetAnnotations(), LAST_ACTIVITY_ANNOTATION)
	}
	if _, ok := meta.GetAnnotations()[LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION]; ok {
		log.Info("Removing last-activity-check-timestamp annotation")
		delete(meta.GetAnnotations(), LAST_ACTIVITY_CHECK_TIMESTAMP_ANNOTATION)
	}
}

// Stop Annotation handling functions
func setStopAnnotation(meta *metav1.ObjectMeta, m *metrics.Metrics, log logr.Logger) {
	if meta == nil {
		log.Info("Error: Metadata is Nil. Can't set Annotations")
		return
	}

	log.Info("Setting stop timestamp annotation")
	t := time.Now()
	if len(meta.GetAnnotations()) == 0 {
		meta.SetAnnotations(map[string]string{})
	}
	meta.Annotations[STOP_ANNOTATION] = t.Format(time.RFC3339)

	if m != nil {
		m.NotebookCullingCount.WithLabelValues(meta.Namespace, meta.Name).Inc()
		m.NotebookCullingTimestamp.WithLabelValues(meta.Namespace, meta.Name).Set(float64(t.Unix()))
	}
}

func StopAnnotationIsSet(meta metav1.ObjectMeta) bool {
	if meta.GetAnnotations() == nil {
		return false
	}
	if metav1.HasAnnotation(meta, STOP_ANNOTATION) {
		return true
	}
	return false
}

// Some Utility Functions
func GetEnvDefault(variable string, defaultVal string) string {
	envVar := os.Getenv(variable)
	if len(envVar) == 0 {
		return defaultVal
	}
	return envVar
}

// Time / Frequency Utility functions
func createTimestamp() string {
	now := time.Now()
	return now.Format(time.RFC3339)
}

func getRequeueTime() time.Duration {
	// The frequency in which we check if the Pod needs culling
	// Uses ENV var: IDLENESS_CHECK_PERIOD
	return time.Duration(IDLENESS_CHECK_PERIOD) * time.Minute
}

func initGlobalVars() error {
	log := logf.Log.WithName("Culler")

	devMode := GetEnvDefault("DEV", DEFAULT_DEV)
	if devMode == "true" {
		DEV = true
	}

	idleTime := GetEnvDefault("CULL_IDLE_TIME", DEFAULT_CULL_IDLE_TIME)
	realIdleTime, err := strconv.Atoi(idleTime)
	if err != nil {
		log.Info(fmt.Sprintf(
			"CULL_IDLE_TIME should be Int. Got %s instead. Using default value.",
			idleTime))
		realIdleTime, _ = strconv.Atoi(DEFAULT_CULL_IDLE_TIME)
	}
	CULL_IDLE_TIME = realIdleTime

	enableCulling := GetEnvDefault("ENABLE_CULLING", DEFAULT_ENABLE_CULLING)
	if enableCulling == "true" {
		ENABLE_CULLING = true
	}

	CLUSTER_DOMAIN = GetEnvDefault("CLUSTER_DOMAIN", DEFAULT_CLUSTER_DOMAIN)

	cullPeriod := GetEnvDefault("IDLENESS_CHECK_PERIOD", DEFAULT_IDLENESS_CHECK_PERIOD)
	period, err := strconv.Atoi(cullPeriod)
	if err != nil {
		return err
	}
	IDLENESS_CHECK_PERIOD = period

	return nil
}

// SetupWithManager : Add the culling controller to the manager
func (r *CullingReconciler) SetupWithManager(mgr ctrl.Manager) error {

	log := r.Log.WithValues("Culler", "setup")

	if err := initGlobalVars(); err != nil {
		log.Error(err, "Could not initialize the global variables")
		return err
	}

	controller := ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Notebook{}).
		Named("Culler")

	err := controller.Complete(r)
	if err != nil {
		return err
	}
	return nil
}
