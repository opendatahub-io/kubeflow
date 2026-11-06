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

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const statefulSetLabelKey = "statefulset"

func networkPolicyTestScheme() *runtime.Scheme {
	testScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(nbv1.AddToScheme(testScheme))
	utilruntime.Must(netv1.AddToScheme(testScheme))
	return testScheme
}

func fakeNotebookReconciler(c client.Client, recorder record.EventRecorder) *OpenshiftNotebookReconciler {
	testScheme := networkPolicyTestScheme()
	if c != nil {
		testScheme = c.Scheme()
	}
	return &OpenshiftNotebookReconciler{
		Client:        c,
		Scheme:        testScheme,
		Log:           logr.Discard(),
		Namespace:     odhNotebookControllerTestNamespace,
		EventRecorder: recorder,
	}
}

func expectNoProhibitedNetworkPolicyForm(np *netv1.NetworkPolicy) {
	ExpectWithOffset(1, np.Spec.PodSelector.MatchLabels).NotTo(BeEmpty())

	for _, rule := range np.Spec.Ingress {
		// "from: []" is prohibited. A nil From is allowed for the documented
		// compatibility rule.
		if rule.From != nil {
			ExpectWithOffset(1, rule.From).NotTo(BeEmpty(), "ingress rule must not use from: []")
		}
		for _, peer := range rule.From {
			ExpectWithOffset(1, peer).NotTo(Equal(netv1.NetworkPolicyPeer{}), "empty peer {} is prohibited")
			if peer.NamespaceSelector != nil {
				ExpectWithOffset(1, len(peer.NamespaceSelector.MatchLabels)+len(peer.NamespaceSelector.MatchExpressions)).NotTo(BeZero(), "namespaceSelector: {} is prohibited")
			}
			if peer.IPBlock != nil {
				ExpectWithOffset(1, peer.IPBlock.CIDR).NotTo(BeElementOf("0.0.0.0/0", "::/0"), "universal CIDR is prohibited")
				for _, except := range peer.IPBlock.Except {
					ExpectWithOffset(1, except).NotTo(BeElementOf("0.0.0.0/0", "::/0"), "universal CIDR is prohibited")
				}
			}
		}
	}
}

var _ = Describe("Notebook NetworkPolicy contract", func() {
	notebook := createNotebook("np-contract-notebook", "default")

	It("builders produce the required policies", func() {
		ctrlPolicy := NewNotebookNetworkPolicy(notebook, odhNotebookControllerTestNamespace)
		Expect(ctrlPolicy.Spec.PodSelector).To(Equal(metav1.LabelSelector{
			MatchLabels: map[string]string{notebookNameLabelKey: notebook.Name},
		}))
		Expect(ctrlPolicy.Spec.PolicyTypes).To(Equal([]netv1.PolicyType{netv1.PolicyTypeIngress}))
		Expect(ctrlPolicy.Spec.Ingress).To(HaveLen(1))
		Expect(ctrlPolicy.Spec.Ingress[0].Ports).To(HaveLen(1))
		Expect(ctrlPolicy.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(int32(NotebookPort)))
		Expect(ctrlPolicy.Spec.Ingress[0].From).To(HaveLen(1))
		Expect(ctrlPolicy.Spec.Ingress[0].From[0].NamespaceSelector).NotTo(BeNil())
		Expect(ctrlPolicy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue(
			namespaceNameLabelKey, odhNotebookControllerTestNamespace))
		expectNoProhibitedNetworkPolicyForm(ctrlPolicy)

		rbacPolicy := NewKubeRbacProxyNetworkPolicy(notebook)
		Expect(rbacPolicy.Spec.PodSelector).To(Equal(metav1.LabelSelector{
			MatchLabels: map[string]string{notebookNameLabelKey: notebook.Name},
		}))
		Expect(rbacPolicy.Spec.PolicyTypes).To(Equal([]netv1.PolicyType{netv1.PolicyTypeIngress}))
		Expect(rbacPolicy.Spec.Ingress).To(HaveLen(1))
		Expect(rbacPolicy.Spec.Ingress[0].Ports).To(HaveLen(1))
		Expect(rbacPolicy.Spec.Ingress[0].Ports[0].Port.IntVal).To(Equal(int32(NotebookKubeRbacProxyPort)))
		// The documented compatibility rule has no "from" peer.
		Expect(rbacPolicy.Spec.Ingress[0].From).To(BeEmpty())
		expectNoProhibitedNetworkPolicyForm(rbacPolicy)

		egressPolicy := NewEgressNetworkPolicy(notebook)
		Expect(egressPolicy.Spec.PodSelector).To(Equal(metav1.LabelSelector{
			MatchLabels: map[string]string{notebookNameLabelKey: notebook.Name},
		}))
		Expect(egressPolicy.Spec.PolicyTypes).To(Equal([]netv1.PolicyType{netv1.PolicyTypeEgress}))
		Expect(egressPolicy.Spec.Ingress).To(BeEmpty())
		Expect(egressPolicy.Spec.Egress).To(Equal([]netv1.NetworkPolicyEgressRule{{}}))
		expectNoProhibitedNetworkPolicyForm(egressPolicy)
	})

	It("selects the labels the upstream controller puts on generated StatefulSet pods", func() {
		// Keep in sync with generateStatefulSet in components/notebook-controller.
		// The controller copies the Notebook labels onto the pod template and adds
		// these three labels.
		podLabelSet := labels.Set{
			statefulSetLabelKey:          notebook.Name,
			notebookNameLabelKey:         notebook.Name,
			"opendatahub.io/workbenches": trueString,
		}

		for _, np := range []*netv1.NetworkPolicy{
			NewNotebookNetworkPolicy(notebook, odhNotebookControllerTestNamespace),
			NewKubeRbacProxyNetworkPolicy(notebook),
			NewEgressNetworkPolicy(notebook),
		} {
			selector, err := metav1.LabelSelectorAsSelector(&np.Spec.PodSelector)
			Expect(err).NotTo(HaveOccurred())
			Expect(selector.Matches(podLabelSet)).To(BeTrue(),
				"policy %s selector %v does not match generated pod labels %v",
				np.Name, np.Spec.PodSelector, podLabelSet)
		}
	})

	It("adds the egress policy to a pre-existing Notebook and adopts matching legacy policies", func() {
		testScheme := networkPolicyTestScheme()
		preExisting := createNotebook("np-preexisting-notebook", "default")
		legacyCtrl := NewNotebookNetworkPolicy(preExisting, odhNotebookControllerTestNamespace)
		legacyRBAC := NewKubeRbacProxyNetworkPolicy(preExisting)
		legacyCtrl.UID = types.UID("legacy-ctrl")
		legacyRBAC.UID = types.UID("legacy-rbac")

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(preExisting, legacyCtrl, legacyRBAC).
			Build()
		r := fakeNotebookReconciler(fakeClient, record.NewFakeRecorder(10))

		Expect(r.ReconcileAllNetworkPolicies(preExisting, context.Background())).To(Succeed())

		egress := &netv1.NetworkPolicy{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{
			Name: preExisting.Name + NotebookEgressNetworkPolicySuffix, Namespace: preExisting.Namespace,
		}, egress)).To(Succeed())
		Expect(egress.Spec.PolicyTypes).To(Equal([]netv1.PolicyType{netv1.PolicyTypeEgress}))
		Expect(egress.Spec.Egress).To(Equal([]netv1.NetworkPolicyEgressRule{{}}))

		for _, legacy := range []*netv1.NetworkPolicy{legacyCtrl, legacyRBAC} {
			unchanged := &netv1.NetworkPolicy{}
			Expect(fakeClient.Get(context.Background(), types.NamespacedName{
				Name: legacy.Name, Namespace: legacy.Namespace,
			}, unchanged)).To(Succeed())
			Expect(unchanged.UID).To(Equal(legacy.UID))
			Expect(unchanged.Spec).To(Equal(legacy.Spec))
			Expect(metav1.IsControlledBy(unchanged, preExisting)).To(BeTrue(),
				"matching legacy policy %s should be adopted by the Notebook", legacy.Name)
		}
	})

	It("refuses to accept a matching policy controlled by another resource", func() {
		testScheme := networkPolicyTestScheme()
		nb := createNotebook("np-foreign-owner", "default")
		nb.UID = types.UID("np-foreign-owner-uid")
		other := createNotebook("np-other-owner", "default")
		other.UID = types.UID("np-other-owner-uid")

		foreign := NewEgressNetworkPolicy(nb)
		foreign.UID = types.UID("foreign-matching-policy-uid")
		foreign.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(other, nbv1.GroupVersion.WithKind("Notebook")),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(nb, other, foreign).
			Build()
		recorder := record.NewFakeRecorder(10)
		r := fakeNotebookReconciler(fakeClient, recorder)

		err := r.ReconcileAllNetworkPolicies(nb, context.Background())
		Expect(err).To(HaveOccurred())
		Eventually(recorder.Events).Should(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring("NetworkPolicyOwnershipConflict"),
		)))

		unchanged := &netv1.NetworkPolicy{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{
			Name: foreign.Name, Namespace: nb.Namespace,
		}, unchanged)).To(Succeed())
		Expect(metav1.IsControlledBy(unchanged, other)).To(BeTrue())
		Expect(metav1.IsControlledBy(unchanged, nb)).To(BeFalse())
	})

	It("continues reconciling the remaining policies when one conflict occurs", func() {
		testScheme := networkPolicyTestScheme()
		nb := createNotebook("np-partial-conflict", "default")
		nb.UID = types.UID("np-partial-conflict-uid")
		other := createNotebook("np-partial-conflict-other", "default")
		other.UID = types.UID("np-partial-conflict-other-uid")
		foreignCtrl := NewNotebookNetworkPolicy(nb, odhNotebookControllerTestNamespace)
		foreignCtrl.UID = types.UID("np-partial-conflict-policy-uid")
		foreignCtrl.Spec.Ingress = nil // drift would otherwise be repaired
		foreignCtrl.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(other, nbv1.GroupVersion.WithKind("Notebook")),
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(nb, other, foreignCtrl).
			Build()
		r := fakeNotebookReconciler(fakeClient, record.NewFakeRecorder(10))

		Expect(r.ReconcileAllNetworkPolicies(nb, context.Background())).NotTo(Succeed())

		unchanged := &netv1.NetworkPolicy{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{
			Name: foreignCtrl.Name, Namespace: nb.Namespace,
		}, unchanged)).To(Succeed())
		Expect(unchanged.Spec.Ingress).To(BeNil())

		for _, suffix := range []string{
			NotebookKubeRbacProxyNetworkPolicySuffix,
			NotebookEgressNetworkPolicySuffix,
		} {
			np := &netv1.NetworkPolicy{}
			Expect(fakeClient.Get(context.Background(), types.NamespacedName{
				Name: nb.Name + suffix, Namespace: nb.Namespace,
			}, np)).To(Succeed(), "expected policy %s to be reconciled despite the earlier conflict", np.Name)
		}
	})

	It("reconciles an existing policy when Create races with AlreadyExists", func() {
		testScheme := networkPolicyTestScheme()
		nb := createNotebook("np-create-race", "default")
		nb.UID = types.UID("np-create-race-uid")
		existing := NewNotebookNetworkPolicy(nb, odhNotebookControllerTestNamespace)
		existing.UID = types.UID("np-create-race-policy-uid")
		existing.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(nb, nbv1.GroupVersion.WithKind("Notebook"))}
		existing.Spec.Ingress = nil // force drift so the fallback path must repair it

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(nb, existing).
			Build()
		r := fakeNotebookReconciler(fakeClient, record.NewFakeRecorder(10))
		// Simulate Get returning NotFound and Create returning AlreadyExists.
		r.Client = &alreadyExistsClient{Client: fakeClient, name: existing.Name}

		Expect(r.ReconcileAllNetworkPolicies(nb, context.Background())).To(Succeed())

		reconciled := &netv1.NetworkPolicy{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{
			Name: existing.Name, Namespace: nb.Namespace,
		}, reconciled)).To(Succeed())
		Expect(reconciled.Spec.Ingress).To(HaveLen(1))
	})

	It("adopts and repairs an unowned same-named policy with drift", func() {
		testScheme := networkPolicyTestScheme()
		nb := createNotebook("np-unowned-policy", "default")
		nb.UID = types.UID("np-unowned-policy-uid")
		unowned := NewEgressNetworkPolicy(nb)
		unowned.UID = types.UID("np-unowned-policy-egress-uid")
		unowned.Spec.Egress = nil // force drift so adoption must repair it

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(nb, unowned).
			Build()
		r := fakeNotebookReconciler(fakeClient, record.NewFakeRecorder(10))

		Expect(r.ReconcileAllNetworkPolicies(nb, context.Background())).To(Succeed())

		reconciled := &netv1.NetworkPolicy{}
		Expect(fakeClient.Get(context.Background(), types.NamespacedName{
			Name: unowned.Name, Namespace: nb.Namespace,
		}, reconciled)).To(Succeed())
		Expect(reconciled.UID).To(Equal(unowned.UID))
		Expect(reconciled.Spec.Egress).To(Equal([]netv1.NetworkPolicyEgressRule{{}}))
		Expect(metav1.IsControlledBy(reconciled, nb)).To(BeTrue())
	})

	It("keeps reconciling its own policies when a namespace default-deny exists", func() {
		testScheme := networkPolicyTestScheme()
		denyAll := &netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "default-deny-all", Namespace: notebook.Namespace},
			Spec: netv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []netv1.PolicyType{netv1.PolicyTypeIngress, netv1.PolicyTypeEgress},
			},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(notebook, denyAll).
			Build()
		r := fakeNotebookReconciler(fakeClient, record.NewFakeRecorder(10))

		Expect(r.ReconcileAllNetworkPolicies(notebook, context.Background())).To(Succeed())
		for _, name := range []string{
			notebook.Name + "-ctrl-np",
			notebook.Name + NotebookKubeRbacProxyNetworkPolicySuffix,
			notebook.Name + NotebookEgressNetworkPolicySuffix,
		} {
			np := &netv1.NetworkPolicy{}
			Expect(fakeClient.Get(context.Background(), types.NamespacedName{
				Name: name, Namespace: notebook.Namespace,
			}, np)).To(Succeed(), "expected policy %s to be reconciled", name)
		}
	})

	It("keeps the existing policy names stable", func() {
		// These names pre-date this change; renaming them would orphan existing
		// policies instead of reconciling them throughout the lifecycle.
		Expect(NotebookKubeRbacProxyNetworkPolicySuffix).To(Equal("-kube-rbac-proxy-np"))
		Expect(NotebookEgressNetworkPolicySuffix).To(Equal("-egress-np"))
		Expect(NotebookPort).To(Equal(8888))
		Expect(NotebookKubeRbacProxyPort).To(Equal(8443))
	})
})

var _ = Describe("NetworkPolicy event reporting", func() {
	notebook := createNotebook("np-event-notebook", "default")

	It("reports a warning event when a required policy cannot be created", func() {
		testScheme := networkPolicyTestScheme()
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(notebook).Build()
		recorder := record.NewFakeRecorder(10)
		r := fakeNotebookReconciler(fakeClient, recorder)
		r.Client = &failingCreateClient{Client: fakeClient}

		err := r.ReconcileAllNetworkPolicies(notebook, context.Background())
		Expect(err).To(HaveOccurred())
		Eventually(recorder.Events).Should(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring("NetworkPolicyReconcileFailed"),
			ContainSubstring("Failed to create"),
		)))
	})

	It("emits an event for repair failures too", func() {
		testScheme := networkPolicyTestScheme()
		nb := createNotebook("np-repair-failure", "default")
		existing := NewNotebookNetworkPolicy(nb, "central")
		existing.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(nb, nbv1.GroupVersion.WithKind("Notebook"))}
		existing.Spec.Ingress = nil // force drift
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(nb, existing).Build()
		recorder := record.NewFakeRecorder(10)
		r := fakeNotebookReconciler(fakeClient, recorder)
		// Force the repair Update to fail without racing the API server.
		r.Client = &failingUpdateClient{Client: fakeClient}
		err := r.ReconcileAllNetworkPolicies(nb, context.Background())
		Expect(err).To(HaveOccurred())
		Eventually(recorder.Events).Should(Receive(And(
			ContainSubstring("Warning"),
			ContainSubstring("NetworkPolicyReconcileFailed"),
		)))
	})
})

type failingCreateClient struct {
	client.Client
}

func (c *failingCreateClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return apierrs.NewInternalError(context.DeadlineExceeded)
}

type failingUpdateClient struct {
	client.Client
}

func (c *failingUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return apierrs.NewInternalError(context.DeadlineExceeded)
}

type alreadyExistsClient struct {
	client.Client
	name     string
	getCalls int
}

func (c *alreadyExistsClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key.Name == c.name && c.getCalls == 0 {
		c.getCalls++
		return apierrs.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *alreadyExistsClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if obj.GetName() == c.name {
		return apierrs.NewAlreadyExists(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, obj.GetName())
	}
	return c.Client.Create(ctx, obj, opts...)
}
