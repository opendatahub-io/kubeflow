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
	"testing"

	"strings"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInjectKubeRbacProxyWithResourceValidation(t *testing.T) {
	tests := []struct {
		name        string
		notebook    *nbv1.Notebook
		expectError bool
	}{
		{
			name: "valid notebook with custom resources",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:    "200m",
						AnnotationAuthSidecarMemoryRequest: "128Mi",
						AnnotationAuthSidecarCPULimit:      "400m",
						AnnotationAuthSidecarMemoryLimit:   "256Mi",
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "invalid notebook - request > limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-notebook",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "500m",
						AnnotationAuthSidecarCPULimit:   "200m",
					},
				},
				Spec: nbv1.NotebookSpec{
					Template: nbv1.NotebookTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "test-notebook",
									Image: "test-image",
								},
							},
						},
					},
				},
			},
			expectError: true,
		},
	}

	kubeRbacProxyImage := KubeRbacProxyConfig{
		ProxyImage: "test-rbac-image",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := InjectKubeRbacProxy(tt.notebook, kubeRbacProxyImage)
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				// Ensure no kube-rbac-proxy container was injected
				for _, c := range tt.notebook.Spec.Template.Spec.Containers {
					if c.Name == ContainerNameKubeRbacProxy {
						t.Errorf("kube-rbac-proxy container should not be injected when validation fails")
					}
				}
			} else {
				if err != nil {
					t.Errorf("expected no error but got: %v", err)
				}

				// Verify that kube-rbac-proxy container was added with correct resources
				var kubeRbacProxyContainer *corev1.Container
				for _, container := range tt.notebook.Spec.Template.Spec.Containers {
					if container.Name == ContainerNameKubeRbacProxy {
						kubeRbacProxyContainer = &container
						break
					}
				}

				if kubeRbacProxyContainer == nil {
					t.Errorf("kube-rbac-proxy container not found")
					return
				}

				// Verify that custom resources were applied if annotations were present
				annotations := tt.notebook.GetAnnotations()
				if annotations != nil {
					if cpuReq, exists := annotations[AnnotationAuthSidecarCPURequest]; exists {
						if kubeRbacProxyContainer.Resources.Requests.Cpu().String() != cpuReq {
							t.Errorf("expected CPU request %s, got %s", cpuReq, kubeRbacProxyContainer.Resources.Requests.Cpu().String())
						}
					}
				}
			}
		})
	}
}

func TestParseAndValidateAuthSidecarResources(t *testing.T) {
	tests := []struct {
		name          string
		notebook      *nbv1.Notebook
		expectError   bool
		errorContains string
		// Expected final values (including defaults) - only for valid cases
		expectedCPUReq   string
		expectedMemReq   string
		expectedCPULimit string
		expectedMemLimit string
	}{
		{
			name: "no annotations - all defaults",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectError:      false,
			expectedCPUReq:   DefaultAuthSidecarCPURequest,
			expectedMemReq:   DefaultAuthSidecarMemoryRequest,
			expectedCPULimit: DefaultAuthSidecarCPULimit,
			expectedMemLimit: DefaultAuthSidecarMemoryLimit,
		},
		{
			name: "all custom values",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:    "200m",
						AnnotationAuthSidecarMemoryRequest: "128Mi",
						AnnotationAuthSidecarCPULimit:      "400m",
						AnnotationAuthSidecarMemoryLimit:   "256Mi",
					},
				},
			},
			expectError:      false,
			expectedCPUReq:   "200m",
			expectedMemReq:   "128Mi",
			expectedCPULimit: "400m",
			expectedMemLimit: "256Mi",
		},
		{
			name: "partial annotations with defaults",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:  "50m",   // < default 100m limit
						AnnotationAuthSidecarMemoryLimit: "128Mi", // > default 64Mi request
					},
				},
			},
			expectError:      false,
			expectedCPUReq:   "50m",
			expectedMemReq:   DefaultAuthSidecarMemoryRequest, // default
			expectedCPULimit: DefaultAuthSidecarCPULimit,      // default
			expectedMemLimit: "128Mi",
		},
		{
			name: "whitespace trimming - spaces around values",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:    " 150m ",
						AnnotationAuthSidecarMemoryRequest: "\t256Mi\n",
						AnnotationAuthSidecarCPULimit:      " 300m ",
						AnnotationAuthSidecarMemoryLimit:   " 512Mi ",
					},
				},
			},
			expectError:      false,
			expectedCPUReq:   "150m",
			expectedMemReq:   "256Mi",
			expectedCPULimit: "300m",
			expectedMemLimit: "512Mi",
		},
		{
			name: "equal requests and limits",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest:    "200m",
						AnnotationAuthSidecarCPULimit:      "200m",
						AnnotationAuthSidecarMemoryRequest: "128Mi",
						AnnotationAuthSidecarMemoryLimit:   "128Mi",
					},
				},
			},
			expectError:      false,
			expectedCPUReq:   "200m",
			expectedMemReq:   "128Mi",
			expectedCPULimit: "200m",
			expectedMemLimit: "128Mi",
		},
		// Error cases
		{
			name: "invalid CPU request format",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "invalid-cpu",
					},
				},
			},
			expectError:   true,
			errorContains: "invalid value for annotation",
		},
		{
			name: "invalid memory request format",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryRequest: "invalid-memory",
					},
				},
			},
			expectError:   true,
			errorContains: "invalid value for annotation",
		},
		{
			name: "invalid CPU limit format",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPULimit: "invalid-cpu",
					},
				},
			},
			expectError:   true,
			errorContains: "invalid value for annotation",
		},
		{
			name: "invalid memory limit format",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryLimit: "invalid-memory",
					},
				},
			},
			expectError:   true,
			errorContains: "invalid value for annotation",
		},
		{
			name: "CPU request greater than explicit limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "500m",
						AnnotationAuthSidecarCPULimit:   "200m",
					},
				},
			},
			expectError:   true,
			errorContains: "CPU request (500m) cannot be greater than CPU limit (200m)",
		},
		{
			name: "memory request greater than explicit limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryRequest: "512Mi",
						AnnotationAuthSidecarMemoryLimit:   "256Mi",
					},
				},
			},
			expectError:   true,
			errorContains: "memory request (512Mi) cannot be greater than memory limit (256Mi)",
		},
		{
			name: "CPU request higher than default limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "200m", // > default 100m limit
					},
				},
			},
			expectError:   true,
			errorContains: "CPU request (200m) cannot be greater than CPU limit (100m)",
		},
		{
			name: "memory request higher than default limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryRequest: "128Mi", // > default 64Mi limit
					},
				},
			},
			expectError:   true,
			errorContains: "memory request (128Mi) cannot be greater than memory limit (64Mi)",
		},
		{
			name: "negative CPU request",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPURequest: "-100m",
					},
				},
			},
			expectError:   true,
			errorContains: "cannot be negative",
		},
		{
			name: "negative memory request",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryRequest: "-64Mi",
					},
				},
			},
			expectError:   true,
			errorContains: "cannot be negative",
		},
		{
			name: "negative CPU limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarCPULimit: "-200m",
					},
				},
			},
			expectError:   true,
			errorContains: "cannot be negative",
		},
		{
			name: "negative memory limit",
			notebook: &nbv1.Notebook{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						AnnotationAuthSidecarMemoryLimit: "-128Mi",
					},
				},
			},
			expectError:   true,
			errorContains: "cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config, err := parseAndValidateAuthSidecarResources(tt.notebook)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain '%s', got '%s'", tt.errorContains, err.Error())
				}
				return
			}

			if err != nil {
				t.Errorf("expected no error but got: %v", err)
				return
			}

			if config == nil {
				t.Errorf("expected config but got nil")
				return
			}

			// Verify the final computed values
			if config.cpuRequest.String() != tt.expectedCPUReq {
				t.Errorf("expected CPU request %s, got %s", tt.expectedCPUReq, config.cpuRequest.String())
			}
			if config.memoryRequest.String() != tt.expectedMemReq {
				t.Errorf("expected memory request %s, got %s", tt.expectedMemReq, config.memoryRequest.String())
			}
			if config.cpuLimit.String() != tt.expectedCPULimit {
				t.Errorf("expected CPU limit %s, got %s", tt.expectedCPULimit, config.cpuLimit.String())
			}
			if config.memoryLimit.String() != tt.expectedMemLimit {
				t.Errorf("expected memory limit %s, got %s", tt.expectedMemLimit, config.memoryLimit.String())
			}
		})
	}
}
