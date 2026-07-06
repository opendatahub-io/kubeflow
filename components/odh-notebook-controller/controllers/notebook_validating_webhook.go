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
	"net/http"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

//+kubebuilder:webhook:path=/validate-notebook-v1,mutating=false,failurePolicy=fail,sideEffects=None,groups=kubeflow.org,resources=notebooks,verbs=update,versions=v1,name=notebooks-validation.opendatahub.io,admissionReviewVersions=v1

// NotebookValidatingWebhook validates Notebook updates
type NotebookValidatingWebhook struct {
	Log     logr.Logger
	Client  client.Client
	Decoder admission.Decoder
}

// Handle validates Notebook updates
func (v *NotebookValidatingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := v.Log.WithValues("notebook", req.Name, "namespace", req.Namespace)

	// Only validate updates
	if req.Operation != admissionv1.Update {
		return admission.Allowed("")
	}

	// Decode the new notebook
	newNotebook := &nbv1.Notebook{}
	if err := v.Decoder.Decode(req, newNotebook); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Decode the old notebook
	oldNotebook := &nbv1.Notebook{}
	if err := v.Decoder.DecodeRaw(req.OldObject, oldNotebook); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Check if MLflow annotation is being removed
	if err := v.validateMLflowAnnotationRemoval(oldNotebook, newNotebook, log); err != nil {
		return admission.Denied(err.Error())
	}

	return admission.Allowed("")
}

// validateMLflowAnnotationRemoval checks if MLflow annotation removal is allowed.
// The MLflow annotation cannot be removed while the notebook is running because:
// - The RoleBinding is deleted immediately on annotation removal
// - But the environment variables persist until the pod restarts
// - This creates a broken state where MLflow client has env vars but no permissions
//
// Note: We intentionally do NOT allow removal when the restart annotation is present.
// The restart annotation only signals intent - it doesn't guarantee immediate restart.
// There could be delays (grace period, image pulls, scheduling) or failures that
// leave the notebook running with env vars but no RoleBinding permissions.
func (v *NotebookValidatingWebhook) validateMLflowAnnotationRemoval(oldNotebook, newNotebook *nbv1.Notebook, log logr.Logger) error {
	// Check if notebook is stopped - if so, allow any changes
	if metav1.HasAnnotation(newNotebook.ObjectMeta, culler.STOP_ANNOTATION) {
		return nil
	}

	// Get old and new MLflow annotation values
	oldInstance, oldHasMLflow := getMLflowInstanceAnnotation(oldNotebook)
	_, newHasMLflow := getMLflowInstanceAnnotation(newNotebook)

	// If MLflow was enabled and is now being disabled/removed on a running notebook
	if oldHasMLflow && !newHasMLflow {
		log.Info("Rejecting MLflow annotation removal on running notebook",
			"oldInstance", oldInstance)
		return fmt.Errorf(
			"cannot remove '%s' annotation while the notebook is running; "+
				"please stop the notebook first, then remove the annotation",
			MLflowInstanceAnnotation)
	}

	return nil
}
