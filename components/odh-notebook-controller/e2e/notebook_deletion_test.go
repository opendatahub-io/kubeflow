package e2e

import (
	"context"
	"fmt"
	"log"
	"testing"

	netv1 "k8s.io/api/networking/v1"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
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
	for _, nbContext := range testCtx.testNotebooks {
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
	// Delete test Notebook resource if found
	notebookLookupKey := types.NamespacedName{Name: nbMeta.Name, Namespace: nbMeta.Namespace}
	createdNotebook := &nbv1.Notebook{}

	err := tc.customClient.Get(tc.ctx, notebookLookupKey, createdNotebook)
	if err == nil {
		nberr := tc.customClient.Delete(tc.ctx, createdNotebook, &client.DeleteOptions{})
		if nberr != nil {
			return fmt.Errorf("error deleting test Notebook %s: %v", nbMeta.Name, nberr)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("error getting test Notebook instance :%v", err)
	}
	return nil
}

func (tc *testContext) testNotebookResourcesDeletion(nbMeta *metav1.ObjectMeta) error {
	// Verify Notebook StatefulSet resource is deleted
	err := wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		_, err = tc.kubeClient.AppsV1().StatefulSets(tc.testNamespace).Get(ctx, nbMeta.Name, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			log.Printf("Failed to get %s statefulset", nbMeta.Name)
			return false, err

		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("unable to delete Statefulset %s :%v ", nbMeta.Name, err)
	}

	// Verify Notebook Network Policies are deleted
	nbNetworkPolicyList := netv1.NetworkPolicyList{}
	opts := []client.ListOption{client.InNamespace(nbMeta.Namespace)}
	err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		nperr := tc.customClient.List(ctx, &nbNetworkPolicyList, opts...)
		if nperr != nil {
			if errors.IsNotFound(nperr) {
				return true, nil
			}
			log.Printf("Failed to get Network policies for %v", nbMeta.Name)
			return false, err
		}

		// Filter the policies to only include those related to this specific notebook based on pod selector
		notebookSpecificPolicies := []netv1.NetworkPolicy{}
		for _, np := range nbNetworkPolicyList.Items {
			if isNetworkPolicyForNotebook(&np, nbMeta.Name) {
				notebookSpecificPolicies = append(notebookSpecificPolicies, np)
			}
		}

		if len(notebookSpecificPolicies) == 0 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("unable to delete Network policies for  %s : %v", nbMeta.Name, err)
	}

	// Verify Notebook HTTPRoute is deleted
	err = wait.PollUntilContextTimeout(tc.ctx, tc.resourceRetryInterval, tc.resourceCreationTimeout, false, func(ctx context.Context) (done bool, err error) {
		_, err = tc.getNotebookHTTPRoute(nbMeta)
		if err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			log.Printf("Failed to get %s HTTPRoute", nbMeta.Name)
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("unable to delete HTTPRoute %s : %v", nbMeta.Name, err)
	}

	return nil
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
