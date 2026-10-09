/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/opendatahub-io/odh-platform-utilities/framework/utils/ingressassignment"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const alphaIngressName = "alpha"

func TestIngressNamespacePredicate(t *testing.T) {
	old := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"other": "one"}}}
	updated := old.DeepCopy()
	updated.Annotations["other"] = "two"
	if ingressNamespacePredicate.Create(event.CreateEvent{Object: old}) ||
		ingressNamespacePredicate.Create(event.CreateEvent{Object: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{IngressNameAnnotation: alphaIngressName}}}}) ||
		ingressNamespacePredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("namespace creation or unrelated annotation change triggered reconciliation")
	}
	updated.Annotations[IngressNameAnnotation] = ""
	if !ingressNamespacePredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("adding an empty ingress annotation did not trigger reconciliation")
	}
	old = updated.DeepCopy()
	updated.Annotations[IngressNameAnnotation] = alphaIngressName
	if !ingressNamespacePredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: updated}) {
		t.Fatal("changing ingress name did not trigger reconciliation")
	}
}

var _ = Describe("Ingress assignment watch", func() {
	const namespaceName = "ingress-watch-test"
	const notebookName = "ingress-watch-book"
	const gatewayNamespace = "projected-ingress"
	const configuredIngresses = `[
		{"name":"primary","gatewayName":"projected-default","gatewayNamespace":"projected-ingress","isDefault":true},
		{"name":"alpha","gatewayName":"gateway-alpha","gatewayNamespace":"projected-ingress"},
		{"name":"beta","gatewayName":"gateway-beta","gatewayNamespace":"projected-ingress"}
	]`
	const withoutBeta = `[
		{"name":"primary","gatewayName":"projected-default","gatewayNamespace":"projected-ingress","isDefault":true},
		{"name":"alpha","gatewayName":"gateway-alpha","gatewayNamespace":"projected-ingress"}
	]`
	routeKey := types.NamespacedName{Name: "nb-" + namespaceName + "-" + notebookName, Namespace: odhNotebookControllerTestNamespace}
	configKey := types.NamespacedName{Name: NotebookControllerConfigMap, Namespace: odhNotebookControllerTestNamespace}

	AfterEach(func() {
		_ = cli.Delete(ctx, &nbv1.Notebook{ObjectMeta: metav1.ObjectMeta{Name: notebookName, Namespace: namespaceName}})
		_ = cli.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}})
		_ = cli.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configKey.Name, Namespace: configKey.Namespace}})
	})

	It("updates routes after Namespace and data-only ConfigMap changes", func() {
		By("Creating a Notebook in a namespace already assigned to an ingress")
		config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configKey.Name, Namespace: configKey.Namespace}, Data: map[string]string{IngressesKey: configuredIngresses}}
		Expect(cli.Create(ctx, config)).To(Succeed())
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName, Annotations: map[string]string{IngressNameAnnotation: alphaIngressName}}}
		Expect(cli.Create(ctx, ns)).To(Succeed())
		notebook := createNotebook(notebookName, namespaceName)
		Expect(cli.Create(ctx, notebook)).To(Succeed())

		gateway := func() (types.NamespacedName, error) {
			route := &gatewayv1.HTTPRoute{}
			if err := cli.Get(ctx, routeKey, route); err != nil {
				return types.NamespacedName{}, err
			}
			if len(route.Spec.ParentRefs) == 0 {
				return types.NamespacedName{}, nil
			}
			Expect(route.Spec.ParentRefs[0].SectionName).To(BeNil())
			parent := route.Spec.ParentRefs[0]
			Expect(parent.Namespace).NotTo(BeNil())
			return types.NamespacedName{Name: string(parent.Name), Namespace: string(*parent.Namespace)}, nil
		}
		Eventually(gateway, 30*time.Second, 250*time.Millisecond).Should(Equal(types.NamespacedName{Name: "gateway-alpha", Namespace: gatewayNamespace}))

		By("Updating only the Namespace ingress annotation")
		Expect(cli.Get(ctx, types.NamespacedName{Name: namespaceName}, ns)).To(Succeed())
		ns.Annotations[IngressNameAnnotation] = "beta"
		Expect(cli.Update(ctx, ns)).To(Succeed())
		Eventually(gateway, 30*time.Second, 250*time.Millisecond).Should(Equal(types.NamespacedName{Name: "gateway-beta", Namespace: gatewayNamespace}))

		By("Removing the selected ingress through a data-only ConfigMap update")
		Expect(cli.Get(ctx, configKey, config)).To(Succeed())
		config.Data[IngressesKey] = withoutBeta
		Expect(cli.Update(ctx, config)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(cli.Get(ctx, routeKey, &gatewayv1.HTTPRoute{}))
		}, 30*time.Second, 250*time.Millisecond).Should(BeTrue())
		By("Reporting the missing assignment as a warning event on the Notebook")
		Eventually(func() ([]corev1.Event, error) {
			events := &corev1.EventList{}
			err := cli.List(ctx, events, client.InNamespace(namespaceName), client.MatchingFields{
				"involvedObject.uid": string(notebook.UID),
				"reason":             "IngressNotFound",
			})
			return events.Items, err
		}, 30*time.Second, 250*time.Millisecond).Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeWarning),
			HaveField("InvolvedObject.Kind", "Notebook"),
			HaveField("InvolvedObject.Name", notebookName),
			HaveField("Message", ContainSubstring(`unknown ingress "beta"`)),
		)))

		By("Restoring the ingress and recreating the route")
		Expect(cli.Get(ctx, configKey, config)).To(Succeed())
		config.Data[IngressesKey] = configuredIngresses
		Expect(cli.Update(ctx, config)).To(Succeed())
		Eventually(gateway, 30*time.Second, 250*time.Millisecond).Should(Equal(types.NamespacedName{Name: "gateway-beta", Namespace: gatewayNamespace}))
		Expect(cli.Get(ctx, types.NamespacedName{Name: namespaceName}, ns)).To(Succeed())
		By("Removing the annotation and returning to the configured default")
		delete(ns.Annotations, IngressNameAnnotation)
		Expect(cli.Update(ctx, ns)).To(Succeed())
		Eventually(gateway, 30*time.Second, 250*time.Millisecond).Should(Equal(types.NamespacedName{Name: "projected-default", Namespace: gatewayNamespace}))

		By("Removing the configured default and retaining the static default Gateway")
		Expect(cli.Get(ctx, configKey, config)).To(Succeed())
		config.Data[IngressesKey] = `[{"name":"alpha","gatewayName":"gateway-alpha","gatewayNamespace":"projected-ingress"}]`
		Expect(cli.Update(ctx, config)).To(Succeed())
		Eventually(gateway, 30*time.Second, 250*time.Millisecond).Should(Equal(types.NamespacedName{
			Name: DefaultGatewayName, Namespace: DefaultGatewayNamespace,
		}))
	})
})

func TestNotebookIngressAssignment(t *testing.T) {
	t.Setenv("NOTEBOOK_GATEWAY_NAME", "custom-default")
	t.Setenv("NOTEBOOK_GATEWAY_NAMESPACE", "custom-ingress")
	const configuredIngresses = `[
		{"name":"primary","gatewayName":"operator-default","gatewayNamespace":"operator-ingress","isDefault":true},
		{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}
	]`
	const additionalOnly = `[{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}]`
	legacyDefault := defaultNotebookIngress()
	require.Equal(t, "custom-default", legacyDefault.GatewayName)
	require.Equal(t, "custom-ingress", legacyDefault.GatewayNamespace)
	operatorDefault := ingressassignment.Ingress{GatewayName: "operator-default", GatewayNamespace: "operator-ingress"}
	alpha := ingressassignment.Ingress{GatewayName: "alpha-gateway", GatewayNamespace: "additional-ingress"}

	cases := []struct {
		name       string
		data       map[string]string
		annotation *string
		want       ingressassignment.Ingress
		wantError  bool
		unknown    bool
	}{
		{name: "missing ConfigMap preserves legacy default", want: legacyDefault},
		{name: "explicit default without configuration", annotation: new("default"), want: legacyDefault},
		{name: "unrelated configuration preserves legacy default", data: map[string]string{"otherSetting": "unrelated"}, want: legacyDefault},
		{name: "absent annotation selects operator default", data: map[string]string{IngressesKey: configuredIngresses}, want: operatorDefault},
		{name: "explicit annotation selects operator default", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new("primary"), want: operatorDefault},
		{name: "additional ingress forwards Gateway name and namespace", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new(alphaIngressName), want: alpha},
		{name: "additional-only list preserves legacy default", data: map[string]string{IngressesKey: additionalOnly}, want: legacyDefault},
		{name: "additional-only list still resolves explicit assignment", data: map[string]string{IngressesKey: additionalOnly}, annotation: new(alphaIngressName), want: alpha},
		{name: "unmarked default name conflicts with legacy fallback", data: map[string]string{IngressesKey: `[{"name":"default","gatewayName":"operator-default","gatewayNamespace":"operator-ingress"}]`}, wantError: true},
		{name: "default name does not override a flagged ingress", data: map[string]string{IngressesKey: `[{"name":"primary","gatewayName":"operator-default","gatewayNamespace":"operator-ingress","isDefault":true},{"name":"default","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}]`}, want: operatorDefault},
		{name: "unmarked default name remains an explicit assignment", data: map[string]string{IngressesKey: `[{"name":"primary","gatewayName":"operator-default","gatewayNamespace":"operator-ingress","isDefault":true},{"name":"default","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}]`}, annotation: new("default"), want: alpha},
		{name: "empty list preserves legacy default", data: map[string]string{IngressesKey: `[]`}, want: legacyDefault},
		{name: "invalid JSON retains existing route", data: map[string]string{IngressesKey: "invalid JSON"}, wantError: true},
		{name: "invalid Gateway retains existing route", data: map[string]string{IngressesKey: `[{"name":"default","gatewayName":"","gatewayNamespace":"operator-ingress","isDefault":true}]`}, wantError: true},
		{name: "multiple defaults retain existing route", data: map[string]string{IngressesKey: `[{"name":"one","gatewayName":"one","gatewayNamespace":"ingress","isDefault":true},{"name":"two","gatewayName":"two","gatewayNamespace":"ingress","isDefault":true}]`}, wantError: true},
		{name: "unknown annotation removes stale route", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new("removed"), wantError: true, unknown: true},
		{name: "default fallback does not accept unknown annotation", data: map[string]string{IngressesKey: `[]`}, annotation: new("removed"), wantError: true, unknown: true},
	}
	for _, auth := range []bool{false, true} {
		name := "regular"
		if auth {
			name = "auth-proxy"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					// Arrange an existing route and an independent Namespace/configuration snapshot.
					ctx := context.Background()
					scheme := runtime.NewScheme()
					for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, gatewayv1.Install, nbv1.AddToScheme} {
						require.NoError(t, add(scheme))
					}
					ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
					if tc.annotation != nil {
						ns.Annotations = map[string]string{IngressNameAnnotation: *tc.annotation}
					}
					notebook := &nbv1.Notebook{ObjectMeta: metav1.ObjectMeta{Name: "book", Namespace: ns.Name}}
					original := NewNotebookHTTPRoute(notebook, "central", legacyDefault)
					if auth {
						original = NewNotebookKubeRbacProxyHTTPRoute(notebook, "central", legacyDefault)
					}
					cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, notebook, original).Build()
					if tc.data != nil {
						require.NoError(t, cli.Create(ctx, &corev1.ConfigMap{
							ObjectMeta: metav1.ObjectMeta{Name: NotebookControllerConfigMap, Namespace: "central"}, Data: tc.data,
						}))
					}
					recorder := record.NewFakeRecorder(1)
					r := &OpenshiftNotebookReconciler{Client: cli, Namespace: "central", Log: logr.Discard(), EventRecorder: recorder}
					reconcile := r.ReconcileHTTPRoute
					if auth {
						reconcile = r.ReconcileKubeRbacProxyHTTPRoute
					}

					// Act using the same configuration read and route path as the controller.
					config, err := r.notebookControllerConfig(ctx)
					require.NoError(t, err)
					err = reconcile(notebook, ctx, config)

					// Assert error handling and the complete route spec, including unchanged default fields.
					if tc.wantError {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
					var route gatewayv1.HTTPRoute
					getErr := cli.Get(ctx, client.ObjectKeyFromObject(original), &route)
					if tc.unknown {
						require.ErrorIs(t, err, ingressassignment.ErrUnknownIngress)
						require.Len(t, recorder.Events, 1)
						require.Equal(t, "Warning IngressNotFound "+err.Error(), <-recorder.Events)
						require.True(t, apierrors.IsNotFound(getErr), "stale route lookup: %v", getErr)
						return
					}
					require.NotErrorIs(t, err, ingressassignment.ErrUnknownIngress)
					require.Empty(t, recorder.Events, "only unknown assignments should emit IngressNotFound")
					require.NoError(t, getErr)
					want := original.DeepCopy()
					if !tc.wantError {
						want.Spec.ParentRefs[0].Name = gatewayv1.ObjectName(tc.want.GatewayName)
						want.Spec.ParentRefs[0].Namespace = new(gatewayv1.Namespace(tc.want.GatewayNamespace))
					}
					require.Equal(t, want.Spec, route.Spec)
					require.Nil(t, route.Spec.ParentRefs[0].SectionName)
				})
			}
		})
	}
}
