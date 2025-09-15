package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func creationTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	notebooksForSelectedDeploymentMode := notebooksForScenario(testCtx.testNotebooks, deploymentMode)
	for _, nbContext := range notebooksForSelectedDeploymentMode {
		// prepend Notebook name to every subtest
		t.Run(nbContext.nbObjectMeta.Name, func(t *testing.T) {
			t.Run("Creation of Notebook instance", func(t *testing.T) {
				err = testCtx.testNotebookCreation(nbContext)
				require.NoError(t, err, "error creating Notebook object ")
			})
			t.Run("Notebook Route Validation", func(t *testing.T) {
				if deploymentMode == ServiceMesh {
					t.Skipf("Skipping as it's not relevant for Service Mesh scenario")
				}
				err = testCtx.testNotebookRouteCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Route for Notebook ")
			})

			t.Run("Notebook Network Policies Validation", func(t *testing.T) {
				err = testCtx.testNetworkPolicyCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Network Policies for Notebook ")
			})

			t.Run("Notebook Statefulset Validation", func(t *testing.T) {
				err = testCtx.testNotebookValidation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing StatefulSet for Notebook ")
			})

			t.Run("Notebook OAuth sidecar Validation", func(t *testing.T) {
				if deploymentMode == ServiceMesh {
					t.Skipf("Skipping as it's not relevant for Service Mesh scenario")
				}
				err = testCtx.testNotebookOAuthSidecar(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing sidecar for Notebook ")
			})

			t.Run("Notebook OAuth sidecar Resource Validation", func(t *testing.T) {
				if deploymentMode == ServiceMesh {
					t.Skipf("Skipping as it's not relevant for Service Mesh scenario")
				}
				err = testCtx.testNotebookOAuthSidecarResources(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing sidecar resources for Notebook ")
			})

			t.Run("Verify Notebook Traffic", func(t *testing.T) {
				err = testCtx.testNotebookTraffic(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Notebook traffic ")
			})

			t.Run("Verify Notebook Culling", func(t *testing.T) {
				err = testCtx.testNotebookCulling(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Notebook culling ")
			})
		})
	}

}

func (tc *testContext) testNotebookCreation(nbContext notebookContext) error {
	logger := GetCreationLogger().WithValues(
		"notebook", nbContext.nbObjectMeta.Name,
		"namespace", nbContext.nbObjectMeta.Namespace,
	)
	logger.Info("Starting notebook creation test")

	startTime := time.Now()

	testNotebook := &nbv1.Notebook{
		ObjectMeta: *nbContext.nbObjectMeta,
		Spec:       *nbContext.nbSpec,
	}

	// Create test Notebook resource if not already created
	notebookLookupKey := types.NamespacedName{Name: testNotebook.Name, Namespace: testNotebook.Namespace}
	createdNotebook := nbv1.Notebook{}

	logger.V(1).Info("Checking if notebook already exists")
	err := tc.customClient.Get(tc.ctx, notebookLookupKey, &createdNotebook)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("Notebook not found, creating new notebook")
			pollCount := 0
			nberr := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
				pollCount++
				creationErr := tc.customClient.Create(ctx, testNotebook)
				if creationErr != nil {
					logger.V(1).Info("Error creating notebook resource, trying again",
						"poll", pollCount,
						"error", creationErr)
					return false, nil
				} else {
					logger.V(1).Info("Successfully created notebook",
						"poll", pollCount)
					return true, nil
				}
			})
			if nberr != nil {
				logger.V(1).Info("Failed to create test notebook",
					"attempts", pollCount,
					"error", nberr)
				return fmt.Errorf("error creating test Notebook %s: %v", testNotebook.Name, nberr)
			}
		} else {
			logger.V(1).Info("Error getting test notebook", "error", err)
			return fmt.Errorf("error getting test Notebook %s: %v", testNotebook.Name, err)
		}
	} else {
		logger.Info("Notebook already exists")
	}

	logger.Info("Notebook creation test completed successfully",
		"duration", time.Since(startTime))
	return nil
}

func (tc *testContext) testNotebookRouteCreation(nbMeta *metav1.ObjectMeta) error {
	nbRoute, err := tc.getNotebookRoute(nbMeta)
	if err != nil {
		return fmt.Errorf("error getting Route for Notebook %v: %v", nbRoute.Name, err)
	}
	isReady := false
	for _, ingress := range nbRoute.Status.Ingress {
		if strings.Contains(ingress.Host, nbRoute.Name) {
			for _, condition := range ingress.Conditions {
				if condition.Type == routev1.RouteAdmitted && condition.Status == v1.ConditionTrue {
					isReady = true
				}
			}
		}
	}
	if !isReady {
		return fmt.Errorf("Notebook Route %s is not Ready.", nbRoute.Name)
	}
	return err
}

func (tc *testContext) testNetworkPolicyCreation(nbMeta *metav1.ObjectMeta) error {
	err := tc.ensureNetworkPolicyAllowingAccessToOnlyNotebookControllerExists(nbMeta)
	if err != nil {
		return err
	}

	if deploymentMode == OAuthProxy {
		return tc.ensureOAuthNetworkPolicyExists(nbMeta)
	}

	return nil
}

func (tc *testContext) ensureOAuthNetworkPolicyExists(nbMeta *metav1.ObjectMeta) error {
	// Test Notebook Network policy that allows all requests on Notebook OAuth port
	notebookOAuthNetworkPolicy, err := tc.getNotebookNetworkPolicy(nbMeta, nbMeta.Name+"-oauth-np")
	if err != nil {
		return fmt.Errorf("error getting network policy for Notebook OAuth port %v: %v", notebookOAuthNetworkPolicy.Name, err)
	}

	if len(notebookOAuthNetworkPolicy.Spec.PolicyTypes) == 0 || notebookOAuthNetworkPolicy.Spec.PolicyTypes[0] != netv1.PolicyTypeIngress {
		return fmt.Errorf("invalid policy type. Expected value :%v", netv1.PolicyTypeIngress)
	}

	if len(notebookOAuthNetworkPolicy.Spec.Ingress) == 0 {
		return fmt.Errorf("invalid network policy, should contain ingress rule")
	} else if len(notebookOAuthNetworkPolicy.Spec.Ingress[0].Ports) != 0 {
		isNotebookPort := false
		for _, notebookport := range notebookOAuthNetworkPolicy.Spec.Ingress[0].Ports {
			if notebookport.Port.IntVal == 8443 {
				isNotebookPort = true
			}
		}
		if !isNotebookPort {
			return fmt.Errorf("invalid Network Policy comfiguration")
		}
	}
	return err
}

func (tc *testContext) ensureNetworkPolicyAllowingAccessToOnlyNotebookControllerExists(nbMeta *metav1.ObjectMeta) error {
	// Test Notebook Network Policy that allows access only to Notebook Controller
	notebookNetworkPolicy, err := tc.getNotebookNetworkPolicy(nbMeta, nbMeta.Name+"-ctrl-np")
	if err != nil {
		return fmt.Errorf("error getting network policy for Notebook %v: %v", notebookNetworkPolicy.Name, err)
	}

	if len(notebookNetworkPolicy.Spec.PolicyTypes) == 0 || notebookNetworkPolicy.Spec.PolicyTypes[0] != netv1.PolicyTypeIngress {
		return fmt.Errorf("invalid policy type. Expected value :%v", netv1.PolicyTypeIngress)
	}

	if len(notebookNetworkPolicy.Spec.Ingress) == 0 {
		return fmt.Errorf("invalid network policy, should contain ingress rule")
	} else if len(notebookNetworkPolicy.Spec.Ingress[0].Ports) != 0 {
		isNotebookPort := false
		for _, notebookport := range notebookNetworkPolicy.Spec.Ingress[0].Ports {
			if notebookport.Port.IntVal == 8888 {
				isNotebookPort = true
			}
		}
		if !isNotebookPort {
			return fmt.Errorf("invalid Network Policy comfiguration")
		}
	} else if len(notebookNetworkPolicy.Spec.Ingress[0].From) == 0 {
		return fmt.Errorf("invalid Network Policy comfiguration")
	}

	return nil
}

func (tc *testContext) testNotebookValidation(nbMeta *metav1.ObjectMeta) error {
	// Verify StatefulSet is running
	err := tc.waitForStatefulSet(nbMeta, 1, 1)
	if err != nil {
		return fmt.Errorf("error validating notebook StatefulSet: %s", err)
	}
	return nil
}

func (tc *testContext) testNotebookOAuthSidecar(nbMeta *metav1.ObjectMeta) error {

	nbPods, err := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("statefulset=%s", nbMeta.Name)})

	if err != nil {
		return fmt.Errorf("error retrieving Notebook pods :%v", err)
	}

	for _, nbpod := range nbPods.Items {
		if nbpod.Status.Phase != v1.PodRunning {
			return fmt.Errorf("notebook pod %v is not in Running phase", nbpod.Name)
		}
		for _, containerStatus := range nbpod.Status.ContainerStatuses {
			if containerStatus.Name == "oauth-proxy" {
				if !containerStatus.Ready {
					return fmt.Errorf("oauth-proxy container is not in Ready state for pod %v", nbpod.Name)
				}
			}
		}
	}
	return nil
}

func (tc *testContext) testNotebookTraffic(nbMeta *metav1.ObjectMeta) error {
	resp, err := tc.curlNotebookEndpoint(*nbMeta)
	if err != nil {
		return fmt.Errorf("error accessing Notebook Endpoint: %v ", err)
	}
	if resp.StatusCode != 200 {
		return errorWithBody(resp)
	}
	return nil
}

func (tc *testContext) testNotebookCulling(nbMeta *metav1.ObjectMeta) error {
	logger := GetCullingLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
	)
	logger.Info("Starting notebook culling test")

	startTime := time.Now()

	// Create Configmap with culling configuration
	cullingConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "notebook-controller-culler-config",
			Namespace: tc.testNamespace,
		},
		Data: map[string]string{
			"ENABLE_CULLING":        "true",
			"CULL_IDLE_TIME":        "2",
			"IDLENESS_CHECK_PERIOD": "1",
		},
	}

	logger.Info("Creating culling ConfigMap",
		"cullIdleTime", "2 minutes",
		"idlenessCheckPeriod", "1 minute")
	_, err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Create(tc.ctx, cullingConfigMap,
		metav1.CreateOptions{})
	if err != nil {
		logger.V(1).Info("Failed to create culling ConfigMap", "error", err)
		return fmt.Errorf("error creating configmapnotebook-controller-culler-config: %v", err)
	}

	// Restart the deployment to get changes from configmap
	logger.V(1).Info("Getting controller deployment for restart")
	controllerDeployment, err := tc.kubeClient.AppsV1().Deployments(tc.testNamespace).Get(tc.ctx,
		"notebook-controller-deployment", metav1.GetOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get controller deployment", "error", err)
		return fmt.Errorf("error getting deployment %v: %v", controllerDeployment.Name, err)
	}

	defer tc.revertCullingConfiguration(cullingConfigMap.ObjectMeta, controllerDeployment.ObjectMeta, nbMeta)

	logger.Info("Rolling out controller deployment to apply culling configuration")
	err = tc.rolloutDeployment(controllerDeployment.ObjectMeta)
	if err != nil {
		logger.V(1).Info("Failed to rollout deployment with culling configuration", "error", err)
		return fmt.Errorf("error rolling out the deployment with culling configuration: %v", err)
	}

	// Active monitoring of culling process with detailed timing validation
	err = tc.monitorNotebookCulling(nbMeta, logger, startTime)
	if err != nil {
		return err
	}
	logger.Info("Successfully verified notebook culling process",
		"duration", time.Since(startTime))
	return nil
}

// monitorNotebookCulling actively monitors the notebook culling process with detailed validation
// It monitors StatefulSet/Pod status and last activity annotations instead of accessing the endpoint
// to avoid interfering with the culling process
func (tc *testContext) monitorNotebookCulling(nbMeta *metav1.ObjectMeta, logger logr.Logger, testStartTime time.Time) error {
	// Configuration for culling monitoring
	const (
		minimumCullTime = 2 * time.Minute  // Notebook should NOT be culled before this
		maximumCullTime = 4 * time.Minute  // Notebook SHOULD be culled by this time
		checkInterval   = 10 * time.Second // Check every 10 seconds
	)

	logger.Info("Starting active notebook culling monitoring (non-intrusive)",
		"minimumCullTime", minimumCullTime,
		"maximumCullTime", maximumCullTime,
		"checkInterval", checkInterval,
		"method", "StatefulSet/Pod monitoring")

	checkCount := 0
	cullingStartTime := time.Now()

	// Monitor until maximum timeout
	for {
		checkCount++
		elapsedSinceStart := time.Since(cullingStartTime)
		elapsedSinceTestStart := time.Since(testStartTime)

		// Check notebook CR for last activity annotation
		lastActivity, annotationErr := tc.getNotebookLastActivity(nbMeta)

		// Check StatefulSet status (primary culling indicator)
		isCulled, ssReplicas, podCount, err := tc.checkNotebookCullingStatus(nbMeta)

		// Log comprehensive status
		logFields := []interface{}{
			"check", checkCount,
			"elapsedTime", elapsedSinceStart,
			"totalTestTime", elapsedSinceTestStart,
			"isCulled", isCulled,
			"statefulSetReplicas", ssReplicas,
			"podCount", podCount,
		}

		if annotationErr == nil && lastActivity != "" {
			logFields = append(logFields, "lastActivity", lastActivity)
		} else if annotationErr != nil {
			logFields = append(logFields, "lastActivityError", annotationErr.Error())
		}

		if err != nil {
			logFields = append(logFields, "statusCheckError", err.Error())
		}

		logger.V(1).Info("Notebook culling status check", logFields...)

		// If notebook has been culled (StatefulSet scaled to 0 or pods removed)
		if isCulled {
			if elapsedSinceStart < minimumCullTime {
				logger.Info("Notebook was culled too early - this may indicate a timing issue",
					"actualCullTime", elapsedSinceStart,
					"minimumExpectedTime", minimumCullTime,
					"checks", checkCount,
					"lastActivity", lastActivity)
				return fmt.Errorf("notebook was culled too early: culled after %v, expected minimum %v",
					elapsedSinceStart, minimumCullTime)
			}

			logger.Info("Notebook successfully culled within expected timeframe",
				"actualCullTime", elapsedSinceStart,
				"totalTestTime", elapsedSinceTestStart,
				"checks", checkCount,
				"lastActivity", lastActivity,
				"finalStatefulSetReplicas", ssReplicas,
				"finalPodCount", podCount)
			return nil
		}

		// If we've exceeded maximum time and notebook is still active
		if elapsedSinceStart >= maximumCullTime {
			logger.Info("Notebook was not culled within maximum expected time",
				"actualTime", elapsedSinceStart,
				"maximumExpectedTime", maximumCullTime,
				"checks", checkCount,
				"lastActivity", lastActivity,
				"statefulSetReplicas", ssReplicas,
				"podCount", podCount)
			return fmt.Errorf("notebook was not culled within expected timeframe: still active after %v (replicas: %d, pods: %d)",
				elapsedSinceStart, ssReplicas, podCount)
		}

		// Break if we've exceeded absolute maximum time (safety check)
		if elapsedSinceStart >= maximumCullTime+time.Minute {
			logger.Info("Exceeded absolute maximum monitoring time",
				"elapsedTime", elapsedSinceStart,
				"checks", checkCount)
			return fmt.Errorf("culling monitoring exceeded absolute maximum time: %v", elapsedSinceStart)
		}

		// Wait before next check
		time.Sleep(checkInterval)
	}
}

// getNotebookLastActivity retrieves the last activity annotation from the Notebook CR
func (tc *testContext) getNotebookLastActivity(nbMeta *metav1.ObjectMeta) (string, error) {
	notebook := &nbv1.Notebook{}
	err := tc.customClient.Get(tc.ctx, types.NamespacedName{
		Name:      nbMeta.Name,
		Namespace: nbMeta.Namespace,
	}, notebook)

	if err != nil {
		return "", fmt.Errorf("failed to get notebook CR: %v", err)
	}

	// Look for common last activity annotation keys
	lastActivityKeys := []string{
		"notebooks.kubeflow.org/last-activity",
		"opendatahub.io/last-activity",
		"last-activity",
	}

	for _, key := range lastActivityKeys {
		if value, exists := notebook.Annotations[key]; exists {
			return value, nil
		}
	}

	return "", nil // No last activity annotation found
}

// checkNotebookCullingStatus checks if the notebook has been culled by examining StatefulSet and Pod status
func (tc *testContext) checkNotebookCullingStatus(nbMeta *metav1.ObjectMeta) (isCulled bool, ssReplicas int32, podCount int, err error) {
	// Check StatefulSet replicas
	ss, ssErr := tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(tc.ctx, nbMeta.Name, metav1.GetOptions{})
	if ssErr != nil {
		if errors.IsNotFound(ssErr) {
			// StatefulSet deleted = definitely culled
			return true, 0, 0, nil
		}
		return false, 0, 0, fmt.Errorf("error getting StatefulSet: %v", ssErr)
	}

	ssReplicas = ss.Status.ReadyReplicas

	// Check Pod count
	pods, podErr := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("statefulset=%s", nbMeta.Name),
	})
	if podErr != nil {
		return false, ssReplicas, 0, fmt.Errorf("error getting Pods: %v", podErr)
	}

	// Count only running/pending pods (not terminating)
	activePodCount := 0
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp == nil {
			activePodCount++
		}
	}

	// Notebook is considered culled if:
	// 1. StatefulSet has 0 ready replicas AND
	// 2. No active pods are running
	isCulled = ssReplicas == 0 && activePodCount == 0

	return isCulled, ssReplicas, activePodCount, nil
}

func (tc *testContext) testNotebookOAuthSidecarResources(nbMeta *metav1.ObjectMeta) error {
	nbPods, err := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("statefulset=%s", nbMeta.Name)})

	if err != nil {
		return fmt.Errorf("error retrieving Notebook pods: %v", err)
	}

	// Get the notebook to check annotations
	notebook := &nbv1.Notebook{}
	err = tc.customClient.Get(tc.ctx, types.NamespacedName{Name: nbMeta.Name, Namespace: nbMeta.Namespace}, notebook)
	if err != nil {
		return fmt.Errorf("error getting notebook: %v", err)
	}

	for _, nbpod := range nbPods.Items {
		if nbpod.Status.Phase != v1.PodRunning {
			return fmt.Errorf("notebook pod %v is not in Running phase", nbpod.Name)
		}

		// Find oauth-proxy container
		var oauthContainer *v1.Container
		for _, container := range nbpod.Spec.Containers {
			if container.Name == "oauth-proxy" {
				oauthContainer = &container
				break
			}
		}

		if oauthContainer == nil {
			return fmt.Errorf("oauth-proxy container not found in pod %v", nbpod.Name)
		}

		// Validate resource specifications based on annotations
		annotations := notebook.GetAnnotations()
		if annotations != nil {
			// Check CPU request
			if expectedCPUReqStr, exists := annotations["notebooks.opendatahub.io/auth-sidecar-cpu-request"]; exists {
				expectedCPUReq, err := resource.ParseQuantity(strings.TrimSpace(expectedCPUReqStr))
				if err != nil {
					return fmt.Errorf("invalid expected CPU request in annotation '%s': %v", expectedCPUReqStr, err)
				}
				actualCPUReq := oauthContainer.Resources.Requests.Cpu()
				if actualCPUReq.Cmp(expectedCPUReq) != 0 {
					return fmt.Errorf("expected CPU request %s, got %s for pod %v", expectedCPUReq.String(), actualCPUReq.String(), nbpod.Name)
				}
			}

			// Check Memory request
			if expectedMemReqStr, exists := annotations["notebooks.opendatahub.io/auth-sidecar-memory-request"]; exists {
				expectedMemReq, err := resource.ParseQuantity(strings.TrimSpace(expectedMemReqStr))
				if err != nil {
					return fmt.Errorf("invalid expected memory request in annotation '%s': %v", expectedMemReqStr, err)
				}
				actualMemReq := oauthContainer.Resources.Requests.Memory()
				if actualMemReq.Cmp(expectedMemReq) != 0 {
					return fmt.Errorf("expected memory request %s, got %s for pod %v", expectedMemReq.String(), actualMemReq.String(), nbpod.Name)
				}
			}

			// Check CPU limit
			if expectedCPULimitStr, exists := annotations["notebooks.opendatahub.io/auth-sidecar-cpu-limit"]; exists {
				expectedCPULimit, err := resource.ParseQuantity(strings.TrimSpace(expectedCPULimitStr))
				if err != nil {
					return fmt.Errorf("invalid expected CPU limit in annotation '%s': %v", expectedCPULimitStr, err)
				}
				actualCPULimit := oauthContainer.Resources.Limits.Cpu()
				if actualCPULimit.Cmp(expectedCPULimit) != 0 {
					return fmt.Errorf("expected CPU limit %s, got %s for pod %v", expectedCPULimit.String(), actualCPULimit.String(), nbpod.Name)
				}
			}

			// Check Memory limit
			if expectedMemLimitStr, exists := annotations["notebooks.opendatahub.io/auth-sidecar-memory-limit"]; exists {
				expectedMemLimit, err := resource.ParseQuantity(strings.TrimSpace(expectedMemLimitStr))
				if err != nil {
					return fmt.Errorf("invalid expected memory limit in annotation '%s': %v", expectedMemLimitStr, err)
				}
				actualMemLimit := oauthContainer.Resources.Limits.Memory()
				if actualMemLimit.Cmp(expectedMemLimit) != 0 {
					return fmt.Errorf("expected memory limit %s, got %s for pod %v", expectedMemLimit.String(), actualMemLimit.String(), nbpod.Name)
				}
			}
		}
	}
	return nil
}

func errorWithBody(resp *http.Response) error {
	dump, err := httputil.DumpResponse(resp, false)
	if err != nil {
		return err
	}

	return fmt.Errorf("unexpected response from Notebook Endpoint:\n%+v", string(dump))
}
