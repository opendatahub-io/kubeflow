package e2e

import (
	"context"
	"fmt"
	"testing"

	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/stretchr/testify/require"
	apiext "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func deletionTestSuite(t *testing.T) {
	testCtx, err := NewTestContext()
	require.NoError(t, err)
	notebooksForSelectedDeploymentMode := notebooksForScenario(testCtx.testNotebooks, deploymentMode)
	for _, nbContext := range notebooksForSelectedDeploymentMode {
		// prepend Notebook name to every subtest
		t.Run(nbContext.nbObjectMeta.Name, func(t *testing.T) {
			t.Run("Notebook Deletion", func(t *testing.T) {
				err = testCtx.testNotebookDeletion(nbContext.nbObjectMeta)
				require.NoError(t, err, "error deleting Notebook object ")
			})
			t.Run("Dependent Resource Deletion", func(t *testing.T) {
				err = testCtx.testNotebookResourcesDeletion(nbContext.nbObjectMeta)
				require.NoError(t, err, "error deleting dependent resources ")
			})
		})
	}
}

func (tc *testContext) testNotebookDeletion(nbMeta *metav1.ObjectMeta) error {
	logger := GetDeletionLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
	)
	logger.Info("Starting notebook deletion test")

	// Delete test Notebook resource if found
	notebookLookupKey := types.NamespacedName{Name: nbMeta.Name, Namespace: nbMeta.Namespace}
	createdNotebook := &nbv1.Notebook{}

	logger.V(1).Info("Checking if notebook exists for deletion")
	err := tc.customClient.Get(tc.ctx, notebookLookupKey, createdNotebook)
	if err == nil {
		logger.Info("Deleting notebook")
		nberr := tc.customClient.Delete(tc.ctx, createdNotebook, &client.DeleteOptions{})
		if nberr != nil {
			logger.V(1).Info("Error deleting notebook", "error", nberr)
			return fmt.Errorf("error deleting test Notebook %s: %v", nbMeta.Name, nberr)
		}
		logger.Info("Notebook deleted successfully")
	} else if !errors.IsNotFound(err) {
		logger.V(1).Info("Error getting notebook instance", "error", err)
		return fmt.Errorf("error getting test Notebook instance :%v", err)
	} else {
		logger.Info("Notebook not found, skipping deletion")
	}
	return nil
}

func (tc *testContext) testNotebookResourcesDeletion(nbMeta *metav1.ObjectMeta) error {
	logger := GetDeletionLogger().WithValues(
		"notebook", nbMeta.Name,
		"namespace", nbMeta.Namespace,
	)
	logger.Info("Starting notebook resources deletion verification")

	// Verify Notebook StatefulSet resource is deleted
	logger.V(1).Info("Verifying StatefulSet deletion")
	pollCount := 0
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		_, err = tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(ctx, nbMeta.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				logger.V(1).Info("StatefulSet successfully deleted",
					"polls", pollCount)
				return true, nil
			}
			logger.V(1).Info("Error getting StatefulSet (continuing to poll)",
				"poll", pollCount,
				"error", err)
			return false, err

		}
		logger.V(1).Info("StatefulSet still exists, continuing to poll",
			"poll", pollCount)
		return false, nil
	})
	if err != nil {
		logger.V(1).Info("StatefulSet deletion verification failed",
			"polls", pollCount,
			"error", err)
		return fmt.Errorf("unable to delete Statefulset %s :%v ", nbMeta.Name, err)
	}

	// Verify Notebook Network Policies are deleted
	logger.V(1).Info("Verifying Network Policies deletion")
	nbNetworkPolicyList := netv1.NetworkPolicyList{}
	opts := filterServiceMeshManagedPolicies(nbMeta)
	pollCount = 0
	err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		pollCount++
		nperr := tc.customClient.List(ctx, &nbNetworkPolicyList, opts...)
		if nperr != nil {
			if errors.IsNotFound(nperr) {
				logger.V(1).Info("Network policies successfully deleted",
					"polls", pollCount)
				return true, nil
			}
			logger.V(1).Info("Error listing network policies (continuing to poll)",
				"poll", pollCount,
				"error", nperr)
			return false, err
		}

		// Filter the policies to only include those related to this specific notebook based on pod selector
		notebookSpecificPolicies := []netv1.NetworkPolicy{}
		for _, np := range nbNetworkPolicyList.Items {
			if isNetworkPolicyForNotebook(&np, nbMeta.Name) {
				notebookSpecificPolicies = append(notebookSpecificPolicies, np)
			}
		}

		logger.V(1).Info("Checking notebook-specific network policies",
			"poll", pollCount,
			"totalPolicies", len(nbNetworkPolicyList.Items),
			"notebookSpecificPolicies", len(notebookSpecificPolicies))

		if len(notebookSpecificPolicies) == 0 {
			logger.V(1).Info("All notebook-specific network policies deleted",
				"polls", pollCount)
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		logger.V(1).Info("Network policies deletion verification failed",
			"polls", pollCount,
			"error", err)
		return fmt.Errorf("unable to delete Network policies for  %s : %v", nbMeta.Name, err)
	}

	if deploymentMode == OAuthProxy {
		// Verify Notebook Route is deleted
		logger.V(1).Info("Verifying Route deletion (OAuth mode)")
		nbRouteLookupKey := types.NamespacedName{Name: nbMeta.Name, Namespace: tc.testNamespace}
		nbRoute := &routev1.Route{}
		pollCount = 0
		err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
			pollCount++
			err = tc.customClient.Get(ctx, nbRouteLookupKey, nbRoute)
			if err != nil {
				if errors.IsNotFound(err) {
					logger.V(1).Info("Route successfully deleted",
						"polls", pollCount)
					return true, nil
				}
				logger.V(1).Info("Error getting Route (continuing to poll)",
					"poll", pollCount,
					"error", err)
				return false, err
			}
			logger.V(1).Info("Route still exists, continuing to poll",
				"poll", pollCount)
			return false, nil
		})
		if err != nil {
			logger.V(1).Info("Route deletion verification failed",
				"polls", pollCount,
				"error", err)
			return fmt.Errorf("unable to delete Route %s : %v", nbMeta.Name, err)
		}
	}

	logger.Info("Notebook resources deletion verification completed successfully")

	return nil
}

func filterServiceMeshManagedPolicies(nbMeta *metav1.ObjectMeta) []client.ListOption {
	labelSelectorReq, err := labels.NewRequirement("app.kubernetes.io/managed-by", selection.NotIn, []string{"maistra-istio-operator"})
	if err != nil {
		// This should never happen with hardcoded values, but log it properly
		logger := GetDeletionLogger()
		logger.Error(err, "Failed to create label selector requirement - this is a programming error")
		panic(fmt.Sprintf("Failed to create label selector requirement: %v", err))
	}

	notManagedByMeshLabel := labels.NewSelector()
	notManagedByMeshLabel = notManagedByMeshLabel.Add(*labelSelectorReq)

	return []client.ListOption{
		client.InNamespace(nbMeta.Namespace),
		client.MatchingLabelsSelector{Selector: notManagedByMeshLabel},
	}
}

// isNetworkPolicyForNotebook checks if a NetworkPolicy targets a specific notebook
// by examining its spec.podSelector for the notebook-name label
func isNetworkPolicyForNotebook(np *netv1.NetworkPolicy, notebookName string) bool {
	// Check if the NetworkPolicy's podSelector has the notebook-name label
	if np.Spec.PodSelector.MatchLabels != nil {
		if labelValue, exists := np.Spec.PodSelector.MatchLabels["notebook-name"]; exists {
			return labelValue == notebookName
		}
	}
	return false
}

func (tc *testContext) isNotebookCRD() error {
	apiextclient, err := apiext.NewForConfig(tc.cfg)
	if err != nil {
		return fmt.Errorf("error creating the apiextension client object %v", err)
	}
	_, err = apiextclient.CustomResourceDefinitions().Get(tc.ctx, "notebooks.kubeflow.org", metav1.GetOptions{})

	return err

}
