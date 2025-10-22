package e2e

import (
	"context"
	"fmt"
	"testing"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

func updateTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	for _, nbContext := range testCtx.testNotebooks {
		// prepend Notebook name to every subtest
		t.Run(nbContext.nbObjectMeta.Name, func(t *testing.T) {
			t.Run("Update Notebook instance", func(t *testing.T) {
				err = testCtx.testNotebookUpdate(nbContext)
				require.NoError(t, err, "error updating Notebook object ")
			})
			t.Run("Notebook HTTPRoute Validation After Update", func(t *testing.T) {
				err = testCtx.testNotebookHTTPRouteCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing HTTPRoute for Notebook after update ")
			})

			t.Run("Notebook Network Policies Validation After Update", func(t *testing.T) {
				err = testCtx.testNetworkPolicyCreation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing Network Policies for Notebook after update ")
			})

			t.Run("Notebook Statefulset Validation After Update", func(t *testing.T) {
				err = testCtx.testNotebookValidation(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing StatefulSet for Notebook after update ")
			})

			t.Run("Notebook RBAC sidecar Validation After Update", func(t *testing.T) {
				err = testCtx.testNotebookRBACProxySidecar(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing RBAC sidecar for Notebook after update ")
			})

			t.Run("Notebook RBAC sidecar Resource Validation After Update", func(t *testing.T) {
				err = testCtx.testNotebookRBACProxySidecarResources(nbContext.nbObjectMeta)
				require.NoError(t, err, "error testing RBAC sidecar resources for Notebook after update ")
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

	// First wait for the notebook spec to be updated
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
		return fmt.Errorf("error waiting for notebook spec update: %s", err)
	}

	// Then wait for the StatefulSet to be updated with the new image
	statefulSetLookupKey := types.NamespacedName{Name: nbContext.nbObjectMeta.Name, Namespace: nbContext.nbObjectMeta.Namespace}
	err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		sts := &appsv1.StatefulSet{}
		err = tc.customClient.Get(ctx, statefulSetLookupKey, sts)
		if err != nil {
			return false, nil // StatefulSet might not exist yet, keep trying
		}

		// Check if StatefulSet has the new image
		if len(sts.Spec.Template.Spec.Containers) > 0 && sts.Spec.Template.Spec.Containers[0].Image == newImage {
			// Also check if the update has been rolled out
			if sts.Status.UpdatedReplicas == sts.Status.Replicas &&
				sts.Status.ReadyReplicas == sts.Status.Replicas &&
				sts.Status.Replicas > 0 {
				return true, nil
			}
		}
		return false, nil
	})

	if err != nil {
		return fmt.Errorf("error waiting for StatefulSet update and rollout to complete: %s", err)
	}
	return nil
}
