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
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	MLflowIdentifier           = "mlflow"
	MLflowClusterRoleName      = "mlflow-integration"
	MLflowTrackingURIEnvVar    = "MLFLOW_TRACKING_URI"
	MLflowK8sIntegrationEnvVar = "MLFLOW_K8S_INTEGRATION"
	MLflowInstanceAnnotation   = "opendatahub.io/mlflow-instance"
)

// mlflowRoleBindingName returns the canonical RoleBinding name for a notebook's MLflow integration.
func mlflowRoleBindingName(notebook *nbv1.Notebook) string {
	return fmt.Sprintf("%s-%s", notebook.Name, MLflowIdentifier)
}

// getMLflowInstanceAnnotation returns the trimmed instance name from the notebook annotation
// and a boolean indicating whether a non-empty value was present.
func getMLflowInstanceAnnotation(notebook *nbv1.Notebook) (string, bool) {
	val := strings.TrimSpace(notebook.Annotations[MLflowInstanceAnnotation])
	return val, val != ""
}

// setNotebookContainerEnvVar sets or updates an environment variable in the notebook container.
// Returns an error if the notebook container is not found.
func setNotebookContainerEnvVar(notebook *nbv1.Notebook, envVarName, envVarValue string) error {
	containers := notebook.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name != notebook.Name {
			continue
		}
		for j, env := range containers[i].Env {
			if env.Name == envVarName {
				if env.Value != envVarValue {
					containers[i].Env[j].Value = envVarValue
				}
				return nil
			}
		}
		containers[i].Env = append(containers[i].Env, corev1.EnvVar{
			Name:  envVarName,
			Value: envVarValue,
		})
		return nil
	}
	return fmt.Errorf("notebook image container not found")
}

// removeNotebookContainerEnvVar removes an environment variable from the notebook container.
func removeNotebookContainerEnvVar(notebook *nbv1.Notebook, envVarName string) {
	containers := notebook.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == notebook.Name {
			env := containers[i].Env
			for j, e := range env {
				if e.Name == envVarName {
					containers[i].Env = append(env[:j], env[j+1:]...)
					return
				}
			}
			return
		}
	}
}

// getMLflowTrackingURI constructs the MLflow Tracking URI from the GatewayAPI public endpoint.
// Reuses the existing getHostnameForPublicEndpoint function and appends '/<MLflowIdentifier>'.
// The path component is derived from the mlflow instance name:
//   - if instanceName == "mlflow" -> "/mlflow"
//   - otherwise -> "/mlflow-<instanceName>"
func getMLflowTrackingURI(ctx context.Context, dynamicClient dynamic.Interface, k8sClient client.Client, log logr.Logger, instanceName string) (string, error) {
	gatewayInstance, err := getGatewayInstance(ctx, dynamicClient, log)
	if err != nil {
		return "", fmt.Errorf("failed to get Gateway instance for MLflow tracking URI: %w", err)
	}

	hostname := getHostnameForPublicEndpoint(ctx, gatewayInstance, k8sClient, log)
	if hostname == "" {
		return "", fmt.Errorf("unable to determine hostname for MLflow tracking URI")
	}
	pathSegment := MLflowIdentifier
	if instanceName != "" && instanceName != MLflowIdentifier {
		pathSegment = fmt.Sprintf("%s-%s", MLflowIdentifier, instanceName)
	}

	trackingURI := fmt.Sprintf("https://%s/%s", hostname, pathSegment)
	return trackingURI, nil
}

// ReconcileMLflowRoleBinding will manage the RoleBinding reconciliation required
// for MLflow access by the notebook service account.
// The RoleBinding references the MLflow ClusterRole but is namespace-scoped for automatic cleanup.
func (r *OpenshiftNotebookReconciler) ReconcileMLflowRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	desiredRoleBinding := NewRoleBinding(notebook, mlflowRoleBindingName(notebook), "ClusterRole", MLflowClusterRoleName)
	if err := ctrl.SetControllerReference(notebook, desiredRoleBinding, r.Scheme); err != nil {
		log.Error(err, "Failed to set controller reference for MLflow RoleBinding")
		return err
	}

	foundRoleBinding := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      desiredRoleBinding.GetName(),
		Namespace: desiredRoleBinding.GetNamespace(),
	}, foundRoleBinding)
	if err != nil {
		if apierrs.IsNotFound(err) {
			err = r.Create(ctx, desiredRoleBinding)
			if err != nil && !apierrs.IsAlreadyExists(err) {
				log.Error(err, "Unable to create the MLflow RoleBinding")
				return err
			}
			return nil
		}
		log.Error(err, "Unable to fetch the MLflow RoleBinding")
		return err
	}

	// Update RoleBinding if Subjects, Labels, or OwnerReferences differ.
	// Mutate found in-place and update it (preserves ResourceVersion/UID).
	// Note: RoleRef is immutable in Kubernetes — if it differs, Update would fail anyway.
	subjectsDiffer := !equality.Semantic.DeepEqual(desiredRoleBinding.Subjects, foundRoleBinding.Subjects)
	labelsDiffer := !equality.Semantic.DeepEqual(desiredRoleBinding.Labels, foundRoleBinding.Labels)
	ownerRefsDiffer := !equality.Semantic.DeepEqual(desiredRoleBinding.OwnerReferences, foundRoleBinding.OwnerReferences)
	needsUpdate := subjectsDiffer || labelsDiffer || ownerRefsDiffer
	if needsUpdate {
		log.Info("Updating MLflow RoleBinding", "RoleBinding.Namespace", foundRoleBinding.Namespace, "RoleBinding.Name", foundRoleBinding.Name)
		foundRoleBinding.Subjects = desiredRoleBinding.Subjects
		foundRoleBinding.Labels = desiredRoleBinding.Labels
		foundRoleBinding.OwnerReferences = desiredRoleBinding.OwnerReferences
		err = r.Update(ctx, foundRoleBinding)
		if err != nil {
			log.Error(err, "Failed to update MLflow RoleBinding", "RoleBinding.Namespace", foundRoleBinding.Namespace, "RoleBinding.Name", foundRoleBinding.Name)
			return err
		}
	}

	return nil
}

// CleanupMLflowRoleBinding removes the RoleBinding associated with the notebook.
// This is called when MLflow integration is disabled to clean up the RoleBinding.
// Note: ownerReferences handle automatic deletion when the notebook is deleted, so no finalizer is needed.
func (r *OpenshiftNotebookReconciler) CleanupMLflowRoleBinding(notebook *nbv1.Notebook, ctx context.Context) error {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)

	// Delete the RoleBinding of the corresponding notebook
	roleBindingName := mlflowRoleBindingName(notebook)
	err := r.Delete(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      roleBindingName,
			Namespace: notebook.Namespace,
		},
	})
	if err != nil {
		if apierrs.IsNotFound(err) {
			return nil
		}
		log.Error(err, "Unable to delete MLflow RoleBinding", "roleBinding", roleBindingName)
		return err
	}

	return nil
}

// ReconcileMLflowIntegration performs the reconciliation of MLflow integration for a Notebook.
// This function:
// 1. Checks if MLflow integration annotation is defined
// 2. Verifies the MLflow ClusterRole exists (requeues if not yet available)
// 3. Creates/updates RoleBinding for MLflow access (references the MLflow ClusterRole) if annotation is present
// 4. Cleans up RoleBinding if annotation is undefined/empty
// Note: MLFLOW_TRACKING_URI and MLFLOW_K8S_INTEGRATION env var injection is handled in the webhook, not here.
//
// Unlike other sub-reconcilers that return only error, this returns (ctrl.Result, error) because
// it needs to express "retry later" when the MLflow ClusterRole doesn't exist yet — OpenShift
// rejects RoleBindings that reference a non-existent ClusterRole.
func (r *OpenshiftNotebookReconciler) ReconcileMLflowIntegration(notebook *nbv1.Notebook, ctx context.Context) (ctrl.Result, error) {
	log := r.Log.WithValues("notebook", notebook.Name, "namespace", notebook.Namespace)
	// Only reconcile RoleBinding when the notebook has the mlflow instance annotation defined (non-empty)
	if _, ok := getMLflowInstanceAnnotation(notebook); !ok {
		// ensure cleanup if annotation was removed
		if err := r.CleanupMLflowRoleBinding(notebook, ctx); err != nil {
			log.Error(err, "Failed to cleanup MLflow RoleBinding when integration annotation is missing")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check that the MLflow ClusterRole exists before creating the RoleBinding.
	// OpenShift rejects RoleBindings that reference a non-existent ClusterRole.
	clusterRoleExists, err := r.checkRoleExists(ctx, "ClusterRole", MLflowClusterRoleName, "")
	if err != nil {
		log.Error(err, "Error checking if MLflow ClusterRole exists")
		return ctrl.Result{}, err
	}
	if !clusterRoleExists {
		log.Info("MLflow ClusterRole does not exist yet, requeueing", "clusterRole", MLflowClusterRoleName)
		r.EventRecorder.Eventf(notebook, corev1.EventTypeWarning, "MLflowClusterRolePending",
			"Waiting for MLflow ClusterRole %q to be created", MLflowClusterRoleName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.ReconcileMLflowRoleBinding(notebook, ctx); err != nil {
		log.Error(err, "Failed to reconcile MLflow RoleBinding")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// HandleMLflowEnvVars handles MLflow-related environment variables in the notebook container.
// This function should be called from the webhook to ensure the env vars are available immediately.
//
// It may set the following environment variables:
//   - MLFLOW_K8S_INTEGRATION: set to 'true' when the 'opendatahub.io/mlflow-instance'
//     annotation is present and non-empty. If the annotation is absent or empty, this
//     variable will not be injected.
//   - MLFLOW_TRACKING_URI: set when the 'opendatahub.io/mlflow-instance' annotation
//     contains a non-empty instance name and a tracking URI can be determined.
func HandleMLflowEnvVars(ctx context.Context, cli client.Client, dynamicClient dynamic.Interface, notebook *nbv1.Notebook, log logr.Logger) {
	// Determine mlflow instance annotation (if present)
	instanceName, instanceEnabled := getMLflowInstanceAnnotation(notebook)

	// Only inject MLFLOW_K8S_INTEGRATION when the mlflow-instance annotation is present (non-empty).
	if instanceEnabled {
		if err := setNotebookContainerEnvVar(notebook, MLflowK8sIntegrationEnvVar, "true"); err != nil {
			log.Error(err, "Notebook image container not found, skipping MLflow K8s integration env var injection")
			// Don't fail webhook - MLflow integration is optional
			return
		}
	} else {
		// Ensure the var is removed when not enabled (do not inject "false")
		removeNotebookContainerEnvVar(notebook, MLflowK8sIntegrationEnvVar)
	}

	// Handle MLFLOW_TRACKING_URI based on integration status
	if !instanceEnabled {
		// Annotation is not present/empty - remove MLFLOW_TRACKING_URI if it exists
		removeNotebookContainerEnvVar(notebook, MLflowTrackingURIEnvVar)
		return
	}

	// Integration is enabled - try to get and set the tracking URI
	trackingURI, err := getMLflowTrackingURI(ctx, dynamicClient, cli, log, instanceName)
	if err != nil {
		log.Error(err, "Unable to determine MLflow tracking URI, skipping injection")
		// Don't fail webhook - just skip MLflow integration
		removeNotebookContainerEnvVar(notebook, MLflowTrackingURIEnvVar)
		return
	}

	// Set the tracking URI
	if err = setNotebookContainerEnvVar(notebook, MLflowTrackingURIEnvVar, trackingURI); err != nil {
		log.Error(err, "Notebook image container not found, skipping MLflow tracking URI injection")
	}
}
