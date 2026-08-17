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
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

var _ = Describe("validatePodSpecSecurity", func() {
	basePodSpec := func() *corev1.PodSpec {
		return &corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "test",
				Image: testContainerImage,
			}},
		}
	}

	It("allows a minimal valid pod spec", func() {
		Expect(validatePodSpecSecurity(basePodSpec(), "test-notebook")).To(Succeed())
	})

	It("allows a pod spec with the notebook service account", func() {
		podSpec := basePodSpec()
		podSpec.ServiceAccountName = "my-notebook"
		Expect(validatePodSpecSecurity(podSpec, "my-notebook")).To(Succeed())
	})

	It("rejects hostNetwork", func() {
		podSpec := basePodSpec()
		podSpec.HostNetwork = true
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostNetwork"))
	})

	It("rejects hostPID", func() {
		podSpec := basePodSpec()
		podSpec.HostPID = true
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostPID"))
	})

	It("rejects hostIPC", func() {
		podSpec := basePodSpec()
		podSpec.HostIPC = true
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostIPC"))
	})

	It("rejects hostPath volumes", func() {
		podSpec := basePodSpec()
		podSpec.Volumes = []corev1.Volume{{
			Name: "host",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/etc"},
			},
		}}
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostPath volume"))
	})

	It("rejects privileged containers", func() {
		podSpec := basePodSpec()
		podSpec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("privileged container"))
	})

	It("rejects privileged init containers", func() {
		podSpec := basePodSpec()
		podSpec.InitContainers = []corev1.Container{{
			Name:  "init",
			Image: "init-image:latest",
			SecurityContext: &corev1.SecurityContext{
				Privileged: ptr.To(true),
			},
		}}
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("privileged container"))
	})

	It("rejects arbitrary service accounts", func() {
		podSpec := basePodSpec()
		podSpec.ServiceAccountName = "admin"
		err := validatePodSpecSecurity(podSpec, "my-notebook")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("serviceAccountName"))
	})
})
