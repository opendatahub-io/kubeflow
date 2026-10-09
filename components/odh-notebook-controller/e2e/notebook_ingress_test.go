package e2e

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/opendatahub-io/odh-platform-utilities/framework/utils/ingressassignment"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ingressAssignmentTest(t *testing.T) {
	tc, err := NewTestContext()
	require.NoError(t, err)
	t.Cleanup(func() {
		if t.Failed() {
			tc.dumpControllerLogs(t)
		}
	})
	const ingressName = "e2e-ingress"

	t.Log("Phase 1: capture existing routes and namespace configuration")
	originalParents := map[string][]gatewayv1.ParentReference{}
	assignedParents := map[string][]gatewayv1.ParentReference{}
	for _, notebook := range tc.testNotebooks {
		route, err := tc.getNotebookHTTPRoute(notebook.nbObjectMeta)
		require.NoError(t, err)
		require.NotEmpty(t, route.Spec.ParentRefs)
		require.Equal(t, tc.testNamespace, route.Namespace)
		require.Equal(t, notebook.nbObjectMeta.Name, route.Labels["notebook-name"])
		require.Equal(t, notebook.nbObjectMeta.Namespace, route.Labels["notebook-namespace"])
		originalParents[notebook.nbObjectMeta.Name] = route.DeepCopy().Spec.ParentRefs
		assigned := route.DeepCopy()
		assigned.Spec.ParentRefs[0].SectionName = nil
		assignedParents[notebook.nbObjectMeta.Name] = assigned.Spec.ParentRefs
	}
	ns, err := tc.kubeClient.CoreV1().Namespaces().Get(tc.ctx, tc.testNamespace, metav1.GetOptions{})
	require.NoError(t, err)
	previousName, hadAnnotation := ns.Annotations[namespaceIngressAnnotation]
	config, err := tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Get(tc.ctx, notebookControllerConfigMapName, metav1.GetOptions{})
	absentConfig := apierrors.IsNotFound(err)
	require.True(t, err == nil || absentConfig, "get controller ConfigMap: %v", err)
	previousIngresses, hadIngresses := "", false
	if !absentConfig {
		previousIngresses, hadIngresses = config.Data[controllerIngressesKey]
	}
	t.Log("Phase 2: prepare the test ingress and expected route parents")
	var ingresses []ingressassignment.Ingress
	if hadIngresses {
		require.NoError(t, json.Unmarshal([]byte(previousIngresses), &ingresses))
	} else {
		parent := originalParents[tc.testNotebooks[0].nbObjectMeta.Name][0]
		require.NotNil(t, parent.Namespace)
		ingresses = []ingressassignment.Ingress{{
			Name: "default", GatewayName: string(parent.Name), GatewayNamespace: string(*parent.Namespace), IsDefault: true,
		}}
	}
	index := slices.IndexFunc(ingresses, func(ingress ingressassignment.Ingress) bool { return ingress.Name == ingressName })
	if index == -1 {
		ingresses = append(ingresses, ingressassignment.Ingress{
			Name: ingressName, GatewayName: ingressName, GatewayNamespace: "e2e-ingress-namespace",
		})
		index = len(ingresses) - 1
	}
	for _, notebook := range tc.testNotebooks {
		parent := &assignedParents[notebook.nbObjectMeta.Name][0]
		parent.Name = gatewayv1.ObjectName(ingresses[index].GatewayName)
		parent.Namespace = new(gatewayv1.Namespace(ingresses[index].GatewayNamespace))
	}
	encoded, err := json.Marshal(ingresses)
	require.NoError(t, err)
	configChanged, namespaceChanged := false, false
	t.Cleanup(func() {
		t.Log("Phase 5: restore configuration and verify the original route parents")
		var namespaceErr, configErr error
		if namespaceChanged {
			namespaceErr = tc.setNamespaceIngressAnnotation(previousName, hadAnnotation)
		}
		if configChanged {
			if absentConfig {
				configErr = tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Delete(tc.ctx, notebookControllerConfigMapName, metav1.DeleteOptions{})
				if apierrors.IsNotFound(configErr) {
					configErr = nil
				}
			} else {
				configErr = tc.setControllerIngresses(previousIngresses, hadIngresses)
			}
		}
		if namespaceErr != nil || configErr != nil {
			t.Errorf("restore ingress test state: namespace: %v, ConfigMap: %v", namespaceErr, configErr)
			return
		}
		require.NoError(t, tc.waitForNotebookHTTPRouteParents(originalParents), "HTTPRoutes did not return to their original Gateways")
	})
	t.Log("Phase 3: publish the ingress configuration")
	if absentConfig {
		_, err = tc.kubeClient.CoreV1().ConfigMaps(tc.testNamespace).Create(tc.ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: notebookControllerConfigMapName, Namespace: tc.testNamespace},
			Data:       map[string]string{controllerIngressesKey: string(encoded)},
		}, metav1.CreateOptions{})
	} else {
		err = tc.setControllerIngresses(string(encoded), true)
	}
	require.NoError(t, err)
	configChanged = true

	t.Log("Phase 4: assign the namespace and verify every Notebook route")
	require.NoError(t, tc.setNamespaceIngressAnnotation(ingressName, true))
	namespaceChanged = true
	require.NoError(t, tc.waitForNotebookHTTPRouteParents(assignedParents), "HTTPRoutes did not move to the assigned Gateway")
}
