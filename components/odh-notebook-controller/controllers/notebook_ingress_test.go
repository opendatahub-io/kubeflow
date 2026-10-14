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
	"errors"
	"strings"
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
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	alphaIngressName           = "alpha"
	betaIngressName            = "beta"
	alphaNotebookOne           = "alpha/one"
	alphaNotebookTwo           = "alpha/two"
	defaultNotebookOne         = "default/one"
	explicitDefaultNotebookOne = "explicit-default/one"
	invalidIngressJSON         = "invalid JSON"
	createEventKind            = "create"
	deleteEventKind            = "delete"
	genericEventKind           = "generic"
	centralTestNamespace       = "central"
	unrelatedConfigKey         = "unrelated"
	userNotebookOne            = "user/one"
	caTestNamespace            = "user"
)

func TestIngressConfigHandler(t *testing.T) {
	const configured = `[
		{"name":"primary","gatewayName":"default-gateway","gatewayNamespace":"ingress","isDefault":true},
		{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"ingress"},
		{"name":"beta","gatewayName":"beta-gateway","gatewayNamespace":"ingress"}
	]`
	const reordered = `[
		{"name":"beta","gatewayName":"beta-gateway","gatewayNamespace":"ingress"},
		{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"ingress"},
		{"name":"primary","gatewayName":"default-gateway","gatewayNamespace":"ingress","isDefault":true}
	]`
	withoutAlpha := strings.ReplaceAll(configured, `{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"ingress"},`, "")
	all := []string{alphaNotebookOne, alphaNotebookTwo, "beta/one", defaultNotebookOne, explicitDefaultNotebookOne}
	cases := []struct {
		name, before, after, kind string
		missingNamespace          string
		nilUpdatedData            bool
		want                      []string
	}{
		{name: "named Gateway change", before: configured, after: strings.ReplaceAll(configured, "alpha-gateway", "new-alpha"), want: []string{alphaNotebookOne, alphaNotebookTwo}},
		{name: "Gateway namespace change", before: configured, after: strings.ReplaceAll(configured, `"alpha-gateway","gatewayNamespace":"ingress"`, `"alpha-gateway","gatewayNamespace":"other"`), want: []string{alphaNotebookOne, alphaNotebookTwo}},
		{name: "default Gateway change", before: configured, after: strings.ReplaceAll(configured, "default-gateway", "new-default"), want: []string{defaultNotebookOne, explicitDefaultNotebookOne}},
		{name: "default designation change", before: configured, after: strings.ReplaceAll(strings.ReplaceAll(configured, `,"isDefault":true`, ""), `"beta-gateway","gatewayNamespace":"ingress"`, `"beta-gateway","gatewayNamespace":"ingress","isDefault":true`), want: []string{defaultNotebookOne}},
		{name: "configured default removed uses static fallback", before: configured, after: strings.ReplaceAll(configured, `,"isDefault":true`, ""), want: []string{defaultNotebookOne}},
		{name: "unchanged ingress configuration", before: configured, after: configured},
		{name: "reordered ingresses", before: configured, after: reordered},
		{name: "unused hostname change", before: configured, after: strings.ReplaceAll(configured, `"name":"alpha"`, `"name":"alpha","hostname":"new.example.com"`)},
		{name: "unused ingress added", before: configured, after: strings.ReplaceAll(configured, "\n\t]", `,{"name":"unused","gatewayName":"unused","gatewayNamespace":"ingress"}]`)},
		{name: "ingress removed", before: configured, after: withoutAlpha, want: []string{alphaNotebookOne, alphaNotebookTwo}},
		{name: "missing assignment restored", before: withoutAlpha, after: configured, want: []string{alphaNotebookOne, alphaNotebookTwo}},
		{name: "missing assignment unchanged while beta changes", before: withoutAlpha, after: strings.ReplaceAll(withoutAlpha, "beta-gateway", "new-beta"), want: []string{"beta/one"}},
		{name: "missing assignment to invalid configuration", before: withoutAlpha, after: strings.ReplaceAll(configured, "beta-gateway", ""), want: all},
		{name: "invalid configuration to missing assignment", before: strings.ReplaceAll(configured, "beta-gateway", ""), after: withoutAlpha, want: all},
		{name: "Namespace lookup failure", before: configured, after: withoutAlpha, missingNamespace: alphaIngressName, want: all},
		{name: "malformed configuration", before: configured, after: invalidIngressJSON, want: all},
		{name: "invalid configuration", before: configured, after: strings.ReplaceAll(configured, "alpha-gateway", ""), want: all},
		{name: "configuration repaired", before: invalidIngressJSON, after: configured, want: all},
		{name: "absent ingress key uses static default", after: `[{"name":"primary","gatewayName":"new-default","gatewayNamespace":"ingress","isDefault":true},{"name":"alpha","gatewayName":"alpha-gateway","gatewayNamespace":"ingress"},{"name":"beta","gatewayName":"beta-gateway","gatewayNamespace":"ingress"}]`, want: all},
		{name: "configured to nil data", before: configured, nilUpdatedData: true, want: all},
		{name: "nil data unchanged", nilUpdatedData: true},
		{name: createEventKind, after: configured, kind: createEventKind, want: all},
		{name: "create with nil data", kind: createEventKind, nilUpdatedData: true, want: all},
		{name: deleteEventKind, before: configured, kind: deleteEventKind, want: all},
		{name: "delete with nil data", kind: deleteEventKind, want: all},
		{name: genericEventKind, after: configured, kind: genericEventKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, nbv1.AddToScheme(scheme))
			objects := []client.Object{}
			for namespace, annotations := range map[string]map[string]string{
				alphaIngressName: {IngressNameAnnotation: alphaIngressName}, betaIngressName: {IngressNameAnnotation: betaIngressName},
				defaultIngressName: nil, "explicit-default": {IngressNameAnnotation: "primary"},
			} {
				if namespace != tc.missingNamespace {
					objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Annotations: annotations}})
				}
			}
			for _, key := range all {
				namespace, name, _ := strings.Cut(key, "/")
				objects = append(objects, &nbv1.Notebook{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}})
			}
			r := &OpenshiftNotebookReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Log: logr.Discard(), Namespace: centralTestNamespace}
			config := func(ingresses string) *corev1.ConfigMap {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: NotebookControllerConfigMap, Namespace: centralTestNamespace}}
				if ingresses != "" {
					cm.Data = map[string]string{IngressesKey: ingresses}
				}
				return cm
			}
			old, updated := config(tc.before), config(tc.after)
			// Metadata and unrelated data never change which Notebook routes need updating.
			updated.Annotations = map[string]string{unrelatedConfigKey: "changed"}
			if updated.Data == nil {
				updated.Data = map[string]string{}
			}
			updated.Data[unrelatedConfigKey] = "changed"
			if tc.nilUpdatedData {
				updated.Data = nil
			}
			q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer q.ShutDown()
			h := r.configMapHandler()
			switch tc.kind {
			case createEventKind:
				h.Create(context.Background(), event.CreateEvent{Object: updated}, q)
			case deleteEventKind:
				h.Delete(context.Background(), event.DeleteEvent{Object: old}, q)
			case genericEventKind:
				h.Generic(context.Background(), event.GenericEvent{Object: updated}, q)
			default:
				h.Update(context.Background(), event.UpdateEvent{ObjectOld: old, ObjectNew: updated}, q)
			}
			var got []string
			for q.Len() > 0 {
				request, _ := q.Get()
				got = append(got, request.Namespace+"/"+request.Name)
				q.Done(request)
			}
			require.ElementsMatch(t, tc.want, got)
		})
	}
}

func TestConfigMapHandlerCABundles(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nbv1.AddToScheme(scheme))
	objects := make([]client.Object, 0, 4)
	for _, key := range []string{userNotebookOne, "user/two", "user/unmounted", "other/one"} {
		namespace, name, _ := strings.Cut(key, "/")
		notebook := &nbv1.Notebook{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if name != "unmounted" {
			notebook.Spec.Template.Spec.Volumes = []corev1.Volume{{
				Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: WorkbenchTrustedCABundleName},
				}},
			}}
		}
		objects = append(objects, notebook)
	}
	r := &OpenshiftNotebookReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Log:    logr.Discard(), Namespace: centralTestNamespace,
	}
	cases := []struct {
		name, namespace string
		want            []string
	}{
		{name: OdhConfigMapName, namespace: caTestNamespace, want: []string{userNotebookOne}},
		{name: SelfSignedConfigMapName, namespace: caTestNamespace, want: []string{userNotebookOne}},
		{name: ServiceCAConfigMapName, namespace: caTestNamespace, want: []string{userNotebookOne}},
		{name: WorkbenchTrustedCABundleName, namespace: caTestNamespace, want: []string{userNotebookOne, "user/two"}},
		{name: OdhConfigMapName, namespace: "empty"},
		{name: WorkbenchTrustedCABundleName, namespace: "empty"},
		{name: unrelatedConfigKey, namespace: caTestNamespace},
		{name: NotebookControllerConfigMap, namespace: caTestNamespace},
	}
	for _, tc := range cases {
		for _, kind := range []string{createEventKind, "update", deleteEventKind, genericEventKind} {
			t.Run(tc.namespace+"/"+tc.name+"/"+kind, func(t *testing.T) {
				old := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: tc.namespace}}
				updated := old.DeepCopy()
				updated.Data = map[string]string{IngressesKey: invalidIngressJSON}
				q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
				defer q.ShutDown()
				h := r.configMapHandler()
				switch kind {
				case createEventKind:
					h.Create(context.Background(), event.CreateEvent{Object: updated}, q)
				case "update":
					h.Update(context.Background(), event.UpdateEvent{ObjectOld: old, ObjectNew: updated}, q)
				case deleteEventKind:
					h.Delete(context.Background(), event.DeleteEvent{Object: old}, q)
				case genericEventKind:
					h.Generic(context.Background(), event.GenericEvent{Object: updated}, q)
				}
				var got []string
				for q.Len() > 0 {
					request, _ := q.Get()
					got = append(got, request.Namespace+"/"+request.Name)
					q.Done(request)
				}
				if kind == genericEventKind {
					require.Empty(t, got)
				} else {
					require.ElementsMatch(t, tc.want, got)
				}
			})
		}
	}
}

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
		ns.Annotations[IngressNameAnnotation] = betaIngressName
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
		name             string
		data             map[string]string
		annotation       *string
		want             ingressassignment.Ingress
		wantError        bool
		unknown          bool
		configMapPresent bool
		cleanupFailure   string
	}{
		{name: "missing ConfigMap preserves legacy default", want: legacyDefault},
		{name: "explicit default without configuration", annotation: new(defaultIngressName), want: legacyDefault},
		{name: "nil ConfigMap data preserves legacy default", configMapPresent: true, want: legacyDefault},
		{name: "explicit default with nil data", configMapPresent: true, annotation: new(defaultIngressName), want: legacyDefault},
		{name: "empty ConfigMap data preserves legacy default", data: map[string]string{}, want: legacyDefault},
		{name: "explicit default with empty data", data: map[string]string{}, annotation: new(defaultIngressName), want: legacyDefault},
		{name: "unrelated configuration preserves legacy default", data: map[string]string{"otherSetting": unrelatedConfigKey}, want: legacyDefault},
		{name: "absent annotation selects operator default", data: map[string]string{IngressesKey: configuredIngresses}, want: operatorDefault},
		{name: "explicit annotation selects operator default", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new("primary"), want: operatorDefault},
		{name: "additional ingress forwards Gateway name and namespace", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new(alphaIngressName), want: alpha},
		{name: "additional-only list preserves legacy default", data: map[string]string{IngressesKey: additionalOnly}, want: legacyDefault},
		{name: "additional-only list still resolves explicit assignment", data: map[string]string{IngressesKey: additionalOnly}, annotation: new(alphaIngressName), want: alpha},
		{name: "unmarked default name conflicts with legacy fallback", data: map[string]string{IngressesKey: `[{"name":"default","gatewayName":"operator-default","gatewayNamespace":"operator-ingress"}]`}, wantError: true},
		{name: "default name does not override a flagged ingress", data: map[string]string{IngressesKey: `[{"name":"primary","gatewayName":"operator-default","gatewayNamespace":"operator-ingress","isDefault":true},{"name":"default","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}]`}, want: operatorDefault},
		{name: "unmarked default name remains an explicit assignment", data: map[string]string{IngressesKey: `[{"name":"primary","gatewayName":"operator-default","gatewayNamespace":"operator-ingress","isDefault":true},{"name":"default","gatewayName":"alpha-gateway","gatewayNamespace":"additional-ingress"}]`}, annotation: new(defaultIngressName), want: alpha},
		{name: "empty list preserves legacy default", data: map[string]string{IngressesKey: `[]`}, want: legacyDefault},
		{name: "invalid JSON retains existing route", data: map[string]string{IngressesKey: invalidIngressJSON}, wantError: true},
		{name: "invalid Gateway retains existing route", data: map[string]string{IngressesKey: `[{"name":"default","gatewayName":"","gatewayNamespace":"operator-ingress","isDefault":true}]`}, wantError: true},
		{name: "multiple defaults retain existing route", data: map[string]string{IngressesKey: `[{"name":"one","gatewayName":"one","gatewayNamespace":"ingress","isDefault":true},{"name":"two","gatewayName":"two","gatewayNamespace":"ingress","isDefault":true}]`}, wantError: true},
		{name: "unknown annotation removes stale route", data: map[string]string{IngressesKey: configuredIngresses}, annotation: new("removed"), unknown: true},
		{name: "default fallback does not accept unknown annotation", data: map[string]string{IngressesKey: `[]`}, annotation: new("removed"), unknown: true},
		{name: "missing ConfigMap with unknown assignment removes stale route", annotation: new("removed"), unknown: true},
		{name: "nil ConfigMap data with unknown assignment removes stale route", configMapPresent: true, annotation: new("removed"), unknown: true},
		{name: "empty ConfigMap data with unknown assignment removes stale route", data: map[string]string{}, annotation: new("removed"), unknown: true},
		{name: "cleanup List failure returns error", annotation: new("removed"), unknown: true, wantError: true, cleanupFailure: "list"},
		{name: "cleanup Delete failure returns error", annotation: new("removed"), unknown: true, wantError: true, cleanupFailure: deleteEventKind},
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
					original := NewNotebookHTTPRoute(notebook, centralTestNamespace, legacyDefault)
					if auth {
						original = NewNotebookKubeRbacProxyHTTPRoute(notebook, centralTestNamespace, legacyDefault)
					}
					cleanupErr := errors.New("cleanup " + tc.cleanupFailure + " failed")
					cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, notebook, original).
						WithInterceptorFuncs(interceptor.Funcs{
							List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
								if _, ok := list.(*gatewayv1.HTTPRouteList); ok && tc.cleanupFailure == "list" {
									return cleanupErr
								}
								return c.List(ctx, list, opts...)
							},
							Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
								if _, ok := obj.(*gatewayv1.HTTPRoute); ok && tc.cleanupFailure == deleteEventKind {
									return cleanupErr
								}
								return c.Delete(ctx, obj, opts...)
							},
						}).Build()
					if tc.data != nil || tc.configMapPresent {
						require.NoError(t, cli.Create(ctx, &corev1.ConfigMap{
							ObjectMeta: metav1.ObjectMeta{Name: NotebookControllerConfigMap, Namespace: centralTestNamespace}, Data: tc.data,
						}))
					}
					recorder := record.NewFakeRecorder(1)
					r := &OpenshiftNotebookReconciler{Client: cli, Namespace: centralTestNamespace, Log: logr.Discard(), EventRecorder: recorder}
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
						_, assignmentErr := r.notebookIngress(ctx, ns.Name, config)
						require.ErrorIs(t, assignmentErr, ingressassignment.ErrUnknownIngress)
						require.Len(t, recorder.Events, 1)
						require.Equal(t, "Warning IngressNotFound "+assignmentErr.Error(), <-recorder.Events)
						if tc.cleanupFailure != "" {
							require.ErrorContains(t, err, cleanupErr.Error())
							require.NoError(t, getErr)
							require.Equal(t, original.Spec, route.Spec)
							return
						}
						require.True(t, apierrors.IsNotFound(getErr), "stale route lookup: %v", getErr)
						require.NoError(t, reconcile(notebook, ctx, config), "repeated cleanup should succeed")
						require.True(t, apierrors.IsNotFound(cli.Get(ctx, client.ObjectKeyFromObject(original), &route)))
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
