package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

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

		logger.V(1).Info("StatefulSet status",
			"poll", pollCount,
			"availableReplicas", notebookStatefulSet.Status.AvailableReplicas,
			"readyReplicas", notebookStatefulSet.Status.ReadyReplicas)

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
