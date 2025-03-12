package e2e

import (
	"context"
	"fmt"
	"testing"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func updateTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	notebooksForSelectedDeploymentMode := notebooksForScenario(testCtx.testNotebooks, deploymentMode)
	for _, nbContext := range notebooksForSelectedDeploymentMode {
		// prepend Notebook name to every subtest
		t.Run(nbContext.nbObjectMeta.Name, func(t *testing.T) {
			t.Run("Update Notebook instance", func(t *testing.T) {
				err = testCtx.testNotebookUpdate(nbContext)
				require.NoError(t, err, "error updating Notebook object ")
			})
			t.Run("Notebook Route Validation After Update", func(t *testing.T) {
				if deploymentMode == ServiceMesh {
					t.Skipf("Skipping as it's not relevant for Service Mesh scenario")
				}
				err = testCtx.testNotebookRouteCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Route for Notebook after update ")
			})

			t.Run("Notebook Network Policies Validation After Update", func(t *testing.T) {
				err = testCtx.testNetworkPolicyCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Network Policies for Notebook after update ")
			})

			t.Run("Notebook Statefulset Validation After Update", func(t *testing.T) {
				err = testCtx.testNotebookValidation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing StatefulSet for Notebook after update ")
			})

			t.Run("Notebook OAuth sidecar Validation After Update", func(t *testing.T) {
				if deploymentMode == ServiceMesh {
					t.Skipf("Skipping as it's not relevant for Service Mesh scenario")
				}
				err = testCtx.testNotebookOAuthSidecar(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing sidecar for Notebook after update ")
			})

			t.Run("Verify Notebook Traffic After Update", func(t *testing.T) {
				err = testCtx.testNotebookTraffic(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Notebook traffic after update ")
			})
		})
	}
}

func (tc *testContext) testNotebookUpdate(nbContext notebookContext) error {
	notebookLookupKey := types.NamespacedName{Name: nbContext.nbObjectMeta.Name, Namespace: nbContext.nbObjectMeta.Namespace}
	updatedNotebook := &nbv1.Notebook{}

	err := tc.customClient.Get(tc.ctx, notebookLookupKey, updatedNotebook)
	if err != nil {
		return fmt.Errorf("error getting Notebook %s: %v", nbContext.nbObjectMeta.Name, err)
	}

	// Example update: Change the Notebook image
	newImage := "quay.io/opendatahub/workbench-images:jupyter-minimal-ubi9-python-3.11-20241119-3ceb400"
	updatedNotebook.Spec.Template.Spec.Containers[0].Image = newImage

	err = tc.customClient.Update(tc.ctx, updatedNotebook)
	if err != nil {
		return fmt.Errorf("error updating Notebook %s: %v", updatedNotebook.Name, err)
	}

	// Wait for the update to be applied
	err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		note := &nbv1.Notebook{}
		err = tc.customClient.Get(ctx, notebookLookupKey, note)
		if err != nil {
			return false, err
		}
		if note.Spec.Template.Spec.Containers[0].Image == newImage {
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		return fmt.Errorf("error validating notebook update: %s", err)
	}
	return nil
}
