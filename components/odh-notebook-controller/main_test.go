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

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const keepThisStr = "keep-this"

func TestStripConfigMapData(t *testing.T) {
	tests := []struct {
		name            string
		input           interface{}
		wantAnnotations map[string]string
	}{
		{
			name: "strips Data, BinaryData, and ManagedFields",
			input: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cm",
					Namespace: "default",
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
					},
				},
				Data:       map[string]string{"key": "value-900KB-payload"},
				BinaryData: map[string][]byte{"bin": []byte("binary-payload")},
			},
		},
		{
			name: "handles nil Data and BinaryData",
			input: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-cm",
					Namespace: "default",
				},
			},
		},
		{
			name:  "passes through non-ConfigMap objects unchanged",
			input: "not-a-configmap",
		},
		{
			name: "handles nil Annotations without panic",
			input: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-annotations-cm",
					Namespace: "default",
				},
			},
			wantAnnotations: map[string]string{},
		},
		{
			name: "strips last-applied-configuration annotation while preserving others",
			input: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "annotated-cm",
					Namespace: "default",
					Annotations: map[string]string{
						"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"ConfigMap"}`,
						"custom-annotation": keepThisStr,
					},
				},
				Data: map[string]string{"key": "value"},
			},
			wantAnnotations: map[string]string{"custom-annotation": keepThisStr},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var expectedName string
			if in, ok := tt.input.(*corev1.ConfigMap); ok {
				expectedName = in.Name
			}

			result, err := stripConfigMapData(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			cm, ok := result.(*corev1.ConfigMap)
			if !ok {
				// Non-ConfigMap input should pass through unchanged
				if result != tt.input {
					t.Fatalf("non-ConfigMap input was modified")
				}
				return
			}

			if cm.Data != nil {
				t.Errorf("Data should be nil, got %v", cm.Data)
			}
			if cm.BinaryData != nil {
				t.Errorf("BinaryData should be nil, got %v", cm.BinaryData)
			}
			if cm.ManagedFields != nil {
				t.Errorf("ManagedFields should be nil, got %v", cm.ManagedFields)
			}
			if cm.Name != expectedName {
				t.Errorf("Name was modified")
			}
			if tt.wantAnnotations != nil {
				for k, v := range tt.wantAnnotations {
					if cm.Annotations[k] != v {
						t.Errorf("Annotation %q = %q, want %q", k, cm.Annotations[k], v)
					}
				}
				for k := range cm.Annotations {
					if _, expected := tt.wantAnnotations[k]; !expected {
						t.Errorf("unexpected annotation %q present after stripping", k)
					}
				}
			}
		})
	}
}

func TestStripSecretData(t *testing.T) {
	tests := []struct {
		name            string
		input           interface{}
		wantAnnotations map[string]string
	}{
		{
			name: "strips Data, StringData, and ManagedFields",
			input: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-secret",
					Namespace: "default",
					ManagedFields: []metav1.ManagedFieldsEntry{
						{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply},
					},
				},
				Data:       map[string][]byte{"password": []byte("supersecret")},
				StringData: map[string]string{"token": "bearer-token-value"},
			},
		},
		{
			name: "handles nil Data and StringData",
			input: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-secret",
					Namespace: "default",
				},
			},
		},
		{
			name:  "passes through non-Secret objects unchanged",
			input: "not-a-secret",
		},
		{
			name: "handles nil Annotations without panic",
			input: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-annotations-secret",
					Namespace: "default",
				},
			},
			wantAnnotations: map[string]string{},
		},
		{
			name: "strips last-applied-configuration annotation while preserving others",
			input: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "annotated-secret",
					Namespace: "default",
					Annotations: map[string]string{
						"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","kind":"Secret"}`,
						"custom-annotation": keepThisStr,
					},
				},
				Data: map[string][]byte{"key": []byte("value")},
			},
			wantAnnotations: map[string]string{"custom-annotation": keepThisStr},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var expectedName string
			if in, ok := tt.input.(*corev1.Secret); ok {
				expectedName = in.Name
			}

			result, err := stripSecretData(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			s, ok := result.(*corev1.Secret)
			if !ok {
				if result != tt.input {
					t.Fatalf("non-Secret input was modified")
				}
				return
			}

			if s.Data != nil {
				t.Errorf("Data should be nil, got %v", s.Data)
			}
			if s.StringData != nil {
				t.Errorf("StringData should be nil, got %v", s.StringData)
			}
			if s.ManagedFields != nil {
				t.Errorf("ManagedFields should be nil, got %v", s.ManagedFields)
			}
			if s.Name != expectedName {
				t.Errorf("Name was modified")
			}
			if tt.wantAnnotations != nil {
				for k, v := range tt.wantAnnotations {
					if s.Annotations[k] != v {
						t.Errorf("Annotation %q = %q, want %q", k, s.Annotations[k], v)
					}
				}
				for k := range s.Annotations {
					if _, expected := tt.wantAnnotations[k]; !expected {
						t.Errorf("unexpected annotation %q present after stripping", k)
					}
				}
			}
		})
	}
}

func TestStripConfigMapData_PreservesLabelsAndAnnotations(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "labeled-cm",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
			Annotations: map[string]string{
				"custom-annotation": keepThisStr,
			},
		},
		Data: map[string]string{"large-key": "large-value"},
	}

	result, err := stripConfigMapData(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stripped := result.(*corev1.ConfigMap)
	if stripped.Data != nil {
		t.Errorf("Data should be nil")
	}
	if stripped.Labels["app"] != "test" {
		t.Errorf("Labels should be preserved")
	}
	if stripped.Annotations["custom-annotation"] != keepThisStr {
		t.Errorf("Custom annotations should be preserved")
	}
}

func TestStripSecretData_PreservesLabelsAndAnnotations(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "labeled-secret",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
			Annotations: map[string]string{
				"custom-annotation": keepThisStr,
			},
		},
		Data: map[string][]byte{"key": []byte("value")},
	}

	result, err := stripSecretData(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stripped := result.(*corev1.Secret)
	if stripped.Data != nil {
		t.Errorf("Data should be nil")
	}
	if stripped.Labels["app"] != "test" {
		t.Errorf("Labels should be preserved")
	}
	if stripped.Annotations["custom-annotation"] != keepThisStr {
		t.Errorf("Custom annotations should be preserved")
	}
}

func TestStripConfigMapData_RemovesLastAppliedConfiguration(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-cm",
			Namespace: "default",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","data":{"key":"large-payload"},"kind":"ConfigMap"}`,
				"custom-annotation": keepThisStr,
			},
		},
		Data: map[string]string{"key": "value"},
	}

	result, err := stripConfigMapData(cm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stripped := result.(*corev1.ConfigMap)
	if _, exists := stripped.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; exists {
		t.Errorf("last-applied-configuration annotation should be removed")
	}
	if stripped.Annotations["custom-annotation"] != keepThisStr {
		t.Errorf("other annotations should be preserved")
	}
}

func TestStripSecretData_RemovesLastAppliedConfiguration(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "applied-secret",
			Namespace: "default",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"apiVersion":"v1","data":{"password":"someSecretValue"},"kind":"Secret"}`,
				"custom-annotation": keepThisStr,
			},
		},
		Data: map[string][]byte{"password": []byte("supersecret")},
	}

	result, err := stripSecretData(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stripped := result.(*corev1.Secret)
	if _, exists := stripped.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; exists {
		t.Errorf("last-applied-configuration annotation should be removed")
	}
	if stripped.Annotations["custom-annotation"] != keepThisStr {
		t.Errorf("other annotations should be preserved")
	}
}

func TestStripSecretData_PreservesType(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "typed-secret",
			Namespace: "default",
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"key": []byte("value")},
	}

	result, err := stripSecretData(secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	stripped := result.(*corev1.Secret)
	if stripped.Data != nil {
		t.Errorf("Data should be nil")
	}
	if stripped.Type != corev1.SecretTypeOpaque {
		t.Errorf("Type should be preserved, got %v", stripped.Type)
	}
}
