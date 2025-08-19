package e2e

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"testing"
	"time"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

	testNotebook := &nbv1.Notebook{
		ObjectMeta: *nbContext.nbObjectMeta,
		Spec:       *nbContext.nbSpec,
	}

	// Create test Notebook resource if not already created
	notebookLookupKey := types.NamespacedName{Name: testNotebook.Name, Namespace: testNotebook.Namespace}
	createdNotebook := nbv1.Notebook{}

	err := tc.customClient.Get(tc.ctx, notebookLookupKey, &createdNotebook)
	if err != nil {
		if errors.IsNotFound(err) {
			nberr := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
				creationErr := tc.customClient.Create(ctx, testNotebook)
				if creationErr != nil {
					log.Printf("Error creating Notebook resource %v: %v, trying again",
						testNotebook.Name, creationErr)
					return false, nil
				} else {
					return true, nil
				}
			})
			if nberr != nil {
				return fmt.Errorf("error creating test Notebook %s: %v", testNotebook.Name, nberr)
			}
		} else {
			return fmt.Errorf("error getting test Notebook %s: %v", testNotebook.Name, err)
		}
	}
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

	_, err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Create(tc.ctx, cullingConfigMap,
		metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating configmapnotebook-controller-culler-config: %v", err)
	}
	// Restart the deployment to get changes from configmap
	controllerDeployment, err := tc.kubeClient.AppsV1().Deployments(tc.testNamespace).Get(tc.ctx,
		"notebook-controller-deployment", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting deployment %v: %v", controllerDeployment.Name, err)
	}

	defer tc.revertCullingConfiguration(cullingConfigMap.ObjectMeta, controllerDeployment.ObjectMeta, nbMeta)

	err = tc.rolloutDeployment(controllerDeployment.ObjectMeta)
	if err != nil {
		return fmt.Errorf("error rolling out the deployment with culling configuration: %v", err)
	}

	// Wait for server to shut down after 'CULL_IDLE_TIME' minutes(around 3 minutes)
	time.Sleep(180 * time.Second)

	// Debug: Check if the notebook has been marked for culling
	notebookLookupKey := types.NamespacedName{Name: nbMeta.Name, Namespace: nbMeta.Namespace}
	notebook := &nbv1.Notebook{}
	if err := tc.customClient.Get(tc.ctx, notebookLookupKey, notebook); err == nil {
		log.Printf("Notebook annotations after culling wait: %+v", notebook.Annotations)
		if _, exists := notebook.Annotations["kubeflow-resource-stopped"]; exists {
			log.Printf("✓ Notebook is marked for culling")
		} else {
			log.Printf("✗ Notebook is NOT marked for culling")
		}
	}

	// Verify that the notebook kernel has shutdown, and the notebook endpoint returns 503
	resp, err := tc.curlNotebookEndpoint(*nbMeta)
	if err != nil {
		return fmt.Errorf("error accessing Notebook Endpoint with 503: %v ", err)
	}
	if resp.StatusCode != 503 {
		return errorWithBody(resp)
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
