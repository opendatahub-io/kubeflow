package e2e

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"log"
	"math/big"
	"net/http"
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

const (
	// letterRunes contains the characters used for random string generation
	letterRunes = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// generateRandomString generates a random string of the specified length
// using alphanumeric characters (0-9, A-Z, a-z)
func generateRandomString(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("length must be greater than 0")
	}

	randomValue := make([]byte, length)
	for i := 0; i < length; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(letterRunes))))
		if err != nil {
			return "", fmt.Errorf("error generating random string: %w", err)
		}
		randomValue[i] = letterRunes[num.Int64()]
	}

	return string(randomValue), nil
}

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
	// Verify StatefulSet is running expected number of replicas
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		// Find StatefulSet by looking for one that has a selector matching "statefulset": notebook.Name
		// This works for both regular names and generated names (when notebook name is too long)
		namespacedStatefulSets := &appsv1.StatefulSetList{}
		err1 := tc.customClient.List(ctx, namespacedStatefulSets, client.InNamespace(tc.testNamespace))
		if err1 != nil {
			log.Printf("Failed to list StatefulSets in namespace %s", tc.testNamespace)
			return false, err1
		}

		// Find the StatefulSet that has a selector for "statefulset": nbMeta.Name
		var notebookStatefulSet *appsv1.StatefulSet
		for _, sts := range namespacedStatefulSets.Items {
			if sts.Spec.Selector != nil && sts.Spec.Selector.MatchLabels != nil {
				if statefulsetLabel, exists := sts.Spec.Selector.MatchLabels["statefulset"]; exists && statefulsetLabel == nbMeta.Name {
					notebookStatefulSet = &sts
					break
				}
			}
		}

		if notebookStatefulSet == nil {
			return false, nil // StatefulSet not found yet
		}

		if notebookStatefulSet.Status.AvailableReplicas == availableReplicas &&
			notebookStatefulSet.Status.ReadyReplicas == readyReplicas {
			return true, nil
		}
		return false, nil
	})
	return err
}

func (tc *testContext) revertCullingConfiguration(cmMeta metav1.ObjectMeta, depMeta metav1.ObjectMeta, nbMeta *metav1.ObjectMeta) {
	// Delete the culling configuration Configmap once the test is completed
	err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Delete(tc.ctx,
		cmMeta.Name, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("error deleting configmap notebook-controller-culler-config: %v ", err)
	}
	// Roll out the controller deployment
	err = tc.rolloutDeployment(depMeta)
	if err != nil {
		log.Printf("error rolling out the deployment %v: %v ", depMeta.Name, err)
	}

	// IMPORTANT: Culling affects ALL notebooks in the namespace, not just the test target
	// The restartAllCulledNotebooks function will handle restarting this notebook and any others that were culled
	err = tc.restartAllCulledNotebooks()
	if err != nil {
		log.Printf("Warning: Failed to restart other culled notebooks: %v", err)
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
			log.Printf("Failed to patch notebook %s: %v", notebook.Name, err)
			continue
		}

		// Wait for the notebook to become ready
		nbMeta := &metav1.ObjectMeta{Name: notebook.Name, Namespace: notebook.Namespace}
		if waitErr := tc.waitForStatefulSet(nbMeta, 1, 1); waitErr != nil {
			log.Printf("Warning: Notebook %s didn't become ready within 2 minutes: %v", notebook.Name, waitErr)
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
	// Let's use long name to test the generated route name https://issues.redhat.com/browse/RHOAIENG-33609
	// Route threshold: name + namespace > 63 chars
	// "thoth-oauth-custom-resources-notebook" (38) + "odh-notebook-controller-system" (31) = 69
	// If total is less than 63, add random characters to exceed the threshold (eg. default namespace name is overrided during the tests execution)
	testNotebookName := "thoth-oauth-custom-resources-notebook"
	currentLength := len(testNotebookName) + len(notebookTestNamespace)
	if currentLength < 63 {
		// Calculate how many characters we need to add to exceed 63 (add 1 to ensure we exceed, not just reach)
		charsNeeded := 63 - currentLength + 1
		randomSuffix, err := generateRandomString(charsNeeded)
		if err != nil {
			log.Fatalf("Failed to generate random string for test notebook name: %v", err)
		}
		testNotebookName = testNotebookName + "-" + randomSuffix
	}

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
