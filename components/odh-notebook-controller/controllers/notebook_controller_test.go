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
	"reflect"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
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

		expectedNotebookOAuthNetworkPolicy := &netv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      notebook.Name + "-rbac-np",
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
								Port:     &[]intstr.IntOrString{intstr.FromInt(int(NotebookRbacPort))}[0],
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
				key := types.NamespacedName{Name: Name + "-rbac-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(*expectedNotebookOAuthNetworkPolicy, CompareNotebookNetworkPolicies))
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
				key := types.NamespacedName{Name: Name + "-rbac-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(*expectedNotebookOAuthNetworkPolicy, CompareNotebookNetworkPolicies))
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

	When("Creating a Notebook with RBAC proxy injection", func() {
		const (
			Name      = "test-notebook-rbac"
			Namespace = "default"
		)

		notebook := createNotebookWithRBAC(Name, Namespace)

		expectedService := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name + "-rbac",
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				Annotations: map[string]string{
					"service.beta.openshift.io/serving-cert-secret-name": Name + "-rbac-tls",
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
										Name:  gatewayv1.ObjectName(Name + "-rbac"),
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
				Name:      Name + "-rbac-config",
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

		It("Should create a Notebook with RBAC annotation", func() {
			By("By creating a new Notebook with RBAC annotation")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the notebook was created successfully")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())

			// Verify the notebook has the RBAC annotation
			Expect(notebook.Annotations[AnnotationInjectRbac]).To(Equal("true"))
		})

		It("Should create a Service for the RBAC proxy", func() {
			By("By checking that the controller has created the Service")
			service := &corev1.Service{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-rbac", Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(*service).To(BeMatchingK8sResource(*expectedService, CompareNotebookServices))
		})

		It("Should create an HTTPRoute for the RBAC proxy", func() {
			By("By checking that the controller has created the HTTPRoute")
			httpRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())
			Expect(*httpRoute).To(BeMatchingK8sResource(*expectedHTTPRoute, CompareNotebookHTTPRoutes))
		})

		It("Should create a ConfigMap for RBAC configuration", func() {
			By("By checking that the controller has created the ConfigMap")
			configMap := &corev1.ConfigMap{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-rbac-config", Namespace: Namespace}
				return cli.Get(ctx, key, configMap)
			}, duration, interval).Should(Succeed())
			Expect(*configMap).To(BeMatchingK8sResource(*expectedConfigMap, CompareNotebookConfigMaps))
		})

		It("Should delete all RBAC resources when the notebook is deleted", func() {
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

			By("By checking that the Notebook owns the RBAC Service object")
			rbacService := &corev1.Service{}
			serviceKey := types.NamespacedName{Name: Name + "-rbac", Namespace: Namespace}
			Expect(cli.Get(ctx, serviceKey, rbacService)).Should(Succeed())
			Expect(rbacService.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the RBAC HTTPRoute object")
			rbacHTTPRoute := &gatewayv1.HTTPRoute{}
			routeKey := types.NamespacedName{Name: Name, Namespace: Namespace}
			Expect(cli.Get(ctx, routeKey, rbacHTTPRoute)).Should(Succeed())
			Expect(rbacHTTPRoute.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the RBAC ConfigMap object")
			rbacConfigMap := &corev1.ConfigMap{}
			configMapKey := types.NamespacedName{Name: Name + "-rbac-config", Namespace: Namespace}
			Expect(cli.Get(ctx, configMapKey, rbacConfigMap)).Should(Succeed())
			Expect(rbacConfigMap.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})
	})

	When("Creating a Notebook without RBAC proxy injection", func() {
		const (
			Name      = "test-notebook-no-rbac"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)

		It("Should create a Notebook without RBAC proxy", func() {
			By("By creating a new Notebook without RBAC annotation")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that no RBAC proxy container was injected")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())

			// Verify only the original container exists
			Expect(notebook.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(notebook.Spec.Template.Spec.Containers[0].Name).To(Equal(Name))

			// Verify no RBAC volumes were added
			volumeNames := make(map[string]bool)
			for _, volume := range notebook.Spec.Template.Spec.Volumes {
				volumeNames[volume.Name] = true
			}
			Expect(volumeNames["rbac-proxy-config"]).To(BeFalse())
			Expect(volumeNames["rbac-tls-certificates"]).To(BeFalse())
		})

		It("Should create an unauthenticated HTTPRoute", func() {
			By("By checking that the controller has created an unauthenticated HTTPRoute")
			httpRoute := &gatewayv1.HTTPRoute{}
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, httpRoute)
			}, duration, interval).Should(Succeed())

			// Verify it's an unauthenticated HTTPRoute (points to the notebook service, not RBAC service)
			Expect(len(httpRoute.Spec.Rules)).To(BeNumerically(">", 0))
			Expect(len(httpRoute.Spec.Rules[0].BackendRefs)).To(BeNumerically(">", 0))
			Expect(string(httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Name)).To(Equal(Name))
			Expect(*httpRoute.Spec.Rules[0].BackendRefs[0].BackendRef.BackendObjectReference.Port).To(Equal(gatewayv1.PortNumber(8888)))
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

func createNotebookWithRBAC(name, namespace string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				AnnotationInjectRbac: "true",
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
