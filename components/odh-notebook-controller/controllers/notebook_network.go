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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	NotebookPort           = 8888
	NotebookRbacPort       = 8443
	NotebookRbacHealthPort = 8444
)

// ReconcileAllNetworkPolicies will manage the network policies reconciliation
// required by the notebook.
func (r *OpenshiftNotebookReconciler) ReconcileAllNetworkPolicies(notebook *nbv1.Notebook, ctx context.Context) error {
	// Initialize logger format
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Generate the desired Network Policies
	desiredNotebookNetworkPolicy := NewNotebookNetworkPolicy(notebook, log, r.Namespace)

	// Create Network Policies if they do not already exist
	err := r.reconcileNetworkPolicy(desiredNotebookNetworkPolicy, ctx, notebook)
	if err != nil {
		log.Error(err, "error creating Notebook network policy")
		return err
	}

	desiredRbacNetworkPolicy := NewRbacNetworkPolicy(notebook)
	err = r.reconcileNetworkPolicy(desiredRbacNetworkPolicy, ctx, notebook)
	if err != nil {
		log.Error(err, "error creating Notebook rbac-proxy network policy")
		return err
	}

	return nil
}

func (r *OpenshiftNotebookReconciler) reconcileNetworkPolicy(desiredNetworkPolicy *netv1.NetworkPolicy, ctx context.Context, notebook *nbv1.Notebook) error {

	// Create the Network Policy if it does not already exist
	foundNetworkPolicy := &netv1.NetworkPolicy{}
	justCreated := false
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredNetworkPolicy.GetName(),
		Namespace: notebook.GetNamespace(),
	}, foundNetworkPolicy)
	if err != nil {
		if apierrs.IsNotFound(err) {
			r.Log.Info("Creating Network Policy", "name", desiredNetworkPolicy.Name)
			// Add .metatada.ownerReferences to the Network Policy to be deleted by
			// the Kubernetes garbage collector if the notebook is deleted
			err = ctrl.SetControllerReference(notebook, desiredNetworkPolicy, r.Scheme)
			if err != nil {
				return err
			}
			// Create the NetworkPolicy in the Openshift cluster
			err = r.Create(ctx, desiredNetworkPolicy)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				return err
			}
			justCreated = true
		} else {
			return err
		}
	}

	// Reconcile the NetworkPolicy spec if it has been manually modified
	if !justCreated && !CompareNotebookNetworkPolicies(*desiredNetworkPolicy, *foundNetworkPolicy) {
		r.Log.Info("Reconciling Network policy", "name", foundNetworkPolicy.Name)
		// Retry the update operation when the ingress controller eventually
		// updates the resource version field
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Get the last route revision
			if err := r.Get(ctx, types.NamespacedName{
				Name:      desiredNetworkPolicy.Name,
				Namespace: notebook.Namespace,
			}, foundNetworkPolicy); err != nil {
				return err
			}
			// Reconcile labels and spec field
			foundNetworkPolicy.Spec = desiredNetworkPolicy.Spec
			foundNetworkPolicy.ObjectMeta.Labels = desiredNetworkPolicy.ObjectMeta.Labels
			return r.Update(ctx, foundNetworkPolicy)
		})
		if err != nil {
			r.Log.Error(err, "Unable to reconcile the Network Policy")
			return err
		}
	}

	return nil
}

// CompareNotebookNetworkPolicies checks if two services are equal, if not return false
func CompareNotebookNetworkPolicies(np1 netv1.NetworkPolicy, np2 netv1.NetworkPolicy) bool {
	// Two network policies will be equal if the labels and specs are identical
	return reflect.DeepEqual(np1.ObjectMeta.Labels, np2.ObjectMeta.Labels) &&
		reflect.DeepEqual(np1.Spec, np2.Spec)
}

// NewNotebookNetworkPolicy defines the desired network policy for Notebook port
func NewNotebookNetworkPolicy(notebook *nbv1.Notebook, log logr.Logger, namespace string) *netv1.NetworkPolicy {
	npProtocol := corev1.ProtocolTCP
	namespaceSel := metav1.LabelSelector{
		MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": namespace,
		},
	}
	// Create a Kubernetes NetworkPolicy resource that allows traffic from notebook controller to the Notebook container
	// Note: This policy needs to update if there is change in the notebook Port.
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-ctrl-np",
			Namespace: notebook.Namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"notebook-name": notebook.Name,
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &npProtocol,
							Port: &intstr.IntOrString{
								IntVal: NotebookPort,
							},
						},
					},
					From: []netv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &namespaceSel,
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

// NewRbacNetworkPolicy defines the desired rbac-proxy Network Policy
func NewRbacNetworkPolicy(notebook *nbv1.Notebook) *netv1.NetworkPolicy {

	npProtocol := corev1.ProtocolTCP
	// Create a Kubernetes NetworkPolicy resource that allows all traffic to the rbac-proxy port of a notebook
	// Note: This policy needs to update if there is a change in rbac-proxy Port or Webhook Port.
	return &netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      notebook.Name + "-rbac-np",
			Namespace: notebook.Namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"notebook-name": notebook.Name,
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &npProtocol,
							Port: &intstr.IntOrString{
								IntVal: NotebookRbacPort,
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
