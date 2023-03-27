package e2e

import (
	"crypto/tls"
	"fmt"
	netv1 "k8s.io/api/networking/v1"
	"log"
	"net/http"
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (tc *testContext) waitForControllerDeployment(name string, replicas int32) error {
	err := wait.Poll(tc.resourceRetryInterval, tc.resourceCreationTimeout, func() (done bool, err error) {

		controllerDeployment, err := tc.kubeClient.AppsV1().Deployments(tc.testNamespace).Get(tc.ctx, name, metav1.GetOptions{})

		if err != nil {
			if errors.IsNotFound(err) {
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

func (tc *testContext) getNotebookRoute(nbMeta *metav1.ObjectMeta) (*routev1.Route, error) {
	nbRouteList := routev1.RouteList{}
	opts := []client.ListOption{
		client.InNamespace(nbMeta.Namespace),
		client.MatchingLabels{"notebook-name": nbMeta.Name}}
	err := wait.Poll(tc.resourceRetryInterval, tc.resourceCreationTimeout, func() (done bool, err error) {
		routeErr := tc.customClient.List(tc.ctx, &nbRouteList, opts...)
		if routeErr != nil {
			log.Printf("error retrieving Notebook route %v", err)
			return false, nil
		} else {
			return true, nil
		}
	})

	if len(nbRouteList.Items) == 0 {
		return nil, fmt.Errorf("no Notebook route found")
	}

	return &nbRouteList.Items[0], err
}

func (tc *testContext) getNotebookNetworkpolicy(nbMeta *metav1.ObjectMeta, name string) (*netv1.NetworkPolicy, error) {
	nbNetworkPolicy := &netv1.NetworkPolicy{}
	err := wait.Poll(tc.resourceRetryInterval, tc.resourceCreationTimeout, func() (done bool, err error) {
		np, npErr := tc.kubeClient.NetworkingV1().NetworkPolicies(nbMeta.Namespace).Get(tc.ctx, name, metav1.GetOptions{})
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
	nbRoute, err := tc.getNotebookRoute(&nbMeta)
	if err != nil {
		return nil, err
	}
	// Access the Notebook endpoint using http request
	notebookEndpoint := "https://" + nbRoute.Spec.Host + "/notebook/" +
		nbRoute.Namespace + "/" + nbRoute.Name + "/api"

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}

	req, err := http.NewRequest("GET", notebookEndpoint, nil)
	if err != nil {
		return nil, err
	}

	return client.Do(req)
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

func (tc *testContext) revertCullingConfiguration(cmMeta metav1.ObjectMeta, depMeta metav1.ObjectMeta) {
	// Delete the culling configuration Configmap once the test is completed
	err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Delete(tc.ctx,
		cmMeta.Name, metav1.DeleteOptions{})
	if err != nil {
		log.Printf("error creating configmap notebook-controller-culler-config: %v ", err)
	}
	// Roll out the controller deployment
	err = tc.rolloutDeployment(depMeta)
	if err != nil {
		log.Printf("error rolling out the deployment %v: %v ", depMeta.Name, err)
	}
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
							Name:       "thoth-minimal-oauth-notebook",
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
