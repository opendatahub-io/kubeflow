package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (tc *testContext) waitForControllerDeployment(name string, replicas int32) error {
	logger := GetHelperLogger().WithValues(
		"deployment", name,
		"replicas", replicas,
		"timeout", tc.resourceCreationTimeout,
	)
	logger.Info("Waiting for controller deployment")

	startTime := time.Now()
	pollCount := 0

	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		controllerDeployment, err := tc.kubeClient.AppsV1().Deployments(tc.testNamespace).Get(ctx, name, metav1.GetOptions{})

		if err != nil {
			if errors.IsNotFound(err) {
				logger.V(1).Info("Deployment not found, continuing to wait",
					"poll", pollCount)
				return false, nil
			}
			logger.V(1).Info("Error getting controller deployment",
				"poll", pollCount,
				"error", err)
			return false, err
		}

		logger.V(1).Info("Deployment status",
			"poll", pollCount,
			"readyReplicas", controllerDeployment.Status.ReadyReplicas,
			"expectedReplicas", replicas,
			"availableReplicas", controllerDeployment.Status.AvailableReplicas,
			"unavailableReplicas", controllerDeployment.Status.UnavailableReplicas)

		for _, condition := range controllerDeployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable {
				if condition.Status == v1.ConditionTrue && controllerDeployment.Status.ReadyReplicas == replicas {
					logger.Info("Controller deployment ready",
						"duration", time.Since(startTime),
						"polls", pollCount)
					return true, nil
				}
				logger.V(1).Info("Deployment not yet ready",
					"poll", pollCount,
					"availableCondition", condition.Status,
					"readyReplicas", controllerDeployment.Status.ReadyReplicas,
					"expectedReplicas", replicas)
			}
		}

		return false, nil

	})

	if err != nil {
		logger.Error(err, "Controller deployment failed to become ready",
			"polls", pollCount,
			"duration", time.Since(startTime))
	}
	return err
}

func (tc *testContext) getNotebookRoute(nbMeta *metav1.ObjectMeta) (*routev1.Route, error) {
	logger := GetHelperLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
		"deploymentMode", deploymentMode.String(),
	)
	logger.Info("Getting notebook route")

	startTime := time.Now()
	nbRouteList := routev1.RouteList{}

	var opts []client.ListOption
	if deploymentMode == ServiceMesh {
		opts = append(opts, client.MatchingLabels{"maistra.io/gateway-name": "odh-gateway"})
		logger.V(1).Info("Using Service Mesh mode",
			"label", "maistra.io/gateway-name=odh-gateway")
	} else {
		opts = append(opts, client.MatchingLabels{"notebook-name": nbMeta.Name})
		logger.V(1).Info("Using OAuth mode",
			"label", fmt.Sprintf("notebook-name=%s", nbMeta.Name))
	}

	pollCount := 0
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		routeErr := tc.customClient.List(ctx, &nbRouteList, opts...)
		if routeErr != nil {
			logger.V(1).Info("Error retrieving route list",
				"poll", pollCount,
				"error", routeErr)
			return false, nil
		} else {
			logger.V(1).Info("Retrieved route list",
				"poll", pollCount,
				"routeCount", len(nbRouteList.Items))
			return true, nil
		}
	})

	if err != nil {
		logger.V(1).Info("Failed to retrieve notebook route after polling",
			"polls", pollCount,
			"error", err)
		return nil, err
	}

	if len(nbRouteList.Items) == 0 {
		logger.V(1).Info("No notebook route found matching the specified labels")
		return nil, fmt.Errorf("no Notebook route found")
	}

	route := &nbRouteList.Items[0]
	logger.Info("Found notebook route",
		"routeName", route.Name,
		"host", route.Spec.Host,
		"duration", time.Since(startTime))
	return route, nil
}

func (tc *testContext) getNotebookNetworkPolicy(nbMeta *metav1.ObjectMeta, name string) (*netv1.NetworkPolicy, error) {
	logger := GetHelperLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
		"networkPolicy", name,
	)
	logger.Info("Getting notebook network policy")

	startTime := time.Now()
	nbNetworkPolicy := &netv1.NetworkPolicy{}
	pollCount := 0

	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		np, npErr := tc.kubeClient.NetworkingV1().NetworkPolicies(nbMeta.Namespace).Get(ctx, name, metav1.GetOptions{})
		if npErr != nil {
			logger.V(1).Info("Network policy not found yet",
				"poll", pollCount,
				"error", npErr)
			return false, nil
		} else {
			nbNetworkPolicy = np
			logger.Info("Network policy found",
				"duration", time.Since(startTime),
				"polls", pollCount)
			return true, nil
		}
	})

	if err != nil {
		logger.V(1).Info("Failed to get network policy",
			"polls", pollCount,
			"duration", time.Since(startTime),
			"error", err)
	}
	return nbNetworkPolicy, err
}

func (tc *testContext) curlNotebookEndpoint(nbMeta metav1.ObjectMeta) (*http.Response, error) {
	logger := GetHelperLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
	)
	logger.Info("Accessing notebook endpoint")

	startTime := time.Now()

	nbRoute, err := tc.getNotebookRoute(&nbMeta)
	if err != nil {
		logger.V(1).Info("Failed to get notebook route", "error", err)
		return nil, err
	}

	// Access the Notebook endpoint using http request
	notebookEndpoint := "https://" + nbRoute.Spec.Host + "/notebook/" +
		nbMeta.Namespace + "/" + nbMeta.Name + "/api"
	logger.V(1).Info("Making HTTP GET request",
		"endpoint", notebookEndpoint)

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	req, err := http.NewRequest("GET", notebookEndpoint, nil)
	if err != nil {
		logger.V(1).Info("Failed to create HTTP request", "error", err)
		return nil, err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		logger.V(1).Info("HTTP request failed",
			"endpoint", notebookEndpoint,
			"error", err)
		return nil, err
	}

	logger.Info("HTTP request completed",
		"statusCode", resp.StatusCode,
		"duration", time.Since(startTime))
	return resp, nil
}

func (tc *testContext) rolloutDeployment(depMeta metav1.ObjectMeta) error {

	// Scale deployment to 0
	err := tc.scaleDeployment(depMeta, int32(0))
	if err != nil {
		return fmt.Errorf("error while scaling down the deployment %v", err)
	}
	// Wait for deployment to scale down
	time.Sleep(5 * time.Second)

	// Scale deployment to 1
	err = tc.scaleDeployment(depMeta, int32(1))
	if err != nil {
		return fmt.Errorf("error while scaling up the deployment %v", err)
	}
	return nil
}

func (tc *testContext) waitForStatefulSet(nbMeta *metav1.ObjectMeta, availableReplicas int32, readyReplicas int32) error {
	logger := GetHelperLogger().WithValues(
		"statefulset", nbMeta.Name,
		"namespace", nbMeta.Namespace,
		"expectedAvailable", availableReplicas,
		"expectedReady", readyReplicas,
	)
	logger.V(1).Info("Waiting for StatefulSet to reach expected replica count")

	// Verify StatefulSet is running expected number of replicas
	pollCount := 0
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		notebookStatefulSet, err1 := tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(ctx,
			nbMeta.Name, metav1.GetOptions{})

		if err1 != nil {
			if errors.IsNotFound(err1) {
				logger.V(1).Info("StatefulSet not found yet",
					"poll", pollCount)
				return false, nil
			} else {
				logger.V(1).Info("Error getting StatefulSet",
					"poll", pollCount,
					"error", err1)
				return false, err1
			}
		}

		// Enhanced logging with detailed StatefulSet status
		logger.V(1).Info("StatefulSet status",
			"poll", pollCount,
			"availableReplicas", notebookStatefulSet.Status.AvailableReplicas,
			"readyReplicas", notebookStatefulSet.Status.ReadyReplicas,
			"replicas", notebookStatefulSet.Status.Replicas,
			"currentReplicas", notebookStatefulSet.Status.CurrentReplicas,
			"updatedReplicas", notebookStatefulSet.Status.UpdatedReplicas,
			"observedGeneration", notebookStatefulSet.Status.ObservedGeneration,
			"currentRevision", notebookStatefulSet.Status.CurrentRevision,
			"updateRevision", notebookStatefulSet.Status.UpdateRevision)

		// Log StatefulSet conditions if any
		if len(notebookStatefulSet.Status.Conditions) > 0 {
			for _, condition := range notebookStatefulSet.Status.Conditions {
				logger.V(1).Info("StatefulSet condition",
					"poll", pollCount,
					"type", condition.Type,
					"status", condition.Status,
					"reason", condition.Reason,
					"message", condition.Message)
			}
		}

		// If replicas are not as expected, get detailed pod information
		if notebookStatefulSet.Status.AvailableReplicas != availableReplicas ||
			notebookStatefulSet.Status.ReadyReplicas != readyReplicas {
			tc.logDetailedPodStatus(nbMeta, pollCount, logger)

			// Log cluster resource availability to help diagnose resource constraints
			tc.logClusterResourceAvailability(pollCount, logger)
		}

		if notebookStatefulSet.Status.AvailableReplicas == availableReplicas &&
			notebookStatefulSet.Status.ReadyReplicas == readyReplicas {
			logger.V(1).Info("StatefulSet reached expected replica count",
				"polls", pollCount)
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		logger.V(1).Info("StatefulSet failed to reach expected replica count",
			"polls", pollCount,
			"error", err)
		// Log final detailed status on failure
		tc.logFinalStatefulSetAndPodStatus(nbMeta, logger)
		// Log final cluster resource status to help diagnose resource issues
		tc.logClusterResourceAvailability(0, logger)
	}
	return err
}

func (tc *testContext) revertCullingConfiguration(cmMeta metav1.ObjectMeta, depMeta metav1.ObjectMeta, nbMeta *metav1.ObjectMeta) {
	logger := GetCullingLogger().WithValues(
		"configMap", cmMeta.Name,
		"deployment", depMeta.Name,
		"notebook", nbMeta.Name,
	)
	logger.Info("Reverting culling configuration")

	// Delete the culling configuration Configmap once the test is completed
	err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Delete(tc.ctx,
		cmMeta.Name, metav1.DeleteOptions{})
	if err != nil {
		logger.Error(err, "Error deleting culling configmap")
	} else {
		logger.V(1).Info("Successfully deleted culling configmap")
	}

	// Roll out the controller deployment
	logger.V(1).Info("Rolling out controller deployment to revert culling configuration")
	err = tc.rolloutDeployment(depMeta)
	if err != nil {
		logger.Error(err, "Error rolling out deployment")
	} else {
		logger.V(1).Info("Successfully rolled out deployment")
	}

	// IMPORTANT: Culling affects ALL notebooks in the namespace, not just the test target
	// The restartAllCulledNotebooks function will handle restarting this notebook and any others that were culled
	logger.V(1).Info("Restarting all culled notebooks")
	err = tc.restartAllCulledNotebooks()
	if err != nil {
		logger.Info("Warning: Failed to restart other culled notebooks",
			"error", err)
	} else {
		logger.V(1).Info("Successfully restarted all culled notebooks")
	}
}

// restartAllCulledNotebooks finds all notebooks with kubeflow-resource-stopped annotation and restarts them
func (tc *testContext) restartAllCulledNotebooks() error {
	// List all notebooks in the test namespace
	notebookList := &nbv1.NotebookList{}
	err := tc.customClient.List(tc.ctx, notebookList, client.InNamespace(tc.testNamespace))
	if err != nil {
		return fmt.Errorf("failed to list notebooks: %v", err)
	}

	culledNotebooks := []nbv1.Notebook{}
	for _, notebook := range notebookList.Items {
		if _, exists := notebook.Annotations["kubeflow-resource-stopped"]; exists {
			culledNotebooks = append(culledNotebooks, notebook)
		}
	}

	if len(culledNotebooks) == 0 {
		return nil
	}

	// Restart each culled notebook
	for _, notebook := range culledNotebooks {
		// Remove the kubeflow-resource-stopped annotation
		patch := client.RawPatch(types.JSONPatchType, []byte(`[{"op": "remove", "path": "/metadata/annotations/kubeflow-resource-stopped"}]`))
		notebookForPatch := &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebook.Name,
				Namespace: notebook.Namespace,
			},
		}

		if err := tc.customClient.Patch(tc.ctx, notebookForPatch, patch); err != nil {
			logger := GetCullingLogger()
			logger.Error(err, "Failed to patch notebook",
				"notebook", notebook.Name)
			continue
		}

		// Wait for the notebook to become ready
		nbMeta := &metav1.ObjectMeta{Name: notebook.Name, Namespace: notebook.Namespace}
		if waitErr := tc.waitForStatefulSet(nbMeta, 1, 1); waitErr != nil {
			logger := GetCullingLogger()
			logger.Info("Warning: Notebook didn't become ready within timeout",
				"notebook", notebook.Name,
				"error", waitErr)
		}
	}

	return nil
}

func (tc *testContext) scaleDeployment(depMeta metav1.ObjectMeta, desiredReplicas int32) error {
	// Get latest version of the deployment to avoid updating a stale object.
	deployment, err := tc.kubeClient.AppsV1().Deployments(depMeta.Namespace).Get(tc.ctx,
		depMeta.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	deployment.Spec.Replicas = &desiredReplicas
	_, err = tc.kubeClient.AppsV1().Deployments(deployment.Namespace).Update(tc.ctx,
		deployment, metav1.UpdateOptions{})
	return err
}

// Add spec and metadata for Notebook objects
func setupThothMinimalOAuthNotebook() notebookContext {
	testNotebookName := "thoth-minimal-oauth-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"notebooks.opendatahub.io/inject-oauth": "true"},
			Name:        testNotebookName,
			Namespace:   notebookTestNamespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:       testNotebookName,
							Image:      "quay.io/thoth-station/s2i-minimal-notebook:v0.2.2",
							WorkingDir: "/opt/app-root/src",
							Ports: []v1.ContainerPort{
								{
									Name:          "notebook-port",
									ContainerPort: 8888,
									Protocol:      "TCP",
								},
							},
							EnvFrom: []v1.EnvFromSource{},
							Env: []v1.EnvVar{
								{
									Name:  "JUPYTER_NOTEBOOK_PORT",
									Value: "8888",
								},
								{
									Name:  "NOTEBOOK_ARGS",
									Value: "--ServerApp.port=8888 --NotebookApp.token='' --NotebookApp.password='' --ServerApp.base_url=/notebook/" + notebookTestNamespace + "/" + testNotebookName,
								},
							},
							Resources: v1.ResourceRequirements{
								Limits: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							LivenessProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path:   "/notebook/" + notebookTestNamespace + "/" + testNotebookName + "/api",
										Port:   intstr.FromString("notebook-port"),
										Scheme: "HTTP",
									},
								},
								InitialDelaySeconds: 5,
								TimeoutSeconds:      1,
								PeriodSeconds:       5,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
						},
					},
				},
			},
		},
	}

	thothMinimalOAuthNbContext := notebookContext{
		nbObjectMeta: &testNotebook.ObjectMeta,
		nbSpec:       &testNotebook.Spec,
	}
	return thothMinimalOAuthNbContext
}

// Add spec and metadata for Notebook objects with custom OAuth proxy resources
func setupThothOAuthCustomResourcesNotebook() notebookContext {
	// Too long name - shall be resolved via https://issues.redhat.com/browse/RHOAIENG-33609
	// testNotebookName := "thoth-oauth-custom-resources-notebook"
	testNotebookName := "thoth-custom-resources-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"notebooks.opendatahub.io/inject-oauth":                "true",
				"notebooks.opendatahub.io/auth-sidecar-cpu-request":    "0.2", // equivalent to 200m
				"notebooks.opendatahub.io/auth-sidecar-memory-request": "128Mi",
				"notebooks.opendatahub.io/auth-sidecar-cpu-limit":      "400m",
				"notebooks.opendatahub.io/auth-sidecar-memory-limit":   "256Mi",
			},
			Name:      testNotebookName,
			Namespace: notebookTestNamespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:       testNotebookName,
							Image:      "quay.io/thoth-station/s2i-minimal-notebook:v0.2.2",
							WorkingDir: "/opt/app-root/src",
							Ports: []v1.ContainerPort{
								{
									Name:          "notebook-port",
									ContainerPort: 8888,
									Protocol:      "TCP",
								},
							},
							EnvFrom: []v1.EnvFromSource{},
							Env: []v1.EnvVar{
								{
									Name:  "JUPYTER_ENABLE_LAB",
									Value: "yes",
								},
							},
							Resources: v1.ResourceRequirements{
								Limits: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: v1.ResourceList{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	thothOAuthCustomResourcesNbContext := notebookContext{
		nbObjectMeta: &testNotebook.ObjectMeta,
		nbSpec:       &testNotebook.Spec,
	}
	return thothOAuthCustomResourcesNbContext
}

func setupThothMinimalServiceMeshNotebook() notebookContext {
	testNotebookName := "thoth-minimal-service-mesh-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"opendatahub.io/service-mesh": "true"},
			Name:        testNotebookName,
			Namespace:   notebookTestNamespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:       testNotebookName,
							Image:      "quay.io/thoth-station/s2i-minimal-notebook:v0.2.2",
							WorkingDir: "/opt/app-root/src",
							Ports: []v1.ContainerPort{
								{
									Name:          "notebook-port",
									ContainerPort: 8888,
									Protocol:      "TCP",
								},
							},
							EnvFrom: []v1.EnvFromSource{},
							Env: []v1.EnvVar{
								{
									Name:  "JUPYTER_NOTEBOOK_PORT",
									Value: "8888",
								},
								{
									Name:  "NOTEBOOK_ARGS",
									Value: "--ServerApp.port=8888 --NotebookApp.token='' --NotebookApp.password='' --ServerApp.base_url=/notebook/" + notebookTestNamespace + "/" + testNotebookName,
								},
							},
							Resources: v1.ResourceRequirements{
								Limits: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							LivenessProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path:   "/notebook/" + notebookTestNamespace + "/" + testNotebookName + "/api",
										Port:   intstr.FromString("notebook-port"),
										Scheme: "HTTP",
									},
								},
								InitialDelaySeconds: 5,
								TimeoutSeconds:      1,
								PeriodSeconds:       5,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
						},
					},
				},
			},
		},
	}

	thothMinimalServiceMeshNbContext := notebookContext{
		nbObjectMeta:   &testNotebook.ObjectMeta,
		nbSpec:         &testNotebook.Spec,
		deploymentMode: ServiceMesh,
	}
	return thothMinimalServiceMeshNbContext
}

func notebooksForScenario(notebooks []notebookContext, mode DeploymentMode) []notebookContext {
	var filtered []notebookContext
	for _, notebook := range notebooks {
		if notebook.deploymentMode == mode {
			filtered = append(filtered, notebook)
		}
	}

	return filtered
}

// logDetailedPodStatus logs detailed information about pods when StatefulSet is not ready
func (tc *testContext) logDetailedPodStatus(nbMeta *metav1.ObjectMeta, pollCount int, logger logr.Logger) {
	pods, err := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("statefulset=%s", nbMeta.Name),
	})
	if err != nil {
		logger.V(1).Info("Failed to get pods for detailed status",
			"poll", pollCount,
			"error", err)
		return
	}

	logger.V(1).Info("Pod count for StatefulSet",
		"poll", pollCount,
		"totalPods", len(pods.Items))

	for i, pod := range pods.Items {
		logger.V(1).Info("Pod detailed status",
			"poll", pollCount,
			"podIndex", i,
			"podName", pod.Name,
			"phase", pod.Status.Phase,
			"message", pod.Status.Message,
			"reason", pod.Status.Reason,
			"deletionTimestamp", pod.DeletionTimestamp,
			"creationTimestamp", pod.CreationTimestamp)

		// Log container statuses
		for j, containerStatus := range pod.Status.ContainerStatuses {
			logger.V(1).Info("Container status",
				"poll", pollCount,
				"podName", pod.Name,
				"containerIndex", j,
				"containerName", containerStatus.Name,
				"ready", containerStatus.Ready,
				"started", containerStatus.Started,
				"restartCount", containerStatus.RestartCount,
				"image", containerStatus.Image)

			// Log waiting state details if present
			if containerStatus.State.Waiting != nil {
				logger.V(1).Info("Container waiting state",
					"poll", pollCount,
					"podName", pod.Name,
					"containerName", containerStatus.Name,
					"reason", containerStatus.State.Waiting.Reason,
					"message", containerStatus.State.Waiting.Message)
			}

			// Log terminated state details if present
			if containerStatus.State.Terminated != nil {
				logger.V(1).Info("Container terminated state",
					"poll", pollCount,
					"podName", pod.Name,
					"containerName", containerStatus.Name,
					"reason", containerStatus.State.Terminated.Reason,
					"message", containerStatus.State.Terminated.Message,
					"exitCode", containerStatus.State.Terminated.ExitCode)
			}
		}

		// Log pod conditions
		for k, condition := range pod.Status.Conditions {
			logger.V(1).Info("Pod condition",
				"poll", pollCount,
				"podName", pod.Name,
				"conditionIndex", k,
				"type", condition.Type,
				"status", condition.Status,
				"reason", condition.Reason,
				"message", condition.Message)
		}
	}

	// Also log recent events for the pods
	tc.logPodEvents(nbMeta, pollCount, logger)
}

// logPodEvents logs recent events related to the notebook pods
func (tc *testContext) logPodEvents(nbMeta *metav1.ObjectMeta, pollCount int, logger logr.Logger) {
	events, err := tc.kubeClient.CoreV1().Events(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		FieldSelector: "involvedObject.kind=Pod",
	})
	if err != nil {
		logger.V(1).Info("Failed to get events",
			"poll", pollCount,
			"error", err)
		return
	}

	// Filter events related to our notebook pods
	relevantEvents := []v1.Event{}
	for _, event := range events.Items {
		if strings.Contains(event.InvolvedObject.Name, nbMeta.Name) {
			relevantEvents = append(relevantEvents, event)
		}
	}

	if len(relevantEvents) > 0 {
		logger.V(1).Info("Recent pod events found",
			"poll", pollCount,
			"eventCount", len(relevantEvents))

		for i, event := range relevantEvents {
			logger.V(1).Info("Pod event",
				"poll", pollCount,
				"eventIndex", i,
				"podName", event.InvolvedObject.Name,
				"type", event.Type,
				"reason", event.Reason,
				"message", event.Message,
				"count", event.Count,
				"firstTimestamp", event.FirstTimestamp,
				"lastTimestamp", event.LastTimestamp)
		}
	}
}

// logFinalStatefulSetAndPodStatus logs comprehensive status when StatefulSet validation fails
func (tc *testContext) logFinalStatefulSetAndPodStatus(nbMeta *metav1.ObjectMeta, logger logr.Logger) {
	logger.Info("Logging final StatefulSet and Pod status for debugging")

	// Get StatefulSet details
	ss, err := tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(tc.ctx, nbMeta.Name, metav1.GetOptions{})
	if err != nil {
		logger.Info("Failed to get StatefulSet for final status",
			"error", err)
	} else {
		logger.Info("Final StatefulSet status",
			"name", ss.Name,
			"replicas", ss.Status.Replicas,
			"readyReplicas", ss.Status.ReadyReplicas,
			"availableReplicas", ss.Status.AvailableReplicas,
			"currentReplicas", ss.Status.CurrentReplicas,
			"updatedReplicas", ss.Status.UpdatedReplicas,
			"observedGeneration", ss.Status.ObservedGeneration)

		// Log final conditions
		for _, condition := range ss.Status.Conditions {
			logger.Info("Final StatefulSet condition",
				"type", condition.Type,
				"status", condition.Status,
				"reason", condition.Reason,
				"message", condition.Message)
		}
	}

	// Get pod details
	pods, err := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("statefulset=%s", nbMeta.Name),
	})
	if err != nil {
		logger.Info("Failed to get pods for final status",
			"error", err)
		return
	}

	logger.Info("Final pod status summary",
		"totalPods", len(pods.Items))

	for _, pod := range pods.Items {
		logger.Info("Final pod status",
			"podName", pod.Name,
			"phase", pod.Status.Phase,
			"message", pod.Status.Message,
			"reason", pod.Status.Reason,
			"startTime", pod.Status.StartTime,
			"deletionTimestamp", pod.DeletionTimestamp)

		// Log final container statuses
		for _, containerStatus := range pod.Status.ContainerStatuses {
			logger.Info("Final container status",
				"podName", pod.Name,
				"containerName", containerStatus.Name,
				"ready", containerStatus.Ready,
				"restartCount", containerStatus.RestartCount,
				"image", containerStatus.Image)

			if containerStatus.State.Waiting != nil {
				logger.Info("Final container waiting state",
					"podName", pod.Name,
					"containerName", containerStatus.Name,
					"reason", containerStatus.State.Waiting.Reason,
					"message", containerStatus.State.Waiting.Message)
			}

			if containerStatus.State.Terminated != nil {
				logger.Info("Final container terminated state",
					"podName", pod.Name,
					"containerName", containerStatus.Name,
					"reason", containerStatus.State.Terminated.Reason,
					"message", containerStatus.State.Terminated.Message,
					"exitCode", containerStatus.State.Terminated.ExitCode)
			}
		}
	}

	// Log all recent events one final time
	events, err := tc.kubeClient.CoreV1().Events(tc.testNamespace).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.Info("Failed to get events for final status",
			"error", err)
		return
	}

	relevantEvents := []v1.Event{}
	for _, event := range events.Items {
		if strings.Contains(event.InvolvedObject.Name, nbMeta.Name) ||
			(event.InvolvedObject.Kind == "StatefulSet" && event.InvolvedObject.Name == nbMeta.Name) {
			relevantEvents = append(relevantEvents, event)
		}
	}

	if len(relevantEvents) > 0 {
		logger.Info("Final events summary",
			"relevantEventCount", len(relevantEvents))

		for _, event := range relevantEvents {
			logger.Info("Final event",
				"objectKind", event.InvolvedObject.Kind,
				"objectName", event.InvolvedObject.Name,
				"type", event.Type,
				"reason", event.Reason,
				"message", event.Message,
				"count", event.Count,
				"lastTimestamp", event.LastTimestamp)
		}
	}
}

// logClusterResourceAvailability logs cluster resource availability to help diagnose resource constraints
func (tc *testContext) logClusterResourceAvailability(pollCount int, logger logr.Logger) {
	logger.V(1).Info("Checking cluster resource availability",
		"poll", pollCount)

	// Get all nodes in the cluster
	nodes, err := tc.kubeClient.CoreV1().Nodes().List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get cluster nodes",
			"poll", pollCount,
			"error", err)
		return
	}

	logger.V(1).Info("Cluster node summary",
		"poll", pollCount,
		"totalNodes", len(nodes.Items))

	var totalAllocatableCPU, totalAllocatableMemory, totalCapacityCPU, totalCapacityMemory resource.Quantity
	var readyNodes, schedulableNodes int

	for i, node := range nodes.Items {
		// Check node conditions
		var nodeReady, nodeSchedulable bool = false, true
		for _, condition := range node.Status.Conditions {
			if condition.Type == v1.NodeReady && condition.Status == v1.ConditionTrue {
				nodeReady = true
			}
		}
		if node.Spec.Unschedulable {
			nodeSchedulable = false
		}

		if nodeReady {
			readyNodes++
		}
		if nodeSchedulable && nodeReady {
			schedulableNodes++
		}

		logger.V(1).Info("Node resource details",
			"poll", pollCount,
			"nodeIndex", i,
			"nodeName", node.Name,
			"ready", nodeReady,
			"schedulable", nodeSchedulable,
			"unschedulable", node.Spec.Unschedulable,
			"capacityCPU", node.Status.Capacity.Cpu().String(),
			"capacityMemory", node.Status.Capacity.Memory().String(),
			"allocatableCPU", node.Status.Allocatable.Cpu().String(),
			"allocatableMemory", node.Status.Allocatable.Memory().String())

		// Log node conditions that might affect scheduling
		for _, condition := range node.Status.Conditions {
			if condition.Status != v1.ConditionTrue {
				logger.V(1).Info("Node condition issue",
					"poll", pollCount,
					"nodeName", node.Name,
					"conditionType", condition.Type,
					"status", condition.Status,
					"reason", condition.Reason,
					"message", condition.Message)
			}
		}

		// Log node taints that might prevent scheduling
		if len(node.Spec.Taints) > 0 {
			for _, taint := range node.Spec.Taints {
				logger.V(1).Info("Node taint",
					"poll", pollCount,
					"nodeName", node.Name,
					"key", taint.Key,
					"value", taint.Value,
					"effect", taint.Effect)
			}
		}

		// Add to totals (only for schedulable and ready nodes)
		if nodeSchedulable && nodeReady {
			if cpu := node.Status.Allocatable.Cpu(); cpu != nil {
				totalAllocatableCPU.Add(*cpu)
			}
			if memory := node.Status.Allocatable.Memory(); memory != nil {
				totalAllocatableMemory.Add(*memory)
			}
			if cpu := node.Status.Capacity.Cpu(); cpu != nil {
				totalCapacityCPU.Add(*cpu)
			}
			if memory := node.Status.Capacity.Memory(); memory != nil {
				totalCapacityMemory.Add(*memory)
			}
		}
	}

	logger.V(1).Info("Cluster resource summary",
		"poll", pollCount,
		"readyNodes", readyNodes,
		"schedulableNodes", schedulableNodes,
		"totalCapacityCPU", totalCapacityCPU.String(),
		"totalCapacityMemory", totalCapacityMemory.String(),
		"totalAllocatableCPU", totalAllocatableCPU.String(),
		"totalAllocatableMemory", totalAllocatableMemory.String())

	// Get resource usage from all pods in the cluster to calculate available resources
	tc.logClusterResourceUsage(pollCount, logger, totalAllocatableCPU, totalAllocatableMemory)

	// Log specific namespace resource usage
	tc.logNamespaceResourceUsage(pollCount, logger)
}

// logClusterResourceUsage logs current resource usage across the cluster
func (tc *testContext) logClusterResourceUsage(pollCount int, logger logr.Logger, totalAllocatableCPU, totalAllocatableMemory resource.Quantity) {
	// Get all pods in the cluster to calculate resource usage
	allPods, err := tc.kubeClient.CoreV1().Pods("").List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get all cluster pods for resource usage",
			"poll", pollCount,
			"error", err)
		return
	}

	var usedCPU, usedMemory resource.Quantity
	var runningPods, pendingPods, failedPods int

	for _, pod := range allPods.Items {
		switch pod.Status.Phase {
		case v1.PodRunning:
			runningPods++
			// Add resource requests for running pods
			for _, container := range pod.Spec.Containers {
				if cpu := container.Resources.Requests.Cpu(); cpu != nil {
					usedCPU.Add(*cpu)
				}
				if memory := container.Resources.Requests.Memory(); memory != nil {
					usedMemory.Add(*memory)
				}
			}
		case v1.PodPending:
			pendingPods++
		case v1.PodFailed:
			failedPods++
		}
	}

	// Calculate available resources
	availableCPU := totalAllocatableCPU.DeepCopy()
	availableCPU.Sub(usedCPU)
	availableMemory := totalAllocatableMemory.DeepCopy()
	availableMemory.Sub(usedMemory)

	logger.V(1).Info("Cluster resource usage",
		"poll", pollCount,
		"totalPods", len(allPods.Items),
		"runningPods", runningPods,
		"pendingPods", pendingPods,
		"failedPods", failedPods,
		"usedCPU", usedCPU.String(),
		"usedMemory", usedMemory.String(),
		"availableCPU", availableCPU.String(),
		"availableMemory", availableMemory.String())

	// Calculate utilization percentages
	if !totalAllocatableCPU.IsZero() {
		cpuUtilization := float64(usedCPU.MilliValue()) / float64(totalAllocatableCPU.MilliValue()) * 100
		logger.V(1).Info("Cluster CPU utilization",
			"poll", pollCount,
			"utilizationPercent", fmt.Sprintf("%.2f%%", cpuUtilization))
	}

	if !totalAllocatableMemory.IsZero() {
		memoryUtilization := float64(usedMemory.Value()) / float64(totalAllocatableMemory.Value()) * 100
		logger.V(1).Info("Cluster memory utilization",
			"poll", pollCount,
			"utilizationPercent", fmt.Sprintf("%.2f%%", memoryUtilization))
	}

	// Log pending pods with resource requests to understand scheduling pressure
	pendingPodsWithResources := 0
	var pendingCPURequests, pendingMemoryRequests resource.Quantity

	for _, pod := range allPods.Items {
		if pod.Status.Phase == v1.PodPending {
			hasCPURequest := false
			hasMemoryRequest := false
			for _, container := range pod.Spec.Containers {
				if cpu := container.Resources.Requests.Cpu(); cpu != nil && !cpu.IsZero() {
					pendingCPURequests.Add(*cpu)
					hasCPURequest = true
				}
				if memory := container.Resources.Requests.Memory(); memory != nil && !memory.IsZero() {
					pendingMemoryRequests.Add(*memory)
					hasMemoryRequest = true
				}
			}
			if hasCPURequest || hasMemoryRequest {
				pendingPodsWithResources++
			}
		}
	}

	if pendingPodsWithResources > 0 {
		logger.V(1).Info("Pending pods resource pressure",
			"poll", pollCount,
			"pendingPodsWithResources", pendingPodsWithResources,
			"pendingCPURequests", pendingCPURequests.String(),
			"pendingMemoryRequests", pendingMemoryRequests.String())
	}
}

// logNamespaceResourceUsage logs resource usage specifically in the test namespace
func (tc *testContext) logNamespaceResourceUsage(pollCount int, logger logr.Logger) {
	namespacePods, err := tc.kubeClient.CoreV1().Pods(tc.testNamespace).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get namespace pods for resource usage",
			"poll", pollCount,
			"namespace", tc.testNamespace,
			"error", err)
		return
	}

	var namespaceCPU, namespaceMemory resource.Quantity
	var namespaceRunning, namespacePending, namespaceFailed int

	for _, pod := range namespacePods.Items {
		switch pod.Status.Phase {
		case v1.PodRunning:
			namespaceRunning++
		case v1.PodPending:
			namespacePending++
		case v1.PodFailed:
			namespaceFailed++
		}

		// Add resource requests
		for _, container := range pod.Spec.Containers {
			if cpu := container.Resources.Requests.Cpu(); cpu != nil {
				namespaceCPU.Add(*cpu)
			}
			if memory := container.Resources.Requests.Memory(); memory != nil {
				namespaceMemory.Add(*memory)
			}
		}
	}

	logger.V(1).Info("Test namespace resource usage",
		"poll", pollCount,
		"namespace", tc.testNamespace,
		"totalPods", len(namespacePods.Items),
		"runningPods", namespaceRunning,
		"pendingPods", namespacePending,
		"failedPods", namespaceFailed,
		"totalCPURequests", namespaceCPU.String(),
		"totalMemoryRequests", namespaceMemory.String())

	// Log details of any pending pods in the namespace
	for _, pod := range namespacePods.Items {
		if pod.Status.Phase == v1.PodPending {
			logger.V(1).Info("Pending pod in test namespace",
				"poll", pollCount,
				"namespace", tc.testNamespace,
				"podName", pod.Name,
				"reason", pod.Status.Reason,
				"message", pod.Status.Message)

			// Log specific container resource requests for pending pods
			for _, container := range pod.Spec.Containers {
				logger.V(1).Info("Pending pod container resources",
					"poll", pollCount,
					"podName", pod.Name,
					"containerName", container.Name,
					"cpuRequest", container.Resources.Requests.Cpu().String(),
					"memoryRequest", container.Resources.Requests.Memory().String(),
					"cpuLimit", container.Resources.Limits.Cpu().String(),
					"memoryLimit", container.Resources.Limits.Memory().String())
			}
		}
	}

	// Check for resource quotas in the namespace
	tc.logNamespaceResourceQuotas(pollCount, logger)
}

// logNamespaceResourceQuotas logs any resource quotas that might be limiting pod creation
func (tc *testContext) logNamespaceResourceQuotas(pollCount int, logger logr.Logger) {
	resourceQuotas, err := tc.kubeClient.CoreV1().ResourceQuotas(tc.testNamespace).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get resource quotas",
			"poll", pollCount,
			"namespace", tc.testNamespace,
			"error", err)
		return
	}

	if len(resourceQuotas.Items) == 0 {
		logger.V(1).Info("No resource quotas found in namespace",
			"poll", pollCount,
			"namespace", tc.testNamespace)
		return
	}

	for i, quota := range resourceQuotas.Items {
		logger.V(1).Info("Resource quota details",
			"poll", pollCount,
			"namespace", tc.testNamespace,
			"quotaIndex", i,
			"quotaName", quota.Name)

		// Log hard limits
		for resource, limit := range quota.Status.Hard {
			used := quota.Status.Used[resource]
			logger.V(1).Info("Resource quota limit",
				"poll", pollCount,
				"quotaName", quota.Name,
				"resource", resource,
				"limit", limit.String(),
				"used", used.String())
		}
	}

	// Check for limit ranges that might affect pod scheduling
	limitRanges, err := tc.kubeClient.CoreV1().LimitRanges(tc.testNamespace).List(tc.ctx, metav1.ListOptions{})
	if err != nil {
		logger.V(1).Info("Failed to get limit ranges",
			"poll", pollCount,
			"namespace", tc.testNamespace,
			"error", err)
		return
	}

	for i, limitRange := range limitRanges.Items {
		logger.V(1).Info("Limit range details",
			"poll", pollCount,
			"namespace", tc.testNamespace,
			"limitRangeIndex", i,
			"limitRangeName", limitRange.Name)

		for j, limit := range limitRange.Spec.Limits {
			logger.V(1).Info("Limit range specification",
				"poll", pollCount,
				"limitRangeName", limitRange.Name,
				"limitIndex", j,
				"type", limit.Type,
				"defaultCPU", limit.Default.Cpu().String(),
				"defaultMemory", limit.Default.Memory().String(),
				"maxCPU", limit.Max.Cpu().String(),
				"maxMemory", limit.Max.Memory().String(),
				"minCPU", limit.Min.Cpu().String(),
				"minMemory", limit.Min.Memory().String())
		}
	}
}
