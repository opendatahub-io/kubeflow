package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testNotebookControllerValidation(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	t.Run("Validate Kubeflow notebook controller", testCtx.testKubeflowNotebookController)
	t.Run("Validate ODH notebook controller", testCtx.testODHNotebookController)
}

func (tc *testContext) testKubeflowNotebookController(t *testing.T) {
	// Verify if notebook-controller configmap is created and has expected data
	configMaplist, err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).List(tc.ctx,
		metav1.ListOptions{})
	require.NoErrorf(t, err, "error getting configmaps from controller namespace")

	notebookConfigFound := false
	for _, configmap := range configMaplist.Items {
		if strings.Contains(configmap.Name, "notebook-controller-config") {
			notebookConfigFound = true
			// Verify if the configmap has key ADD_FSGROUP with value 'false'
			require.EqualValuesf(t, "false", configmap.Data["ADD_FSGROUP"],
				"error getting ADD_FSGROUP in the configmap")
			// Verify if the configmap has key USE_ISTIO with value 'false'
			require.EqualValuesf(t, "false", configmap.Data["USE_ISTIO"],
				"error getting USE_ISTIO in the configmap")
			// Verify if the configmap has key ISTIO_GATEWAY with value 'kubeflow/kubeflow-gateway'
			require.EqualValuesf(t, "kubeflow/kubeflow-gateway",
				configmap.Data["ISTIO_GATEWAY"], "error getting ISTIO_GATEWAY in the configmap")
		}

	}
	require.EqualValuesf(t, true, notebookConfigFound,
		"error getting notebook-controller configmap")

	// Verify if the controller pod is running
	require.NoErrorf(t, tc.waitForControllerDeployment("notebook-controller-deployment", 1),
		"error in validating notebook controller")

	// Verify if the Notebook CRD is deployed
	require.NoErrorf(t, tc.isNotebookCRD(), "error verifying Notebook CRD")
}

func (tc *testContext) testODHNotebookController(t *testing.T) {
	// Verify if the controller pod is running
	require.NoErrorf(t, tc.waitForControllerDeployment("odh-notebook-controller-manager", 1),
		"error in validating odh notebook controller")
}
