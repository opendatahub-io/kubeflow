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
	"os"
	"testing"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestNotebooksForDSPA_RHOAIENG4531 verifies the DSPA Watch mapper function
// that was added to fix RHOAIENG-4531. When a DSPA changes, the mapper should
// return reconcile requests for all notebooks in the same namespace.
func TestNotebooksForDSPA_RHOAIENG4531(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nbv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add notebook scheme: %v", err)
	}
	if err := dspav1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add dspa scheme: %v", err)
	}

	tests := []struct {
		name              string
		setPipelineSecret string
		dspaNamespace     string
		notebooks         []nbv1.Notebook
		wantCount         int
		wantNamespaces    []string
		wantNames         []string
	}{
		{
			name:              "returns reconcile requests for all notebooks in DSPA namespace",
			setPipelineSecret: "true",
			dspaNamespace:     "test-ns",
			notebooks: []nbv1.Notebook{
				newTestNotebook("nb1", "test-ns"),
				newTestNotebook("nb2", "test-ns"),
			},
			wantCount:      2,
			wantNamespaces: []string{"test-ns", "test-ns"},
			wantNames:      []string{"nb1", "nb2"},
		},
		{
			name:              "returns empty when SET_PIPELINE_SECRET is not true",
			setPipelineSecret: "false",
			dspaNamespace:     "test-ns",
			notebooks: []nbv1.Notebook{
				newTestNotebook("nb1", "test-ns"),
			},
			wantCount: 0,
		},
		{
			name:              "returns empty when SET_PIPELINE_SECRET is unset",
			setPipelineSecret: "",
			dspaNamespace:     "test-ns",
			notebooks: []nbv1.Notebook{
				newTestNotebook("nb1", "test-ns"),
			},
			wantCount: 0,
		},
		{
			name:              "returns empty when no notebooks exist in namespace",
			setPipelineSecret: "true",
			dspaNamespace:     "empty-ns",
			notebooks:         []nbv1.Notebook{},
			wantCount:         0,
		},
		{
			name:              "only returns notebooks from DSPA namespace, not other namespaces",
			setPipelineSecret: "true",
			dspaNamespace:     "target-ns",
			notebooks: []nbv1.Notebook{
				newTestNotebook("nb-target", "target-ns"),
				newTestNotebook("nb-other", "other-ns"),
			},
			wantCount:      1,
			wantNamespaces: []string{"target-ns"},
			wantNames:      []string{"nb-target"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set env var
			if tt.setPipelineSecret == "" {
				os.Unsetenv("SET_PIPELINE_SECRET")
			} else {
				os.Setenv("SET_PIPELINE_SECRET", tt.setPipelineSecret)
			}
			defer os.Unsetenv("SET_PIPELINE_SECRET")

			// Build fake client with notebooks
			objs := make([]runtime.Object, len(tt.notebooks))
			for i := range tt.notebooks {
				objs[i] = &tt.notebooks[i]
			}
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithRuntimeObjects(objs...).
				Build()

			reconciler := &OpenshiftNotebookReconciler{
				Client: fakeClient,
				Log:    ctrl.Log.WithName("test"),
			}

			// Create a DSPA object as the trigger
			dspa := &dspav1.DataSciencePipelinesApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dspa",
					Namespace: tt.dspaNamespace,
				},
			}

			requests := reconciler.notebooksForDSPA(context.Background(), dspa)

			if len(requests) != tt.wantCount {
				t.Errorf("notebooksForDSPA() returned %d requests, want %d", len(requests), tt.wantCount)
			}

			for i, req := range requests {
				if i < len(tt.wantNames) {
					if req.NamespacedName.Name != tt.wantNames[i] {
						t.Errorf("request[%d].Name = %q, want %q", i, req.NamespacedName.Name, tt.wantNames[i])
					}
				}
				if i < len(tt.wantNamespaces) {
					if req.NamespacedName.Namespace != tt.wantNamespaces[i] {
						t.Errorf("request[%d].Namespace = %q, want %q", i, req.NamespacedName.Namespace, tt.wantNamespaces[i])
					}
				}
			}
		})
	}
}

func newTestNotebook(name, namespace string) nbv1.Notebook {
	return nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  name,
						Image: "registry.redhat.io/ubi9/ubi:latest",
					}},
				},
			},
		},
	}
}
