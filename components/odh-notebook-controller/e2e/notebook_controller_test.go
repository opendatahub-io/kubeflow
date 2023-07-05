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
			break
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
