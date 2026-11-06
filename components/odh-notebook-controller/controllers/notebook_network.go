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
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	NotebookPort                             = 8888
	NotebookKubeRbacProxyPort                = 8443
	NotebookKubeRbacProxyHealthPort          = 8444
	NotebookKubeRbacProxyNetworkPolicySuffix = "-kube-rbac-proxy-np"
	NotebookEgressNetworkPolicySuffix        = "-egress-np"

	namespaceNameLabelKey = "kubernetes.io/metadata.name"
	notebookNameLabelKey  = "notebook-name"
	notebookKind          = "Notebook"
)

// ReconcileAllNetworkPolicies will manage the network policies reconciliation
// required by the notebook.
func (r *OpenshiftNotebookReconciler) ReconcileAllNetworkPolicies(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired Network Policies. Ingress and egress are kept as
	// separate objects so they can be changed independently, as permitted by
	// the operand NetworkPolicy contract.
	desiredNetworkPolicies := []*netv1.NetworkPolicy{
		NewNotebookNetworkPolicy(notebook, r.Namespace),
		NewKubeRbacProxyNetworkPolicy(notebook),
		NewEgressNetworkPolicy(notebook),
	}

	var errs []error
	for _, desiredNetworkPolicy := range desiredNetworkPolicies {
		if err := r.reconcileNetworkPolicy(desiredNetworkPolicy, ctx, notebook); err != nil {
			log.Error(err, "error reconciling Notebook network policy", "name", desiredNetworkPolicy.Name)
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (r *OpenshiftNotebookReconciler) recordNetworkPolicyEvent(notebook *nbv1.Notebook, eventType, reason, messageFmt string, args ...interface{}) {
	if r.EventRecorder == nil {
		return
	}
	r.EventRecorder.Eventf(notebook, eventType, reason, messageFmt, args...)
}

func (r *OpenshiftNotebookReconciler) reconcileNetworkPolicy(desiredNetworkPolicy *netv1.NetworkPolicy, ctx context.Context, notebook *nbv1.Notebook) error {
	// Create the Network Policy if it does not already exist.
	foundNetworkPolicy := &netv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredNetworkPolicy.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundNetworkPolicy)
	if err != nil {
		if !apierrs.IsNotFound(err) {
			return err
		}

		r.Log.Info("Creating Network Policy", "name", desiredNetworkPolicy.Name)
		// Add .metadata.ownerReferences to the Network Policy to be deleted by
		// the Kubernetes garbage collector if the notebook is deleted.
		if err := ctrl.SetControllerReference(notebook, desiredNetworkPolicy, r.Scheme); err != nil {
			r.recordNetworkPolicyEvent(notebook, corev1.EventTypeWarning, "NetworkPolicyReconcileFailed",
				"Failed to set owner reference on required NetworkPolicy %q: %v", desiredNetworkPolicy.Name, err)
			return err
		}
		// Create the NetworkPolicy in the OpenShift cluster.
		err = r.Create(ctx, desiredNetworkPolicy)
		if err == nil {
			return nil
		}
		if !apierrs.IsAlreadyExists(err) {
			r.recordNetworkPolicyEvent(notebook, corev1.EventTypeWarning, "NetworkPolicyReconcileFailed",
				"Failed to create required NetworkPolicy %q: %v", desiredNetworkPolicy.Name, err)
			return err
		}
		// Another actor created the policy between Get and Create. Fall through
		// to the existing-policy path so its spec is compared and reconciled.
	}

	return r.reconcileExistingNetworkPolicy(desiredNetworkPolicy, ctx, notebook)
}

func isKubeflowNotebookOwner(ownerRef *metav1.OwnerReference) bool {
	if ownerRef == nil || ownerRef.Kind != notebookKind {
		return false
	}
	groupVersion, err := schema.ParseGroupVersion(ownerRef.APIVersion)
	if err != nil || groupVersion.Group != nbv1.GroupVersion.Group {
		return false
	}
	// Versions within the Kubeflow API group refer to the same Notebook
	// resource. Accept the versions supported by this repository and reject
	// same-named owners from other API groups or unknown versions.
	switch groupVersion.Version {
	case "v1", "v1beta1", "v1alpha1":
		return true
	default:
		return false
	}
}

func (r *OpenshiftNotebookReconciler) networkPolicyOwnershipConflict(notebook *nbv1.Notebook, foundNetworkPolicy *netv1.NetworkPolicy) error {
	r.recordNetworkPolicyEvent(notebook, corev1.EventTypeWarning, "NetworkPolicyOwnershipConflict",
		"Refusing to overwrite NetworkPolicy %q because it is not controlled by this Notebook", foundNetworkPolicy.Name)
	return fmt.Errorf("networkpolicy %q is not controlled by Notebook %s", foundNetworkPolicy.Name, notebook.Name)
}

func (r *OpenshiftNotebookReconciler) reconcileExistingNetworkPolicy(desiredNetworkPolicy *netv1.NetworkPolicy, ctx context.Context, notebook *nbv1.Notebook) error {
	ownershipConflict := false
	// Retry the update operation when the ingress controller eventually
	// updates the resource version field.
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		foundNetworkPolicy := &netv1.NetworkPolicy{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      desiredNetworkPolicy.Name,
			Namespace: notebook.Namespace,
		}, foundNetworkPolicy); err != nil {
			return err
		}

		if !metav1.IsControlledBy(foundNetworkPolicy, notebook) {
			// Never overwrite a policy controlled by another resource, even when
			// its spec happens to match the desired policy.
			if controllerRef := metav1.GetControllerOf(foundNetworkPolicy); controllerRef != nil {
				if !isKubeflowNotebookOwner(controllerRef) || controllerRef.Name != notebook.Name {
					ownershipConflict = true
					return r.networkPolicyOwnershipConflict(notebook, foundNetworkPolicy)
				}

				// The policy is owned by a previous Notebook with the same name but
				// a different UID. That can happen if a Notebook is recreated before
				// garbage collection removes its policies. Drop the stale controller
				// reference so the policy can be adopted below.
				ownerRefs := foundNetworkPolicy.OwnerReferences[:0]
				for _, ownerRef := range foundNetworkPolicy.OwnerReferences {
					if ownerRef.Controller != nil && *ownerRef.Controller {
						continue
					}
					ownerRefs = append(ownerRefs, ownerRef)
				}
				foundNetworkPolicy.OwnerReferences = ownerRefs
			}

			// Adopt an unowned policy by taking controller ownership. This keeps
			// the pre-existing same-name behaviour for legacy policies, makes the
			// garbage collector remove them with the Notebook, and avoids leaving
			// drifted policies permanently unrepaired.
			if err := ctrl.SetControllerReference(notebook, foundNetworkPolicy, r.Scheme); err != nil {
				return err
			}
		} else if CompareNotebookNetworkPolicies(*desiredNetworkPolicy, *foundNetworkPolicy) {
			return nil
		}

		// Reconcile labels and spec in place. The policy names are part of the
		// lifecycle contract and must not be recreated.
		foundNetworkPolicy.Spec = desiredNetworkPolicy.Spec
		foundNetworkPolicy.Labels = desiredNetworkPolicy.Labels
		return r.Update(ctx, foundNetworkPolicy)
	})
	if err != nil && !ownershipConflict {
		r.Log.Error(err, "Unable to reconcile the Network Policy")
		r.recordNetworkPolicyEvent(notebook, corev1.EventTypeWarning, "NetworkPolicyReconcileFailed",
			"Failed to repair required NetworkPolicy %q: %v", desiredNetworkPolicy.Name, err)
	}
	return err
}

// CompareNotebookNetworkPolicies checks if two services are equal, if not return false
func CompareNotebookNetworkPolicies(np1 netv1.NetworkPolicy, np2 netv1.NetworkPolicy) bool {
	// Two network policies will be equal if the labels and specs are identical
	return reflect.DeepEqual(np1.Labels, np2.Labels) &&
		reflect.DeepEqual(np1.Spec, np2.Spec)
}

func notebookPodSelector(notebook *nbv1.Notebook) metav1.LabelSelector {
	return metav1.LabelSelector{
		MatchLabels: map[string]string{
			notebookNameLabelKey: notebook.Name,
		},
	}
}

func controllerNamespacePeer(namespace string) netv1.NetworkPolicyPeer {
	return netv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				namespaceNameLabelKey: namespace,
			},
		},
	}
}

func notebookPortRule() netv1.NetworkPolicyPort {
	protocol := corev1.ProtocolTCP
	return netv1.NetworkPolicyPort{
		Protocol: &protocol,
		Port: &intstr.IntOrString{
			IntVal: NotebookPort,
		},
	}
}

// NewNotebookNetworkPolicy defines the desired network policy for Notebook port.
//
// The 8888 peer intentionally covers the whole controller namespace. On RHOAI
// the controller namespace is a shared platform namespace
// (redhat-ods-applications) hosting the upstream notebook-controller, whose idle
// culler calls the notebook Service on 8888, as well as this controller and
// other platform workloads. Narrowing the peer belongs in an administrator
// profile passed down through the Workbenches CR; until that plumbing exists,
// keep the existing behaviour rather than risk breaking the culler.
//
// Note: This policy needs to update if there is change in the notebook Port.
func NewNotebookNetworkPolicy(notebook *nbv1.Notebook, controllerNamespace string) *netv1.NetworkPolicy {
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-ctrl-np",
			Namespace: notebook.Namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: notebookPodSelector(notebook),
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{notebookPortRule()},
					From:  []netv1.NetworkPolicyPeer{controllerNamespacePeer(controllerNamespace)},
				},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeIngress,
			},
		},
	}
}

// NewKubeRbacProxyNetworkPolicy defines the desired kube-rbac-proxy Network Policy.
//
// Endpoint: kube-rbac-proxy front door on 8443 for user/browser traffic arriving
// through the platform gateway/ingress (OpenShift router/HAProxy, Gateway API /
// istio, or host-network implementations).
//
// Why there is no "from" peer: the caller identity differs per supported
// platform, so no stable pod/namespace selector represents every gateway
// implementation. This is the contract's documented compatibility rule
// (unrestricted sources, known port). Control in depth is provided by
// kube-rbac-proxy, which terminates TLS and performs authentication and
// authorization (SubjectAccessReview) before proxying to the notebook on 8888.
// Narrowing this rule belongs in an administrator profile passed down through
// the Workbenches CR.
func NewKubeRbacProxyNetworkPolicy(notebook *nbv1.Notebook) *netv1.NetworkPolicy {
	protocol := corev1.ProtocolTCP
	// Create a Kubernetes NetworkPolicy resource that allows traffic to the
	// kube-rbac-proxy port of a notebook.
	// Note: This policy needs to update if there is a change in kube-rbac-proxy Port or Webhook Port.
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + NotebookKubeRbacProxyNetworkPolicySuffix,
			Namespace: notebook.Namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: notebookPodSelector(notebook),
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &protocol,
							Port: &intstr.IntOrString{
								IntVal: NotebookKubeRbacProxyPort,
							},
						},
					},
				},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeIngress,
			},
		},
	}
}

// NewEgressNetworkPolicy defines the required egress policy for the Notebook.
//
// Outbound traffic is intentionally unrestricted (egress: - {}):
//   - notebooks reach external object storage, package registries, model
//     registries, managed databases, and arbitrary user Services;
//   - NetworkPolicy cannot select DNS names or dynamically addressed services,
//     which is the operand contract's explicit allow-all-egress carve-out.
//
// External controls relied upon: egress firewalls and platform/infrastructure
// network policy. The policy exists so every Notebook workload is selected by
// an egress policy even when the namespace has no blanket platform policy.
func NewEgressNetworkPolicy(notebook *nbv1.Notebook) *netv1.NetworkPolicy {
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + NotebookEgressNetworkPolicySuffix,
			Namespace: notebook.Namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: notebookPodSelector(notebook),
			Egress: []netv1.NetworkPolicyEgressRule{
				{},
			},
			PolicyTypes: []netv1.PolicyType{
				netv1.PolicyTypeEgress,
			},
		},
	}
}
