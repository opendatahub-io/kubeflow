package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func (tc *testContext) waitForControllerDeployment(name string, replicas int32) error {
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {

		controllerDeployment, err := tc.kubeClient.AppsV1().Deployments(tc.testNamespace).Get(ctx, name, metav1.GetOptions{})

		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			log.Printf("Failed to get %s controller deployment", name)
			return false, err
		}

		for _, condition := range controllerDeployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable {
				if condition.Status == v1.ConditionTrue && controllerDeployment.Status.ReadyReplicas == replicas {
					return true, nil
				}
			}
		}

		log.Printf("Error in %s deployment", name)
		return false, nil

	})
	return err
}

func (tc *testContext) getNotebookHTTPRoute(nbMeta *metav1.ObjectMeta) (*gatewayv1.HTTPRoute, error) {
	nbHTTPRouteList := gatewayv1.HTTPRouteList{}

	var opts []client.ListOption
	opts = append(opts, client.InNamespace(nbMeta.Namespace))
	opts = append(opts, client.MatchingLabels{"notebook-name": nbMeta.Name})
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		routeErr := tc.customClient.List(ctx, &nbHTTPRouteList, opts...)
		if routeErr != nil {
			log.Printf("error retrieving Notebook HTTPRoute %v", err)
			return false, nil
		} else {
			return true, nil
		}
	})

	if len(nbHTTPRouteList.Items) == 0 {
		// Return proper Kubernetes NotFound error
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "gateway.networking.k8s.io", Resource: "httproutes"},
			fmt.Sprintf("notebook-%s", nbMeta.Name),
		)
	}

	return &nbHTTPRouteList.Items[0], err
}

func (tc *testContext) getNotebookNetworkPolicy(nbMeta *metav1.ObjectMeta, name string) (*netv1.NetworkPolicy, error) {
	nbNetworkPolicy := &netv1.NetworkPolicy{}
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		np, npErr := tc.kubeClient.NetworkingV1().NetworkPolicies(nbMeta.Namespace).Get(ctx, name, metav1.GetOptions{})
		if npErr != nil {
			log.Printf("error retrieving Notebook Network policy %v: %v", name, err)
			return false, nil
		} else {
			nbNetworkPolicy = np
			return true, nil
		}
	})

	return nbNetworkPolicy, err
}

func (tc *testContext) curlNotebookEndpoint(nbMeta metav1.ObjectMeta) (*http.Response, error) {
	nbHTTPRoute, err := tc.getNotebookHTTPRoute(&nbMeta)
	if err != nil {
		return nil, err
	}

	// Get the Gateway hostname from the Gateway resource
	// since HTTPRoute doesn't have hostnames set by our controller
	var hostname string
	if len(nbHTTPRoute.Spec.Hostnames) > 0 {
		// Use hostname from HTTPRoute if available
		hostname = string(nbHTTPRoute.Spec.Hostnames[0])
	} else {
		// Try to get hostname from the Gateway resource
		gatewayName := string(nbHTTPRoute.Spec.ParentRefs[0].Name)
		gatewayNamespace := string(*nbHTTPRoute.Spec.ParentRefs[0].Namespace)

		gateway := &gatewayv1.Gateway{}
		err := tc.customClient.Get(tc.ctx, client.ObjectKey{
			Name:      gatewayName,
			Namespace: gatewayNamespace,
		}, gateway)
		if err != nil {
			return nil, fmt.Errorf("unable to get Gateway %s/%s: %v", gatewayNamespace, gatewayName, err)
		}

		// Extract hostname from Gateway status or use a default
		if len(gateway.Status.Addresses) > 0 {
			hostname = gateway.Status.Addresses[0].Value
		} else {
			// If no hostname is available, skip the traffic test
			return nil, fmt.Errorf("no hostname available in Gateway %s/%s status", gatewayNamespace, gatewayName)
		}
	}

	notebookEndpoint := "https://" + hostname + "/notebook/" +
		nbMeta.Namespace + "/" + nbMeta.Name + "/api"

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	req, err := http.NewRequest("GET", notebookEndpoint, nil)
	if err != nil {
		return nil, err
	}

	return httpClient.Do(req)
}

func (tc *testContext) rolloutDeployment(depMeta metav1.ObjectMeta) error {

	// Scale deployment to 0
	err := tc.scaleDeployment(depMeta, int32(0))
	if err != nil {
		return fmt.Errorf("error while scaling down the deployment: %v", err)
	}
	// Wait for deployment to fully scale down
	err = tc.waitForDeploymentReplicas(depMeta, 0)
	if err != nil {
		return fmt.Errorf("error waiting for deployment to scale down: %v", err)
	}

	// Scale deployment to 1
	err = tc.scaleDeployment(depMeta, int32(1))
	if err != nil {
		return fmt.Errorf("error while scaling up the deployment: %v", err)
	}
	// Wait for deployment to be available
	err = tc.waitForControllerDeployment(depMeta.Name, 1)
	if err != nil {
		return fmt.Errorf("error waiting for deployment to become available: %v", err)
	}
	return nil
}

func (tc *testContext) waitForDeploymentReplicas(depMeta metav1.ObjectMeta, replicas int32) error {
	return wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		deployment, err := tc.kubeClient.AppsV1().Deployments(depMeta.Namespace).Get(ctx, depMeta.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			log.Printf("Failed to get %s deployment", depMeta.Name)
			return false, err
		}
		return deployment.Status.Replicas == replicas && deployment.Status.ReadyReplicas == replicas, nil
	})
}

func (tc *testContext) waitForStatefulSet(nbMeta *metav1.ObjectMeta, availableReplicas int32, readyReplicas int32) error {
	// Verify StatefulSet is running expected number of replicas
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		notebookStatefulSet, err1 := tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(ctx,
			nbMeta.Name, metav1.GetOptions{})

		if err1 != nil {
			if apierrors.IsNotFound(err1) {
				return false, nil
			} else {
				log.Printf("Failed to get %s statefulset", nbMeta.Name)
				return false, err1
			}
		}
		if notebookStatefulSet.Status.AvailableReplicas == availableReplicas &&
			notebookStatefulSet.Status.ReadyReplicas == readyReplicas {
			return true, nil
		}
		return false, nil
	})
	return err
}

func (tc *testContext) revertCullingConfiguration(cmMeta metav1.ObjectMeta, depMeta metav1.ObjectMeta) error {
	// Delete the culling configuration Configmap once the test is completed
	err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Delete(tc.ctx,
		cmMeta.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		log.Printf("Configmap %s already deleted, continuing with cleanup", cmMeta.Name)
	} else if err != nil {
		return fmt.Errorf("error deleting configmap %s: %v", cmMeta.Name, err)
	}
	// Roll out the controller deployment
	err = tc.rolloutDeployment(depMeta)
	if err != nil {
		return fmt.Errorf("error rolling out the deployment %s: %v", depMeta.Name, err)
	}

	// IMPORTANT: Culling affects ALL notebooks in the namespace, not just the test target
	// The restartAllCulledNotebooks function will handle restarting any notebooks that were culled
	err = tc.restartAllCulledNotebooks()
	if err != nil {
		return fmt.Errorf("failed to restart culled notebooks: %v", err)
	}
	return nil
}

// restartNotebook removes the kubeflow-resource-stopped annotation from a notebook
// and waits for its StatefulSet to reach 1/1 ready replicas with a dedicated recovery timeout.
func (tc *testContext) restartNotebook(name, namespace string) error {
	// Use strategic merge patch with null value so the operation is idempotent —
	// if the annotation is already absent (e.g. removed by a concurrent goroutine),
	// the patch is a no-op instead of returning a 422 error.
	patch := client.RawPatch(types.StrategicMergePatchType, []byte(`{"metadata":{"annotations":{"kubeflow-resource-stopped":null}}}`))
	notebookForPatch := &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	if err := tc.customClient.Patch(tc.ctx, notebookForPatch, patch); err != nil {
		return fmt.Errorf("failed to remove kubeflow-resource-stopped annotation: %v", err)
	}
	log.Printf("Removed kubeflow-resource-stopped annotation from notebook %s", name)

	// Use a dedicated 3-minute recovery timeout for notebooks restarting after culling,
	// which may need extra time for image pulls and pod scheduling on slow CI.
	const recoveryTimeout = 3 * time.Minute
	return wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, recoveryTimeout, false, func(ctx context.Context) (done bool, err error) {
		sts, err := tc.kubeClient.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		return sts.Status.AvailableReplicas == 1 && sts.Status.ReadyReplicas == 1, nil
	})
}

// restartAllCulledNotebooks finds all notebooks with kubeflow-resource-stopped annotation
// and restarts them in parallel using restartNotebook.
func (tc *testContext) restartAllCulledNotebooks() error {
	// List all notebooks in the test namespace
	notebookList := &nbv1.NotebookList{}
	err := tc.customClient.List(tc.ctx, notebookList, client.InNamespace(tc.testNamespace))
	if err != nil {
		return fmt.Errorf("failed to list notebooks: %v", err)
	}

	var culledNotebooks []nbv1.Notebook
	for _, notebook := range notebookList.Items {
		if _, exists := notebook.Annotations["kubeflow-resource-stopped"]; exists {
			culledNotebooks = append(culledNotebooks, notebook)
		}
	}

	if len(culledNotebooks) == 0 {
		return nil
	}

	// Restart all culled notebooks in parallel
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(culledNotebooks))

	for _, notebook := range culledNotebooks {
		go func(nb nbv1.Notebook) {
			results <- result{name: nb.Name, err: tc.restartNotebook(nb.Name, nb.Namespace)}
		}(notebook)
	}

	var failures []string
	for range culledNotebooks {
		r := <-results
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.name, r.err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("failed to restart culled notebooks: %s", strings.Join(failures, "; "))
	}
	return nil
}

// ensureNotebookRunning verifies a notebook's StatefulSet is running. If the notebook
// has a kubeflow-resource-stopped annotation (e.g. left over from culling), it removes
// the annotation and waits for readiness using restartNotebook. If the notebook is not
// stopped, it waits for the StatefulSet with the standard timeout.
func (tc *testContext) ensureNotebookRunning(nbMeta *metav1.ObjectMeta) error {
	notebook := &nbv1.Notebook{}
	err := tc.customClient.Get(tc.ctx, types.NamespacedName{Name: nbMeta.Name, Namespace: nbMeta.Namespace}, notebook)
	if err != nil {
		return fmt.Errorf("failed to get notebook %s: %v", nbMeta.Name, err)
	}

	if _, stopped := notebook.Annotations["kubeflow-resource-stopped"]; stopped {
		if err := tc.restartNotebook(nbMeta.Name, nbMeta.Namespace); err != nil {
			return fmt.Errorf("failed to restart notebook %s: %v", nbMeta.Name, err)
		}
		return nil
	}

	// Notebook is not stopped, just verify StatefulSet is ready
	if err := tc.waitForStatefulSet(nbMeta, 1, 1); err != nil {
		return fmt.Errorf("notebook %s StatefulSet is not ready: %v", nbMeta.Name, err)
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
func setupThothMinimalRbacNotebook() notebookContext {
	testNotebookName := "thoth-minimal-rbac-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"notebooks.opendatahub.io/inject-auth": "true"},
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

	thothMinimalRbacNbContext := notebookContext{
		nbObjectMeta: &testNotebook.ObjectMeta,
		nbSpec:       &testNotebook.Spec,
	}
	return thothMinimalRbacNbContext
}

// Add spec and metadata for Notebook objects with custom rbac proxy resources
func setupThothRbacCustomResourcesNotebook() notebookContext {
	// Too long name - shall be resolved via https://issues.redhat.com/browse/RHOAIENG-33609
	// testNotebookName := "thoth-rbac-custom-resources-notebook-1"
	testNotebookName := "thoth-custom-resources-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"notebooks.opendatahub.io/inject-auth":                 "true",
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

	thothRbacCustomResourcesNbContext := notebookContext{
		nbObjectMeta: &testNotebook.ObjectMeta,
		nbSpec:       &testNotebook.Spec,
	}
	return thothRbacCustomResourcesNbContext
}
