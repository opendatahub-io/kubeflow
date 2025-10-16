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
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

var _ = Describe("The Openshift Notebook controller", func() {
	// Define utility constants for testing timeouts/durations and intervals.
	const (
		duration = 10 * time.Second
		interval = 200 * time.Millisecond
	)

	When("Creating a Notebook", func() {
		const (
			Name      = "test-notebook"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		pathPrefix := gatewayv1.PathMatchPathPrefix
		expectedHTTPRoute := gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{
							Group:     func() *gatewayv1.Group { g := gatewayv1.Group("gateway.networking.k8s.io"); return &g }(),
							Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
							Name:      gatewayv1.ObjectName("data-science-gateway"),
							Namespace: func() *gatewayv1.Namespace { ns := gatewayv1.Namespace("openshift-ingress"); return &ns }(),
						},
					},
				},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathPrefix,
									Value: (*string)(&[]string{"/notebook/" + Namespace + "/" + Name}[0]),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: func() *gatewayv1.Group { g := gatewayv1.Group(""); return &g }(),
										Kind:  func() *gatewayv1.Kind { k := gatewayv1.Kind("Service"); return &k }(),
										Name:  gatewayv1.ObjectName(Name),
										Port:  (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8888}[0]),
									},
									Weight: func() *int32 { w := int32(1); return &w }(),
								},
							},
						},
					},
				},
			},
		}

		httpRoute := &gatewayv1.HTTPRoute{}

		It("Should create an HTTPRoute to expose the traffic externally", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the controller has created the HTTPRoute")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should reconcile the HTTPRoute when modified", func() {
			By("By simulating a manual HTTPRoute modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"rules":[{"backendRefs":[{"name":"foo","port":8888}]}]}}`))
			Expect(cli.Patch(ctx, httpRoute, patch)).Should(Succeed())

			By("By checking that the controller has restored the HTTPRoute spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, httpRoute)
				if err != nil {
					return "", err
				}
				if len(httpRoute.Spec.Rules) > 0 && len(httpRoute.Spec.Rules[0].BackendRefs) > 0 {
					return string(httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Name), nil
				}
				return "", nil
			}, duration, interval).Should(Equal(Name))
			Expect(*httpRoute).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should recreate the HTTPRoute when deleted", func() {
			By("By deleting the notebook HTTPRoute")
			Expect(cli.Delete(ctx, httpRoute)).Should(Succeed())

			By("By checking that the controller has recreated the HTTPRoute")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should delete the Openshift Route", func() {
			// Testenv cluster does not implement Kubernetes GC:
			// https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			// To test that the deletion lifecycle works, test the ownership
			// instead of asserting on existence.
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}

			By("By checking that the Notebook owns the Route object")
			Expect(httpRoute.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})

	})

	// New test case for RoleBinding reconciliation
	When("Reconcile RoleBindings is called for a Notebook", func() {
		const (
			name      = "test-notebook-rolebinding"
			namespace = "default"
		)
		notebook := createNotebook(name, namespace)

		// Define the role and role-binding names and types used in the reconciliation
		roleRefName := "ds-pipeline-user-access-dspa"
		roleBindingName := "elyra-pipelines-" + name

		BeforeEach(func() {
			// Skip the tests if SET_PIPELINE_RBAC is not set to "true"
			fmt.Printf("SET_PIPELINE_RBAC is: %s\n", os.Getenv("SET_PIPELINE_RBAC"))
			if os.Getenv("SET_PIPELINE_RBAC") != "true" {
				Skip("Skipping RoleBinding reconciliation tests as SET_PIPELINE_RBAC is not set to 'true'")
			}
		})

		It("Should create a RoleBinding when the referenced Role exists", func() {
			By("Creating a Notebook and ensuring the Role exists")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			// Simulate the Role required by RoleBinding
			role := &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      roleRefName,
					Namespace: namespace,
				},
			}
			Expect(cli.Create(ctx, role)).Should(Succeed())
			defer func() {
				if err := cli.Delete(ctx, role); err != nil {
					GinkgoT().Logf("Failed to delete Role: %v", err)
				}
			}()

			By("Checking that the RoleBinding is created")
			roleBinding := &rbacv1.RoleBinding{}
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())

			Expect(roleBinding.RoleRef.Name).To(Equal(roleRefName))
			Expect(roleBinding.Subjects[0].Name).To(Equal(name))
			Expect(roleBinding.Subjects[0].Kind).To(Equal("ServiceAccount"))
		})

		It("Should delete the RoleBinding when the Notebook is deleted", func() {
			By("Ensuring the RoleBinding exists")
			roleBinding := &rbacv1.RoleBinding{}
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())

			By("Deleting the Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("Ensuring the RoleBinding is deleted")
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: roleBindingName, Namespace: namespace}, roleBinding)
			}, duration, interval).Should(Succeed())
		})

	})

	// New test case for notebook creation
	When("Creating a Notebook, test certificate is mounted", func() {
		const (
			Name      = "test-notebook"
			Namespace = "default"
		)

		It("Should mount a trusted-ca when it exists on the given namespace", func() {
			logger := logr.Discard()

			By("By simulating the existence of odh-trusted-ca-bundle ConfigMap")
			// Create a ConfigMap similar to odh-trusted-ca-bundle for simulation
			workbenchTrustedCACertBundle := "workbench-trusted-ca-bundle"
			trustedCACertBundle := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "odh-trusted-ca-bundle",
					Namespace: "default",
					Labels: map[string]string{
						"config.openshift.io/inject-trusted-cabundle": "true",
					},
				},
				// NOTE: use valid short CA certs and make them each be different
				// $ openssl req -nodes -x509 -newkey ed25519 -days 365 -set_serial 1 -out /dev/stdout -subj "/"
				Data: map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI3MzdaFw0yNTExMTMy\nMzI3MzdaMAAwKjAFBgMrZXADIQDEMMlJ1P0gyxEV7A8PgpNosvKZgE4ttDDpu/w9\n35BHzjAFBgMrZXADQQDHT8ulalOcI6P5lGpoRcwLzpa4S/5pyqtbqw2zuj7dIJPI\ndNb1AkbARd82zc9bF+7yDkCNmLIHSlDORUYgTNEL\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI2NTlaFw0yNTExMTMy\nMzI2NTlaMAAwKjAFBgMrZXADIQB/v02zcoIIcuan/8bd7cvrBuCGTuVZBrYr1RdA\n0k58yzAFBgMrZXADQQBKsL1tkpOZ6NW+zEX3mD7bhmhxtODQHnANMXEXs0aljWrm\nAxDrLdmzsRRYFYxe23OdXhWqPs8SfO8EZWEvXoME\n-----END CERTIFICATE-----",
				},
			}

			serviceCACertBundle := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "openshift-service-ca.crt",
					Namespace: "default",
					// Annotations: map[string]string{
					// 	"service.beta.openshift.io/inject-cabundle": "true",
					// },
				},
				Data: map[string]string{
					"service-ca.crt": "-----BEGIN CERTIFICATE-----\nMIIBATCBtKADAgECAgEBMAUGAytlcDAAMB4XDTI1MDYxNjE2MTg0MVoXDTI2MDYx\nNjE2MTg0MVowADAqMAUGAytlcAMhAP7g8UxhoFPZXQiy4sSbOsLrlXq2RgFTzQOD\nj8O8e9qmo1MwUTAdBgNVHQ4EFgQUTCWpJDtMDVadBlVpkVTiLnCihqMwHwYDVR0j\nBBgwFoAUTCWpJDtMDVadBlVpkVTiLnCihqMwDwYDVR0TAQH/BAUwAwEB/zAFBgMr\nZXADQQDKpiapbn7ub7/hT7Whad9wbvIY8wXrWojgZXXbWaMQFV8i8GW7QN4w/C1p\nB8i0efvecoLP/mqmXNyl7KgTnC4D\n-----END CERTIFICATE-----",
				},
			}

			// Create the ConfigMap
			Expect(cli.Create(ctx, trustedCACertBundle)).Should(Succeed())
			Expect(cli.Create(ctx, serviceCACertBundle)).Should(Succeed())
			defer func() {
				// Clean up the ConfigMap after the test
				if err := cli.Delete(ctx, trustedCACertBundle); err != nil {
					// Log the error without failing the test
					logger.Info("Error occurred during deletion of ConfigMap: %v", err)
				}
				if err := cli.Delete(ctx, serviceCACertBundle); err != nil {
					// Log the error without failing the test
					logger.Info("Error occurred during deletion of ConfigMap: %v", err)
				}
			}()

			By("By creating a new Notebook")
			notebook := createNotebook(Name, Namespace)
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that trusted-ca bundle is mounted")
			// Assert that the volume mount and volume are added correctly
			volumeMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
			expectedVolumeMount := corev1.VolumeMount{
				Name:      "trusted-ca",
				MountPath: volumeMountPath,
				SubPath:   "ca-bundle.crt",
				ReadOnly:  true,
			}
			// Check if the volume mount is present and matches the expected one
			Expect(notebook.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(expectedVolumeMount))

			expectedVolume := corev1.Volume{
				Name: "trusted-ca",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: workbenchTrustedCACertBundle},
						Optional:             ptr.To(true),
						Items: []corev1.KeyToPath{
							{
								Key:  "ca-bundle.crt",
								Path: "ca-bundle.crt",
							},
						},
					},
				},
			}
			// Check if the volume is present and matches the expected one
			Expect(notebook.Spec.Template.Spec.Volumes).To(ContainElement(expectedVolume))

			// Check the content in workbench-trusted-ca-bundle matches what we expect:
			//   - have 3 certificates there in ca-bundle.crt
			//   - all certificates are valid
			// Wait for the controller to create/update the workbench-trusted-ca-bundle
			configMapName := "workbench-trusted-ca-bundle"
			// TODO(RHOAIENG-15907): use eventually to mask product flakiness
			Eventually(func() error {
				return checkCertConfigMapWithError(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 3)
			}, duration, interval).Should(Succeed())
		})

	})

	// New test case for notebook creation with long name
	When("Creating a long named Notebook", func() {

		// With the work done https://issues.redhat.com/browse/RHOAIENG-4148,
		// 48 characters is the maximum length for a notebook name.
		// This would the extent of the test.
		// TODO: Update the test to use the maximum length when the work is done.
		const (
			Name      = "test-notebook-with-a-very-long-name-thats-48char"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		pathPrefix := gatewayv1.PathMatchPathPrefix
		expectedHTTPRoute := gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{
							Group:     func() *gatewayv1.Group { g := gatewayv1.Group("gateway.networking.k8s.io"); return &g }(),
							Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
							Name:      gatewayv1.ObjectName("data-science-gateway"),
							Namespace: func() *gatewayv1.Namespace { ns := gatewayv1.Namespace("openshift-ingress"); return &ns }(),
						},
					},
				},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathPrefix,
									Value: (*string)(&[]string{"/notebook/" + Namespace + "/" + Name}[0]),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: func() *gatewayv1.Group { g := gatewayv1.Group(""); return &g }(),
										Kind:  func() *gatewayv1.Kind { k := gatewayv1.Kind("Service"); return &k }(),
										Name:  gatewayv1.ObjectName(Name),
										Port:  (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8888}[0]),
									},
									Weight: func() *int32 { w := int32(1); return &w }(),
								},
							},
						},
					},
				},
			},
		}

		httpRoute2 := &gatewayv1.HTTPRoute{}

		It("Should create an HTTPRoute to expose the traffic externally", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has created the HTTPRoute")
			Eventually(func() error {
				httpRoute, err := getHTTPRouteFromList(httpRoute2, notebook, Name, Namespace)
				if httpRoute == nil {
					return err
				}
				return nil
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute2).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should reconcile the HTTPRoute when modified", func() {
			By("By simulating a manual HTTPRoute modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"rules":[{"backendRefs":[{"name":"foo","port":8888}]}]}}`))
			Expect(cli.Patch(ctx, httpRoute2, patch)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has restored the HTTPRoute spec")
			Eventually(func() (string, error) {
				httpRoute, err := getHTTPRouteFromList(httpRoute2, notebook, Name, Namespace)
				if httpRoute == nil {
					return "", err
				}
				if len(httpRoute.Spec.Rules) > 0 && len(httpRoute.Spec.Rules[0].BackendRefs) > 0 {
					return string(httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Name), nil
				}
				return "", nil
			}, duration, interval).Should(Equal(Name))
			Expect(*httpRoute2).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should recreate the HTTPRoute when deleted", func() {
			By("By deleting the notebook HTTPRoute")
			Expect(cli.Delete(ctx, httpRoute2)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the HTTPRoute")
			Eventually(func() error {
				httpRoute, err := getHTTPRouteFromList(httpRoute2, notebook, Name, Namespace)
				if httpRoute == nil {
					return err
				}
				return nil
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute2).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should delete the Openshift Route", func() {
			// Testenv cluster does not implement Kubernetes GC:
			// https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			// To test that the deletion lifecycle works, test the ownership
			// instead of asserting on existence.
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}

			By("By checking that the Notebook owns the Route object")
			Expect(httpRoute2.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})

	})

	// New test case for notebook update
	When("Updating a Notebook", func() {
		const (
			Name      = "test-notebook-update"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		It("Should update the Notebook specification", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By updating the Notebook's image")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())

			updatedImage := "registry.redhat.io/ubi9/ubi:updated"
			notebook.Spec.Template.Spec.Containers[0].Image = updatedImage
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook's image is updated")
			Eventually(func() string {
				Expect(cli.Get(ctx, key, notebook)).Should(Succeed())
				return notebook.Spec.Template.Spec.Containers[0].Image
			}, duration, interval).Should(Equal(updatedImage))
		})

		It("When notebook CR is updated, should mount a trusted-ca if it exists on the given namespace", func() {
			logger := logr.Discard()

			By("By simulating the existence of odh-trusted-ca-bundle ConfigMap")
			// Create a ConfigMap similar to odh-trusted-ca-bundle for simulation
			workbenchTrustedCACertBundle := "workbench-trusted-ca-bundle"
			trustedCACertBundle := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "odh-trusted-ca-bundle",
					Namespace: "default",
					Labels: map[string]string{
						"config.openshift.io/inject-trusted-cabundle": "true",
					},
				},
				Data: map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4MjZaFw0yNTExMTMy\nMzI4MjZaMAAwKjAFBgMrZXADIQD77pLvWIX0WmlkYthRZ79oIf7qrGO7yECf668T\nSB42vTAFBgMrZXADQQDs76j81LPh+lgnnf4L0ROUqB66YiBx9SyDTjm83Ya4KC+2\nLEP6Mw1//X2DX89f1chy7RxCpFS3eXb7U/p+GPwA\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4NDJaFw0yNTExMTMy\nMzI4NDJaMAAwKjAFBgMrZXADIQAw01381TUVSxaCvjQckcw3RTcg+bsVMgNZU8eF\nXa/f3jAFBgMrZXADQQBeJZHSiMOYqa/tXUrQTfNIcklHuvieGyBRVSrX3bVUV2uM\nDBkZLsZt65rCk1A8NG+xkA6j3eIMAA9vBKJ0ht8F\n-----END CERTIFICATE-----",
				},
			}
			// Create the ConfigMap
			Expect(cli.Create(ctx, trustedCACertBundle)).Should(Succeed())
			defer func() {
				// Clean up the ConfigMap after the test
				if err := cli.Delete(ctx, trustedCACertBundle); err != nil {
					// Log the error without failing the test
					logger.Info("Error occurred during deletion of ConfigMap: %v", err)
				}
			}()

			By("By updating the Notebook's image")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())

			updatedImage := "registry.redhat.io/ubi9/ubi:updated"
			notebook.Spec.Template.Spec.Containers[0].Image = updatedImage
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("By checking that trusted-ca bundle is mounted")
			// Assert that the volume mount and volume are added correctly
			volumeMountPath := "/etc/pki/tls/custom-certs/ca-bundle.crt"
			expectedVolumeMount := corev1.VolumeMount{
				Name:      "trusted-ca",
				MountPath: volumeMountPath,
				SubPath:   "ca-bundle.crt",
				ReadOnly:  true,
			}
			Expect(notebook.Spec.Template.Spec.Containers[0].VolumeMounts).To(ContainElement(expectedVolumeMount))

			expectedVolume := corev1.Volume{
				Name: "trusted-ca",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: workbenchTrustedCACertBundle},
						Optional:             ptr.To(true),
						Items: []corev1.KeyToPath{
							{
								Key:  "ca-bundle.crt",
								Path: "ca-bundle.crt",
							},
						},
					},
				},
			}
			Expect(notebook.Spec.Template.Spec.Volumes).To(ContainElement(expectedVolume))

			// Check the content in workbench-trusted-ca-bundle matches what we expect:
			//   - have 3 certificates there in ca-bundle.crt
			//   - all certificates are valid
			// Wait for the controller to create/update the workbench-trusted-ca-bundle
			configMapName := "workbench-trusted-ca-bundle"
			// TODO(RHOAIENG-15907): use eventually to mask product flakiness
			Eventually(func() error {
				return checkCertConfigMapWithError(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 3)
			}, duration, interval).Should(Succeed())
		})
	})

	When("Creating a Notebook, test Networkpolicies", func() {
		const (
			Name      = "test-notebook-np"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		npProtocol := corev1.ProtocolTCP
		testPodNamespace := odhNotebookControllerTestNamespace

		expectedNotebookNetworkPolicy := netv1.NetworkPolicy{
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
								// Since for unit tests the controller does not run in a cluster pod,
								// it cannot detect its own pod's namespace. Therefore, we define it
								// to be `redhat-ods-applications` (in suite_test.go)
								NamespaceSelector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"kubernetes.io/metadata.name": testPodNamespace,
									},
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

		expectedNotebookKubeRbacNetworkPolicy := &netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebook.Name + NotebookKubeRbacProxyNetworkPolicySuffix,
				Namespace: notebook.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "kubeflow.org/v1",
						Kind:               "Notebook",
						Name:               notebook.Name,
						UID:                notebook.UID,
						Controller:         &[]bool{true}[0],
						BlockOwnerDeletion: &[]bool{true}[0],
					},
				},
			},
			Spec: netv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"notebook-name": notebook.Name,
					},
				},
				PolicyTypes: []netv1.PolicyType{
					netv1.PolicyTypeIngress,
				},
				Ingress: []netv1.NetworkPolicyIngressRule{
					{
						Ports: []netv1.NetworkPolicyPort{
							{
								Protocol: &npProtocol,
								Port:     &[]intstr.IntOrString{intstr.FromInt(int(NotebookKubeRbacProxyPort))}[0],
							},
						},
					},
				},
			},
		}

		notebookNetworkPolicy := &netv1.NetworkPolicy{}
		notebookOAuthNetworkPolicy := &netv1.NetworkPolicy{}

		It("Should create network policies to restrict undesired traffic", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the controller has created Network policy to allow only controller traffic")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-ctrl-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookNetworkPolicy).To(BeMatchingK8sResource(expectedNotebookNetworkPolicy, CompareNotebookNetworkPolicies))

			By("By checking that the controller has created Network policy to allow all requests on OAuth port")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + NotebookKubeRbacProxyNetworkPolicySuffix, Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(*expectedNotebookKubeRbacNetworkPolicy, CompareNotebookNetworkPolicies))
		})

		It("Should reconcile the Network policies when modified", func() {
			By("By simulating a manual NetworkPolicy modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"policyTypes":["Egress"]}}`))
			Expect(cli.Patch(ctx, notebookNetworkPolicy, patch)).Should(Succeed())

			By("By checking that the controller has restored the network policy spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name + "-ctrl-np", Namespace: Namespace}
				err := cli.Get(ctx, key, notebookNetworkPolicy)
				if err != nil {
					return "", err
				}
				return string(notebookNetworkPolicy.Spec.PolicyTypes[0]), nil
			}, duration, interval).Should(Equal("Ingress"))
			Expect(*notebookNetworkPolicy).To(BeMatchingK8sResource(expectedNotebookNetworkPolicy, CompareNotebookNetworkPolicies))
		})

		It("Should recreate the Network Policy when deleted", func() {
			By("By deleting the notebook OAuth Network Policy")
			Expect(cli.Delete(ctx, notebookOAuthNetworkPolicy)).Should(Succeed())

			By("By checking that the controller has recreated the OAuth Network policy")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + NotebookKubeRbacProxyNetworkPolicySuffix, Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(*expectedNotebookKubeRbacNetworkPolicy, CompareNotebookNetworkPolicies))
		})

		It("Should delete the Network Policies", func() {
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}

			By("By checking that the Notebook owns the Notebook Network Policy object")
			Expect(notebookNetworkPolicy.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Notebook OAuth Network Policy object")
			Expect(notebookOAuthNetworkPolicy.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})

	})

	When("Creating a Notebook with kube-rbac-proxy injection", func() {
		const (
			Name      = "test-notebook-kube-rbac-proxy"
			Namespace = "default"
		)

		notebook := createNotebookWithKubeRbacProxy(Name, Namespace)

		expectedService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name + KubeRbacProxyServiceSuffix,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				Annotations: map[string]string{
					"service.beta.openshift.io/serving-cert-secret-name": Name + KubeRbacProxyTLSCertVolumeSecretSuffix,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "kubeflow.org/v1",
						Kind:               "Notebook",
						Name:               Name,
						UID:                notebook.UID,
						Controller:         &[]bool{true}[0],
						BlockOwnerDeletion: &[]bool{true}[0],
					},
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{
					Name:       "kube-rbac-proxy",
					Port:       8443,
					TargetPort: intstr.FromString("kube-rbac-proxy"),
					Protocol:   corev1.ProtocolTCP,
				}},
				Selector: map[string]string{
					"statefulset": Name,
				},
			},
		}

		pathPrefix := gatewayv1.PathMatchPathPrefix
		expectedHTTPRoute := &gatewayv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "kubeflow.org/v1",
						Kind:               "Notebook",
						Name:               Name,
						UID:                notebook.UID,
						Controller:         &[]bool{true}[0],
						BlockOwnerDeletion: &[]bool{true}[0],
					},
				},
			},
			Spec: gatewayv1.HTTPRouteSpec{
				CommonRouteSpec: gatewayv1.CommonRouteSpec{
					ParentRefs: []gatewayv1.ParentReference{
						{
							Group:     func() *gatewayv1.Group { g := gatewayv1.Group("gateway.networking.k8s.io"); return &g }(),
							Kind:      func() *gatewayv1.Kind { k := gatewayv1.Kind("Gateway"); return &k }(),
							Name:      gatewayv1.ObjectName("data-science-gateway"),
							Namespace: func() *gatewayv1.Namespace { ns := gatewayv1.Namespace("openshift-ingress"); return &ns }(),
						},
					},
				},
				Rules: []gatewayv1.HTTPRouteRule{
					{
						Matches: []gatewayv1.HTTPRouteMatch{
							{
								Path: &gatewayv1.HTTPPathMatch{
									Type:  &pathPrefix,
									Value: (*string)(&[]string{"/notebook/" + Namespace + "/" + Name}[0]),
								},
							},
						},
						BackendRefs: []gatewayv1.HTTPBackendRef{
							{
								BackendRef: gatewayv1.BackendRef{
									BackendObjectReference: gatewayv1.BackendObjectReference{
										Group: func() *gatewayv1.Group { g := gatewayv1.Group(""); return &g }(),
										Kind:  func() *gatewayv1.Kind { k := gatewayv1.Kind("Service"); return &k }(),
										Name:  gatewayv1.ObjectName(Name + KubeRbacProxyServiceSuffix),
										Port:  (*gatewayv1.PortNumber)(&[]gatewayv1.PortNumber{8443}[0]),
									},
									Weight: func() *int32 { w := int32(1); return &w }(),
								},
							},
						},
					},
				},
			},
		}

		expectedConfigMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name + KubeRbacProxyConfigSuffix,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion:         "kubeflow.org/v1",
						Kind:               "Notebook",
						Name:               Name,
						UID:                notebook.UID,
						Controller:         &[]bool{true}[0],
						BlockOwnerDeletion: &[]bool{true}[0],
					},
				},
			},
			Data: map[string]string{
				"config-file.yaml": fmt.Sprintf(`authorization:
  resourceAttributes:
    verb: get
    resource: notebooks
    apiGroup: kubeflow.org
    resourceName: %s
    namespace: %s`, Name, Namespace),
			},
		}

		It("Should create a Notebook with inject-auth annotation to contain kube-rbac-proxy sidecar", func() {
			By("By creating a new Notebook with inject-auth annotation")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the notebook was created successfully")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())

			// Verify the notebook has the inject-auth annotation
			Expect(notebook.Annotations[AnnotationInjectAuth]).To(Equal("true"))
		})

		It("Should inject the kube-rbac-proxy as a sidecar container", func() {
			By("By checking that the webhook has injected the sidecar container")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Eventually(func() error {
				err := cli.Get(ctx, key, notebook)
				if err != nil {
					return err
				}

				// Verify we have exactly 2 containers (original + kube-rbac-proxy)
				if len(notebook.Spec.Template.Spec.Containers) != 2 {
					return fmt.Errorf("expected 2 containers, got %d", len(notebook.Spec.Template.Spec.Containers))
				}

				// Verify the second container is the kube-rbac-proxy
				kubeRbacProxyContainer := notebook.Spec.Template.Spec.Containers[1]
				if kubeRbacProxyContainer.Name != "kube-rbac-proxy" {
					return fmt.Errorf("expected kube-rbac-proxy container name 'kube-rbac-proxy', got '%s'", kubeRbacProxyContainer.Name)
				}

				// Verify kube-rbac-proxy container has the correct image
				expectedImagePrefix := kubeRbacProxyImage
				if !strings.HasPrefix(kubeRbacProxyContainer.Image, expectedImagePrefix) {
					return fmt.Errorf("expected kube-rbac-proxy image to start with '%s', got '%s'", expectedImagePrefix, kubeRbacProxyContainer.Image)
				}

				// Verify kube-rbac-proxy container has the correct port
				foundPort := false
				for _, port := range kubeRbacProxyContainer.Ports {
					if port.Name == "kube-rbac-proxy" && port.ContainerPort == 8443 {
						foundPort = true
						break
					}
				}
				if !foundPort {
					return fmt.Errorf("kube-rbac-proxy container missing port 'kube-rbac-proxy' on 8443")
				}

				return nil
			}, duration, interval).Should(Succeed())
		})

		It("Should create a Service for the kube-rbac-proxy", func() {
			By("By checking that the controller has created the Service")
			service := &corev1.Service{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + KubeRbacProxyServiceSuffix, Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(*service).To(BeMatchingK8sResource(*expectedService, CompareNotebookServices))
		})

		It("Should create an HTTPRoute for the kube-rbac-proxy", func() {
			By("By checking that the controller has created the HTTPRoute")
			httpRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute).To(BeMatchingK8sResource(*expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should reconcile the HTTPRoute when modified", func() {
			By("By simulating a manual HTTPRoute modification")
			httpRoute := &gatewayv1.HTTPRoute{}
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, httpRoute)).Should(Succeed())

			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"rules":[{"backendRefs":[{"name":"foo","port":8888}]}]}}`))
			Expect(cli.Patch(ctx, httpRoute, patch)).Should(Succeed())

			By("By checking that the controller has restored the HTTPRoute spec")
			Eventually(func() (string, error) {
				err := cli.Get(ctx, key, httpRoute)
				if err != nil {
					return "", err
				}
				if len(httpRoute.Spec.Rules) > 0 && len(httpRoute.Spec.Rules[0].BackendRefs) > 0 {
					return string(httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Name), nil
				}
				return "", nil
			}, duration, interval).Should(Equal(Name + KubeRbacProxyServiceSuffix))
			Expect(*httpRoute).To(BeMatchingK8sResource(*expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should recreate the HTTPRoute when deleted", func() {
			By("By deleting the notebook HTTPRoute")
			httpRoute := &gatewayv1.HTTPRoute{}
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, httpRoute)).Should(Succeed())
			Expect(cli.Delete(ctx, httpRoute)).Should(Succeed())

			By("By checking that the controller has recreated the HTTPRoute")
			Eventually(func() error {
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute).To(BeMatchingK8sResource(*expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should remove the reconciliation lock annotation", func() {
			By("By checking that the reconciliation lock annotation is removed")
			Eventually(func() (map[string]string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, notebook)
				if err != nil {
					return nil, err
				}
				return notebook.Annotations, nil
			}, duration, interval).Should(Not(HaveKey(culler.STOP_ANNOTATION)))
		})

		It("Should reconcile the Notebook when modified", func() {
			By("By simulating a manual Notebook modification")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())

			// Store original values
			originalServiceAccount := notebook.Spec.Template.Spec.ServiceAccountName
			var originalContainerImage string
			var originalVolumeSource corev1.VolumeSource

			// Store original container image if there's a second container (kube-rbac-proxy)
			if len(notebook.Spec.Template.Spec.Containers) > 1 {
				originalContainerImage = notebook.Spec.Template.Spec.Containers[1].Image
			}

			// Store original volume source if there's a second volume
			if len(notebook.Spec.Template.Spec.Volumes) > 1 {
				originalVolumeSource = notebook.Spec.Template.Spec.Volumes[1].VolumeSource
			}

			// Make manual modifications
			notebook.Spec.Template.Spec.ServiceAccountName = "foo"
			if len(notebook.Spec.Template.Spec.Containers) > 1 {
				notebook.Spec.Template.Spec.Containers[1].Image = "bar"
			}
			if len(notebook.Spec.Template.Spec.Volumes) > 1 {
				notebook.Spec.Template.Spec.Volumes[1].VolumeSource = corev1.VolumeSource{}
			}
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("By checking that the controller has restored the Notebook spec")
			Eventually(func() error {
				err := cli.Get(ctx, key, notebook)
				if err != nil {
					return err
				}

				// Check ServiceAccount restoration
				if notebook.Spec.Template.Spec.ServiceAccountName != originalServiceAccount {
					return fmt.Errorf("ServiceAccount not restored: expected %s, got %s",
						originalServiceAccount, notebook.Spec.Template.Spec.ServiceAccountName)
				}

				// Check container image restoration if applicable
				if len(notebook.Spec.Template.Spec.Containers) > 1 && originalContainerImage != "" {
					if notebook.Spec.Template.Spec.Containers[1].Image != originalContainerImage {
						return fmt.Errorf("Container image not restored: expected %s, got %s",
							originalContainerImage, notebook.Spec.Template.Spec.Containers[1].Image)
					}
				}

				// Check volume source restoration if applicable
				if len(notebook.Spec.Template.Spec.Volumes) > 1 {
					if !reflect.DeepEqual(notebook.Spec.Template.Spec.Volumes[1].VolumeSource, originalVolumeSource) {
						return fmt.Errorf("Volume source not restored")
					}
				}

				return nil
			}, duration, interval).Should(Succeed())
		})

		It("Should create a ConfigMap for kube-rbac-proxy configuration", func() {
			By("By checking that the controller has created the ConfigMap")
			configMap := &corev1.ConfigMap{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + KubeRbacProxyConfigSuffix, Namespace: Namespace}
				return cli.Get(ctx, key, configMap)
			}, duration, interval).Should(Succeed())
			Expect(*configMap).To(BeMatchingK8sResource(*expectedConfigMap, CompareNotebookConfigMaps))
		})

		It("Should recreate the ConfigMap when deleted", func() {
			By("By deleting the notebook ConfigMap")
			configMap := &corev1.ConfigMap{}
			key := types.NamespacedName{Name: Name + KubeRbacProxyConfigSuffix, Namespace: Namespace}
			Expect(cli.Get(ctx, key, configMap)).Should(Succeed())
			Expect(cli.Delete(ctx, configMap)).Should(Succeed())

			By("By checking that the controller has recreated the ConfigMap")
			Eventually(func() error {
				return cli.Get(ctx, key, configMap)
			}, duration, interval).Should(Succeed())
			Expect(*configMap).To(BeMatchingK8sResource(*expectedConfigMap, CompareNotebookConfigMaps))
		})

		It("Should create a Service Account for the notebook", func() {
			By("By checking that the controller has created the Service Account")
			serviceAccount := &corev1.ServiceAccount{}
			expectedServiceAccount := createExpectedServiceAccount(Name, Namespace)
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(*serviceAccount).To(BeMatchingK8sResource(expectedServiceAccount, CompareNotebookServiceAccounts))
		})

		It("Should recreate the Service Account when deleted", func() {
			By("By deleting the notebook Service Account")
			serviceAccount := &corev1.ServiceAccount{}
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, serviceAccount)).Should(Succeed())
			Expect(cli.Delete(ctx, serviceAccount)).Should(Succeed())

			By("By checking that the controller has recreated the Service Account")
			expectedServiceAccount := createExpectedServiceAccount(Name, Namespace)
			Eventually(func() error {
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(*serviceAccount).To(BeMatchingK8sResource(expectedServiceAccount, CompareNotebookServiceAccounts))
		})

		It("Should delete all kube-rbac-proxy related resources when the notebook is deleted", func() {
			// Testenv cluster does not implement Kubernetes GC:
			// https://book.kubebuilder.io/reference/envtest.html#testing-considerations
			// To test that the deletion lifecycle works, test the ownership
			// instead of asserting on existence.
			expectedOwnerReference := metav1.OwnerReference{
				APIVersion:         "kubeflow.org/v1",
				Kind:               "Notebook",
				Name:               Name,
				UID:                notebook.GetObjectMeta().GetUID(),
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}

			By("By checking that the Notebook owns the kube-rbac-proxy Service object")
			kubeRbacProxyService := &corev1.Service{}
			serviceKey := types.NamespacedName{Name: Name + KubeRbacProxyServiceSuffix, Namespace: Namespace}
			Expect(cli.Get(ctx, serviceKey, kubeRbacProxyService)).Should(Succeed())
			Expect(kubeRbacProxyService.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the kube-rbac-proxy HTTPRoute object")
			kubeRbacProxyHTTPRoute := &gatewayv1.HTTPRoute{}
			routeKey := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, routeKey, kubeRbacProxyHTTPRoute)).Should(Succeed())
			Expect(kubeRbacProxyHTTPRoute.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the kube-rbac-proxy ConfigMap object")
			kubeRbacProxyConfigMap := &corev1.ConfigMap{}
			configMapKey := types.NamespacedName{Name: Name + KubeRbacProxyConfigSuffix, Namespace: Namespace}
			Expect(cli.Get(ctx, configMapKey, kubeRbacProxyConfigMap)).Should(Succeed())
			Expect(kubeRbacProxyConfigMap.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})
	})

	When("Testing HTTPRoute mode switching", func() {
		const (
			Name      = "test-notebook-mode-switch"
			Namespace = "default"
		)

		It("Should clean up unauthenticated HTTPRoutes when enabling kube-rbac-proxy", func() {
			By("Creating a notebook without kube-rbac-proxy initially")
			notebook := createNotebook(Name, Namespace)
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("Verifying an unauthenticated HTTPRoute is created")
			unauthenticatedRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, unauthenticatedRoute)
			}, duration, interval).Should(Succeed())

			// Verify it points to the regular service (port 8888)
			Expect(len(unauthenticatedRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(unauthenticatedRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(Name))
			Expect(*unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8888)))

			By("Updating the notebook to add kube-rbac-proxy sidecar container")
			key := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())
			if notebook.Annotations == nil {
				notebook.Annotations = make(map[string]string)
			}
			notebook.Annotations[AnnotationInjectAuth] = "true"
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("Verifying the unauthenticated HTTPRoute is cleaned up")
			Eventually(func() error {
				err := cli.Get(ctx, key, unauthenticatedRoute)
				if err != nil {
					return err
				}
				// Check if it still points to regular service (should be gone or changed)
				if len(unauthenticatedRoute.Spec.Rules) > 0 && len(unauthenticatedRoute.Spec.Rules[0].BackendRefs) > 0 {
					backendName := string(unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Name)
					backendPort := unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Port
					if backendName == Name && backendPort != nil && *backendPort == 8888 {
						return fmt.Errorf("unauthenticated HTTPRoute still exists")
					}
				}
				return nil
			}, duration, interval).Should(Succeed())

			By("Verifying an kube-rbac-proxy HTTPRoute is created")
			kubeRbacProxyRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				return cli.Get(ctx, key, kubeRbacProxyRoute)
			}, duration, interval).Should(Succeed())

			// Verify it points to the kube-rbac-proxy service (port 8443)
			Expect(len(kubeRbacProxyRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(Name + KubeRbacProxyServiceSuffix))
			Expect(*kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8443)))

			By("Cleaning up the test notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())
		})

		It("Should clean up kube-rbac-proxy HTTPRoutes when disabling kube-rbac-proxy authentication", func() {
			By("Creating a notebook with kube-rbac-proxy initially")
			notebook := createNotebookWithKubeRbacProxy(Name+"-rbac-to-regular", Namespace)
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("Verifying an kube-rbac-proxy HTTPRoute is created")
			kubeRbacProxyRoute := &gatewayv1.HTTPRoute{}
			key := types.NamespacedName{Name: Name + "-rbac-to-regular", Namespace: Namespace}
			Eventually(func() error {
				return cli.Get(ctx, key, kubeRbacProxyRoute)
			}, duration, interval).Should(Succeed())

			// Verify it points to the kube-rbac-proxy service (port 8443)
			Expect(len(kubeRbacProxyRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(Name + "-rbac-to-regular" + KubeRbacProxyServiceSuffix))
			Expect(*kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8443)))

			By("Updating the notebook to disable kube-rbac-proxy authentication")
			Expect(cli.Get(ctx, key, notebook)).Should(Succeed())
			delete(notebook.Annotations, AnnotationInjectAuth)
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("Verifying the kube-rbac-proxy HTTPRoute is cleaned up")
			Eventually(func() error {
				err := cli.Get(ctx, key, kubeRbacProxyRoute)
				if err != nil {
					return err
				}
				// Check if it still points to kube-rbac-proxy service (should be gone or changed)
				if len(kubeRbacProxyRoute.Spec.Rules) > 0 && len(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs) > 0 {
					backendName := string(kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Name)
					backendPort := kubeRbacProxyRoute.Spec.Rules[0].BackendRefs[0].Port
					if backendName == Name+"-rbac-to-regular"+KubeRbacProxyServiceSuffix && backendPort != nil && *backendPort == 8443 {
						return fmt.Errorf("kube-rbac-proxy HTTPRoute still exists")
					}
				}
				return nil
			}, duration, interval).Should(Succeed())

			By("Verifying an unauthenticated HTTPRoute is created")
			unauthenticatedRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				return cli.Get(ctx, key, unauthenticatedRoute)
			}, duration, interval).Should(Succeed())

			// Verify it points to the regular service (port 8888)
			Expect(len(unauthenticatedRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(unauthenticatedRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Name)).To(Equal(Name + "-rbac-to-regular"))
			Expect(*unauthenticatedRoute.Spec.Rules[0].BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8888)))

			By("Cleaning up the test notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())
		})
	})

	When("Creating a Notebook without kube-rbac-proxy proxy injection", func() {
		const (
			Name      = "test-notebook-no-kube-rbac-proxy"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		It("Should create a Notebook without kube-rbac-proxy proxy", func() {
			By("By creating a new Notebook without inject-auth annotation")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that no kube-rbac-proxy proxy container was injected")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())

			// Verify only the original container exists
			Expect(notebook.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(notebook.Spec.Template.Spec.Containers[0].Name).To(Equal(Name))

			// Verify no kube-rbac-proxy volumes were added
			volumeNames := make(map[string]bool)
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				volumeNames[volume.Name] = true
			}
			Expect(volumeNames[KubeRbacProxyConfigVolumeName]).To(BeFalse())
			Expect(volumeNames[KubeRbacProxyTLSCertsVolumeName]).To(BeFalse())
		})

		It("Should create an unauthenticated HTTPRoute", func() {
			By("By checking that the controller has created an unauthenticated HTTPRoute")
			httpRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())

			// Verify it's an unauthenticated HTTPRoute (points to the notebook service, not kube-rbac-proxy service)
			Expect(len(httpRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(httpRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Name)).To(Equal(Name))
			Expect(*httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Port).To(Equal(gatewayv1.PortNumber(8888)))
		})
	})

	When("Checking ds-pipeline-config secret lifecycle", func() {
		const (
			notebookName = "dspa-notebook"
			dsSecretName = "ds-pipeline-config"
			Namespace    = "dspa-test-namespace"
			accessKeyKey = "accesskey"
			secretKeyKey = "secretkey"
			secretName   = "cos-secret"
			gatewayName  = "data-science-gateway"
		)

		testNamespaces = append(testNamespaces, Namespace)

		var (
			dspaObj      *dspav1.DataSciencePipelinesApplication
			s3CredSecret *corev1.Secret
			gateway      *unstructured.Unstructured
		)
		BeforeEach(func() {
			//Pass env to be visible within test suite
			_ = os.Setenv("SET_PIPELINE_SECRET", "true")
			fmt.Printf("SET_PIPELINE_SECRET is: %s\n", os.Getenv("SET_PIPELINE_SECRET"))
			if os.Getenv("SET_PIPELINE_SECRET") != "true" {
				Skip("Skipping elyra secret creation reconciliation tests as SET_PIPELINE_SECRET is not set to 'true'")
			}

		})

		It("should create a ds-pipeline-config secret if a DSPA is present", func() {
			// Create all the necessary objects within dspa-test-namespace namespace: DSPA CR, Gateway CR, and COS Secret.
			// These components are required for the 'ds-pipeline-config' secret to be generated.
			By("Creating a DSPA object")
			dspaObj = &dspav1.DataSciencePipelinesApplication{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dspa",
					Namespace: Namespace,
				},
				Spec: dspav1.DSPASpec{
					ObjectStorage: &dspav1.ObjectStorage{
						ExternalStorage: &dspav1.ExternalStorage{
							Host:   "minio.default.svc.cluster.local",
							Bucket: "pipelines",
							S3CredentialSecret: &dspav1.S3CredentialSecret{
								SecretName: secretName,
								AccessKey:  accessKeyKey,
								SecretKey:  secretKeyKey,
							},
						},
					},
				},
			}
			Expect(cli.Create(ctx, dspaObj)).To(Succeed())
			By("Setting DSPA Status with a API server URL")
			dspaObj.Status = dspav1.DSPAStatus{
				Components: dspav1.ComponentStatus{
					APIServer: dspav1.ComponentDetailStatus{
						ExternalUrl: "https://pipeline-api.example.com",
					},
				},
			}
			// Update only the status subresource
			Expect(cli.Status().Update(ctx, dspaObj)).To(Succeed())

			By("Creating a COS3 credentials Secret")
			s3CredSecret = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      secretName,
					Namespace: Namespace,
				},
				Data: map[string][]byte{
					accessKeyKey: []byte("testaccesskey"),
					secretKeyKey: []byte("testsecretkey"),
				},
			}
			Expect(cli.Create(ctx, s3CredSecret)).To(Succeed())

			By("Creating openshift-ingress namespace for Gateway")
			openshiftIngressNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "openshift-ingress",
				},
			}
			Expect(cli.Create(ctx, openshiftIngressNS)).To(Succeed())

			By("Creating a Gateway CR")
			gateway = &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "gateway.networking.k8s.io/v1",
					"kind":       "Gateway",
					"metadata": map[string]interface{}{
						"name":      gatewayName,
						"namespace": "openshift-ingress",
					},
					"spec": map[string]interface{}{
						"gatewayClassName": "openshift-default",
						"listeners": []interface{}{
							map[string]interface{}{
								"hostname": "gateway.example.com",
								"name":     "https",
								"port":     int64(443),
								"protocol": "HTTPS",
							},
						},
					},
				},
			}
			gateway.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "gateway.networking.k8s.io",
				Version: "v1",
				Kind:    "Gateway",
			})
			Expect(cli.Create(ctx, gateway)).To(Succeed())

			By("Waiting for the Gateway object to be created")
			time.Sleep(10 * time.Millisecond)

			By("Creating Notebook")
			notebook := createNotebook(notebookName, Namespace)
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			By("Waiting for ds-pipeline-config Secret to be created")
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: dsSecretName, Namespace: Namespace}, &corev1.Secret{})
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed())

			// ----------------------------------------------------------------
			// Test workaround for the RHOAIENG-24545 - we need to manually modify
			// the workbench so that the expected resource is mounted properly
			// kubeflow-resource-stopped: '2025-06-25T13:53:46Z'
			By("Running the workaround for RHOAIENG-24545")
			notebook.Spec.Template.Spec.ServiceAccountName = "foo"
			Expect(cli.Update(ctx, notebook)).Should(Succeed())
			// end of workaround
			// ----------------------------------------------------------------

			By("Waiting and validating for volumeMount 'elyra-dsp-details' to be injected into the Notebook")
			Eventually(func() bool {
				var refreshed nbv1.Notebook
				if err := cli.Get(ctx, types.NamespacedName{
					Name:      notebookName,
					Namespace: Namespace,
				}, &refreshed); err != nil {
					return false
				}
				for _, c := range refreshed.Spec.Template.Spec.Containers {
					for _, vm := range c.VolumeMounts {
						if vm.Name == "elyra-dsp-details" && vm.MountPath == "/opt/app-root/runtimes" {
							return true
						}
					}
				}
				return false
			}, 15*time.Second, 500*time.Millisecond).Should(
				BeTrue(), "Expected elyra-dsp-details volumeMount to be present",
			)

			By("Check notebook status")
			notebook = &nbv1.Notebook{}
			err := cli.Get(ctx, client.ObjectKey{Name: notebookName, Namespace: Namespace}, notebook)
			Expect(err).ToNot(HaveOccurred())

			By("Validating the content of the ds-pipeline-config Secret")
			var fetchedSecret corev1.Secret
			err = cli.Get(ctx, types.NamespacedName{
				Name:      dsSecretName,
				Namespace: Namespace,
			}, &fetchedSecret)
			Expect(err).NotTo(HaveOccurred(), "Expected ds-pipeline-config Secret to exist")
			Expect(fetchedSecret.Data).To(HaveKey("odh_dsp.json"))
			Expect(fetchedSecret.Data["odh_dsp.json"]).ToNot(BeEmpty())
			Expect(fetchedSecret.OwnerReferences).ToNot(BeEmpty(), "ds-pipeline-config Secret should have ownerReference")

			By("Modifying DSPA Bucket field to trigger update")
			Expect(cli.Get(ctx, client.ObjectKeyFromObject(dspaObj), dspaObj)).To(Succeed())
			dspaObj.Spec.ExternalStorage.Bucket = "changed-bucket"
			Expect(cli.Update(ctx, dspaObj)).To(Succeed())

			By("Checking if ds-pipeline-config Secret is updated")
			Eventually(func(g Gomega) {
				var updatedSecret corev1.Secret
				g.Expect(cli.Get(ctx, types.NamespacedName{
					Name:      dsSecretName,
					Namespace: Namespace,
				}, &updatedSecret)).To(Succeed())
				// The secret content should be updated to reflect the bucket change
				g.Expect(string(updatedSecret.Data["odh_dsp.json"])).To(ContainSubstring("changed-bucket"))
			}, 30*time.Second, 2*time.Second).Should(Succeed())

			By("Deleting the DSPA and Notebook")
			Expect(cli.Delete(ctx, notebook)).To(Succeed())
			Expect(cli.Delete(ctx, dspaObj)).To(Succeed())

			By("Ensuring the DSPA and secret is fully deleted")
			Eventually(func() error {
				var deletedDspa dspav1.DataSciencePipelinesApplication
				return cli.Get(ctx, types.NamespacedName{
					Name:      "dspa",
					Namespace: Namespace,
				}, &deletedDspa)
			}, time.Second*10, time.Millisecond*250).ShouldNot(Succeed())
			err = cli.Delete(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dsSecretName,
					Namespace: Namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Ensuring ds-pipeline-config Secret is garbage collected")
			Eventually(func() error {
				var fetched corev1.Secret
				return cli.Get(ctx, types.NamespacedName{
					Name:      dsSecretName,
					Namespace: Namespace,
				}, &fetched)
			}, time.Second*10, time.Millisecond*250).ShouldNot(Succeed())

		})

		AfterEach(func() {
			By("Cleaning up all created resources")

			// Delete DSPA
			if dspaObj != nil {
				_ = cli.Delete(ctx, dspaObj)
				Eventually(func() error {
					return cli.Get(ctx, client.ObjectKeyFromObject(dspaObj), dspaObj)
				}, time.Second*5, time.Millisecond*250).ShouldNot(Succeed())
			}
			// Simulate garbage collection of owned resources
			// (only needed in tests due to lack of real GC)
			eventuallyDeleted := &corev1.Secret{}
			Eventually(func() bool {
				err := cli.Get(ctx, types.NamespacedName{
					Name:      dsSecretName,
					Namespace: Namespace,
				}, eventuallyDeleted)
				return apierrors.IsNotFound(err)
			}, time.Second*5, time.Millisecond*500).Should(BeTrue())

			// Delete S3 credentials secret
			if s3CredSecret != nil {
				_ = cli.Delete(ctx, s3CredSecret)
			}

			// Delete Gateway
			if gateway != nil {
				_ = cli.Delete(ctx, gateway)
				Eventually(func() error {
					return cli.Get(ctx, client.ObjectKeyFromObject(gateway), gateway)
				}, time.Second*5, time.Millisecond*250).ShouldNot(Succeed())
			}

			// Delete openshift-ingress namespace
			openshiftIngressNS := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "openshift-ingress",
				},
			}
			_ = cli.Delete(ctx, openshiftIngressNS)

			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: Namespace,
				},
			}
			_ = cli.Delete(ctx, ns)
		})

	})

})

func createNotebook(name, namespace string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  name,
					Image: "registry.redhat.io/ubi9/ubi:latest",
				}}}},
		},
	}
}

func createNotebookWithKubeRbacProxy(name, namespace string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				AnnotationInjectAuth: "true",
			},
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  name,
					Image: "registry.redhat.io/ubi9/ubi:latest",
				}}}},
		},
	}
}

// createExpectedServiceAccount creates the expected ServiceAccount for notebooks
func createExpectedServiceAccount(name, namespace string) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"notebook-name": name,
			},
		},
	}
}

// CompareNotebookServiceAccounts compares two ServiceAccount objects for testing
func CompareNotebookServiceAccounts(sa1 corev1.ServiceAccount, sa2 corev1.ServiceAccount) bool {
	// Compare basic metadata
	if sa1.Name != sa2.Name || sa1.Namespace != sa2.Namespace {
		return false
	}

	// Compare labels
	if !reflect.DeepEqual(sa1.Labels, sa2.Labels) {
		return false
	}

	return true
}

// CompareNotebookServices compares two Service objects for testing
func CompareNotebookServices(s1 corev1.Service, s2 corev1.Service) bool {
	// Compare basic metadata
	if s1.Name != s2.Name || s1.Namespace != s2.Namespace {
		return false
	}

	// Compare labels
	if !reflect.DeepEqual(s1.Labels, s2.Labels) {
		return false
	}

	// Compare spec
	if !reflect.DeepEqual(s1.Spec.Ports, s2.Spec.Ports) {
		return false
	}

	if !reflect.DeepEqual(s1.Spec.Selector, s2.Spec.Selector) {
		return false
	}

	return true
}

// CompareNotebookConfigMaps compares two ConfigMap objects for testing
func CompareNotebookConfigMaps(cm1 corev1.ConfigMap, cm2 corev1.ConfigMap) bool {
	// Compare basic metadata
	if cm1.Name != cm2.Name || cm1.Namespace != cm2.Namespace {
		return false
	}

	// Compare labels
	if !reflect.DeepEqual(cm1.Labels, cm2.Labels) {
		return false
	}

	// Compare data
	if !reflect.DeepEqual(cm1.Data, cm2.Data) {
		return false
	}

	return true
}

func getHTTPRouteFromList(httpRoute *gatewayv1.HTTPRoute, notebook *nbv1.Notebook, name, namespace string) (*gatewayv1.HTTPRoute, error) {
	httpRouteList := &gatewayv1.HTTPRouteList{}
	opts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{"notebook-name": name},
	}

	err := cli.List(ctx, httpRouteList, opts...)
	if err != nil {
		return nil, err
	}

	// Get the HTTPRoute from the list
	for _, nHTTPRoute := range httpRouteList.Items {
		if metav1.IsControlledBy(&nHTTPRoute, notebook) {
			*httpRoute = nHTTPRoute
			return httpRoute, nil
		}
	}
	return nil, errors.New("HTTPRoute not found")
}

// checkCertConfigMapWithError checks the content of a config map defined by the name and namespace
// It tries to parse the given certFileName and checks that all certificates can be parsed there and that the number of the certificates matches what we expect.
// Returns an error instead of using Expect() for use with Eventually()
func checkCertConfigMapWithError(ctx context.Context, namespace string, configMapName string, certFileName string, expNumberCerts int) error {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: configMapName}
	if err := cli.Get(ctx, key, configMap); err != nil {
		return fmt.Errorf("failed to get ConfigMap %s/%s: %v", namespace, configMapName, err)
	}

	certData, exists := configMap.Data[certFileName]
	if !exists {
		return fmt.Errorf("certificate file %s not found in ConfigMap %s/%s", certFileName, namespace, configMapName)
	}

	// Attempt to decode PEM encoded certificates so we are sure all are readable as expected
	certDataByte := []byte(certData)
	certificatesFound := 0
	for len(certDataByte) > 0 {
		block, remainder := pem.Decode(certDataByte)
		certDataByte = remainder

		if block == nil {
			break
		}

		if block.Type == "CERTIFICATE" {
			// Attempt to parse the certificate
			if _, err := x509.ParseCertificate(block.Bytes); err != nil {
				return fmt.Errorf("failed to parse certificate %d: %v", certificatesFound+1, err)
			}
			certificatesFound++
		}
	}

	if certificatesFound != expNumberCerts {
		return fmt.Errorf("expected %d certificates, found %d. Certificate data:\n%s", expNumberCerts, certificatesFound, certData)
	}

	return nil
}
