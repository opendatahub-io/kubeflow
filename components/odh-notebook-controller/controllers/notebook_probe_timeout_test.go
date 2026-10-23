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
	"fmt"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	probeTimeoutHealthPath = "/healthz"
	falseString            = "false"
)

func newProbeTimeoutNotebook(name string, annotations map[string]string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   Namespace,
			Annotations: annotations,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  name,
							Image: "quay.io/example/workbench:latest",
							LivenessProbe: &corev1.Probe{
								ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: probeTimeoutHealthPath, Port: intstr.FromInt32(8888)}},
								TimeoutSeconds: 1,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(8888)}},
								TimeoutSeconds: 7,
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/startup", Port: intstr.FromInt32(8888)}},
								TimeoutSeconds: 3,
							},
						},
						{
							Name:  "other-sidecar",
							Image: "quay.io/example/sidecar:latest",
							LivenessProbe: &corev1.Probe{
								ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: probeTimeoutHealthPath, Port: intstr.FromInt32(8080)}},
								TimeoutSeconds: 3,
							},
						},
					},
				},
			},
		},
	}
}

var _ = Describe("workbench probe timeout admission", func() {
	var createdNotebooks []*nbv1.Notebook
	var settings *corev1.ConfigMap

	AfterEach(func() {
		for _, notebook := range createdNotebooks {
			Expect(cli.Delete(ctx, notebook)).To(Succeed())
		}
		if settings != nil {
			Expect(cli.Delete(ctx, settings)).To(Succeed())
		}
		createdNotebooks = nil
		settings = nil
	})

	It("applies the current configured timeout to workbench and injected proxy probes", func() {
		settings = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: notebookControllerSettingsConfigMap, Namespace: odhNotebookControllerTestNamespace},
			Data:       map[string]string{workbenchProbeTimeoutKey: "5"},
		}
		Expect(cli.Create(ctx, settings)).To(Succeed())

		notebook := newProbeTimeoutNotebook("configured-probe-timeout", map[string]string{AnnotationInjectAuth: trueString})
		Expect(cli.Create(ctx, notebook)).To(Succeed())
		createdNotebooks = append(createdNotebooks, notebook)
		Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())

		containers := notebook.Spec.Template.Spec.Containers
		Expect(containers).To(HaveLen(3))
		Expect(containers[0].LivenessProbe.TimeoutSeconds).To(Equal(int32(5)))
		Expect(containers[0].ReadinessProbe.TimeoutSeconds).To(Equal(int32(5)))
		Expect(containers[0].StartupProbe.TimeoutSeconds).To(Equal(int32(3)))
		Expect(containers[1].LivenessProbe.TimeoutSeconds).To(Equal(int32(3)))
		Expect(containers[2].Name).To(Equal(ContainerNameKubeRbacProxy))
		Expect(containers[2].LivenessProbe.TimeoutSeconds).To(Equal(int32(5)))
		Expect(containers[2].ReadinessProbe.TimeoutSeconds).To(Equal(int32(5)))

		settings.Data[workbenchProbeTimeoutKey] = "9"
		Expect(cli.Update(ctx, settings)).To(Succeed())
		updatedNotebook := newProbeTimeoutNotebook("updated-probe-timeout", nil)
		Expect(cli.Create(ctx, updatedNotebook)).To(Succeed())
		createdNotebooks = append(createdNotebooks, updatedNotebook)
		Expect(cli.Get(ctx, types.NamespacedName{Name: updatedNotebook.Name, Namespace: updatedNotebook.Namespace}, updatedNotebook)).To(Succeed())
		Expect(updatedNotebook.Spec.Template.Spec.Containers[0].LivenessProbe.TimeoutSeconds).To(Equal(int32(9)))
		Expect(updatedNotebook.Spec.Template.Spec.Containers[0].ReadinessProbe.TimeoutSeconds).To(Equal(int32(9)))
	})

	It("admits creates and updates when the container name differs from the Notebook name", func() {
		settings = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: notebookControllerSettingsConfigMap, Namespace: odhNotebookControllerTestNamespace},
			Data:       map[string]string{workbenchProbeTimeoutKey: "5"},
		}
		Expect(cli.Create(ctx, settings)).To(Succeed())

		for index, injectAuth := range []string{falseString, trueString} {
			By(fmt.Sprintf("Admitting a differently named container with proxy injection %s", injectAuth))
			notebook := newProbeTimeoutNotebook(fmt.Sprintf("mismatched-probe-timeout-%d", index), map[string]string{AnnotationInjectAuth: injectAuth})
			notebook.Spec.Template.Spec.Containers[0].Name = "custom-workbench"
			originalContainers := notebook.DeepCopy().Spec.Template.Spec.Containers
			Expect(cli.Create(ctx, notebook)).To(Succeed())
			createdNotebooks = append(createdNotebooks, notebook)
			Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())
			Expect(notebook.Spec.Template.Spec.Containers[:2]).To(Equal(originalContainers))

			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook); err != nil {
					return err
				}
				if notebook.Labels == nil {
					notebook.Labels = make(map[string]string)
				}
				notebook.Labels["probe-timeout-test"] = "updated"
				return cli.Update(ctx, notebook)
			})).To(Succeed())
			Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())
			containers := notebook.Spec.Template.Spec.Containers
			Expect(containers[:2]).To(Equal(originalContainers))
			if injectAuth == trueString {
				Expect(containers).To(HaveLen(3))
				Expect(containers[2].Name).To(Equal(ContainerNameKubeRbacProxy))
				Expect(containers[2].LivenessProbe.TimeoutSeconds).To(Equal(int32(5)))
				Expect(containers[2].ReadinessProbe.TimeoutSeconds).To(Equal(int32(5)))
			} else {
				Expect(containers).To(HaveLen(2))
			}
		}
	})

	It("preserves existing probe timeouts when the setting is absent", func() {
		notebook := newProbeTimeoutNotebook("unconfigured-probe-timeout", map[string]string{AnnotationInjectAuth: trueString})
		Expect(cli.Create(ctx, notebook)).To(Succeed())
		createdNotebooks = append(createdNotebooks, notebook)
		Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())

		containers := notebook.Spec.Template.Spec.Containers
		Expect(containers[0].LivenessProbe.TimeoutSeconds).To(Equal(int32(1)))
		Expect(containers[0].ReadinessProbe.TimeoutSeconds).To(Equal(int32(7)))
		Expect(containers[2].Name).To(Equal(ContainerNameKubeRbacProxy))
		Expect(containers[2].LivenessProbe.TimeoutSeconds).To(Equal(int32(1)))
		Expect(containers[2].ReadinessProbe.TimeoutSeconds).To(Equal(int32(1)))
	})

	It("defers a timeout change until a running workbench is explicitly restarted", func() {
		settings = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: notebookControllerSettingsConfigMap, Namespace: odhNotebookControllerTestNamespace},
			Data:       map[string]string{workbenchProbeTimeoutKey: "5"},
		}
		Expect(cli.Create(ctx, settings)).To(Succeed())

		notebook := newProbeTimeoutNotebook("pending-probe-timeout", nil)
		Expect(cli.Create(ctx, notebook)).To(Succeed())
		createdNotebooks = append(createdNotebooks, notebook)
		// The webhook injects kubeflow-resource-stopped on create and the controller
		// normally removes it after initial reconciliation. Remove it explicitly here
		// so this admission test does not depend on controller queue timing.
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook); err != nil {
				return err
			}
			delete(notebook.Annotations, "kubeflow-resource-stopped")
			return cli.Update(ctx, notebook)
		})).To(Succeed())
		Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())
		Expect(metav1.HasAnnotation(notebook.ObjectMeta, "kubeflow-resource-stopped")).To(BeFalse())

		settings.Data[workbenchProbeTimeoutKey] = "9"
		Expect(cli.Update(ctx, settings)).To(Succeed())
		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook); err != nil {
				return err
			}
			if notebook.Labels == nil {
				notebook.Labels = make(map[string]string)
			}
			notebook.Labels["probe-timeout-test"] = "pending"
			return cli.Update(ctx, notebook)
		})).To(Succeed())
		Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())
		Expect(notebook.Spec.Template.Spec.Containers[0].LivenessProbe.TimeoutSeconds).To(Equal(int32(5)))
		Expect(notebook.Annotations["notebooks.opendatahub.io/update-pending"]).ToNot(BeEmpty())

		Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook); err != nil {
				return err
			}
			if notebook.Annotations == nil {
				notebook.Annotations = make(map[string]string)
			}
			notebook.Annotations["notebooks.opendatahub.io/notebook-restart"] = trueString
			return cli.Update(ctx, notebook)
		})).To(Succeed())
		Expect(cli.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, notebook)).To(Succeed())
		Expect(notebook.Spec.Template.Spec.Containers[0].LivenessProbe.TimeoutSeconds).To(Equal(int32(9)))
		Expect(notebook.Annotations["notebooks.opendatahub.io/update-pending"]).To(BeEmpty())
	})

	It("rejects invalid configured timeouts", func() {
		settings = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: notebookControllerSettingsConfigMap, Namespace: odhNotebookControllerTestNamespace},
			Data:       map[string]string{workbenchProbeTimeoutKey: "0"},
		}
		Expect(cli.Create(ctx, settings)).To(Succeed())

		for index, value := range []string{"0", "-1", "invalid", "2147483648", ""} {
			settings.Data[workbenchProbeTimeoutKey] = value
			Expect(cli.Update(ctx, settings)).To(Succeed())
			notebook := newProbeTimeoutNotebook(fmt.Sprintf("invalid-probe-timeout-%d", index), nil)
			err := cli.Create(ctx, notebook)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(workbenchProbeTimeoutKey))
		}
	})
})
