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
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	"github.com/kubeflow/kubeflow/components/notebook-controller/pkg/culler"
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

		expectedRoute := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: routev1.RouteSpec{
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   Name,
					Weight: ptr.To[int32](100),
				},
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString("http-" + Name),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationEdge,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
			Status: routev1.RouteStatus{
				Ingress: []routev1.RouteIngress{},
			},
		}

		route := &routev1.Route{}

		It("Should create a Route to expose the traffic externally", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the controller has created the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should reconcile the Route when modified", func() {
			By("By simulating a manual Route modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"to":{"name":"foo"}}}`))
			Expect(cli.Patch(ctx, route, patch)).Should(Succeed())

			By("By checking that the controller has restored the Route spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, route)
				if err != nil {
					return "", err
				}
				return route.Spec.To.Name, nil
			}, duration, interval).Should(Equal(Name))
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should recreate the Route when deleted", func() {
			By("By deleting the notebook route")
			Expect(cli.Delete(ctx, route)).Should(Succeed())

			By("By checking that the controller has recreated the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
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
			Expect(route.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

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
			trustedCACertBundle := createOAuthConfigmap(
				"odh-trusted-ca-bundle",
				"default",
				map[string]string{
					"config.openshift.io/inject-trusted-cabundle": "true",
				},
				// NOTE: use valid short CA certs and make them each be different
				// $ openssl req -nodes -x509 -newkey ed25519 -days 365 -set_serial 1 -out /dev/stdout -subj "/"
				map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI3MzdaFw0yNTExMTMy\nMzI3MzdaMAAwKjAFBgMrZXADIQDEMMlJ1P0gyxEV7A8PgpNosvKZgE4ttDDpu/w9\n35BHzjAFBgMrZXADQQDHT8ulalOcI6P5lGpoRcwLzpa4S/5pyqtbqw2zuj7dIJPI\ndNb1AkbARd82zc9bF+7yDkCNmLIHSlDORUYgTNEL\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI2NTlaFw0yNTExMTMy\nMzI2NTlaMAAwKjAFBgMrZXADIQB/v02zcoIIcuan/8bd7cvrBuCGTuVZBrYr1RdA\n0k58yzAFBgMrZXADQQBKsL1tkpOZ6NW+zEX3mD7bhmhxtODQHnANMXEXs0aljWrm\nAxDrLdmzsRRYFYxe23OdXhWqPs8SfO8EZWEvXoME\n-----END CERTIFICATE-----",
				})

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
			// TODO(RHOAIENG-15907): adding sleep to reduce flakiness
			time.Sleep(2 * time.Second)
			configMapName := "workbench-trusted-ca-bundle"
			checkCertConfigMap(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 3)
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

		expectedRoute := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "nb-",
				Namespace:    Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: routev1.RouteSpec{
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   Name,
					Weight: ptr.To[int32](100),
				},
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString("http-" + Name),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationEdge,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
			Status: routev1.RouteStatus{
				Ingress: []routev1.RouteIngress{},
			},
		}

		route := &routev1.Route{}

		It("Should create a Route to expose the traffic externally", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has created the Route")
			Eventually(func() error {
				route, err := getRouteFromList(route, notebook, Name, Namespace)
				if route == nil {
					return err
				}
				return nil
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should reconcile the Route when modified", func() {
			By("By simulating a manual Route modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"to":{"name":"foo"}}}`))
			Expect(cli.Patch(ctx, route, patch)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has restored the Route spec")
			Eventually(func() (string, error) {
				route, err := getRouteFromList(route, notebook, Name, Namespace)
				if route == nil {
					return "", err
				}
				return route.Spec.To.Name, nil
			}, duration, interval).Should(Equal(Name))
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should recreate the Route when deleted", func() {
			By("By deleting the notebook route")
			Expect(cli.Delete(ctx, route)).Should(Succeed())
			time.Sleep(interval)

			By("By checking that the controller has recreated the Route")
			Eventually(func() error {
				route, err := getRouteFromList(route, notebook, Name, Namespace)
				if route == nil {
					return err
				}
				return nil
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
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
			Expect(route.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

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

			updatedImage := "registry.redhat.io/ubi8/ubi:updated"
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
			trustedCACertBundle := createOAuthConfigmap(
				"odh-trusted-ca-bundle",
				"default",
				map[string]string{
					"config.openshift.io/inject-trusted-cabundle": "true",
				},
				map[string]string{
					"ca-bundle.crt":     "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4MjZaFw0yNTExMTMy\nMzI4MjZaMAAwKjAFBgMrZXADIQD77pLvWIX0WmlkYthRZ79oIf7qrGO7yECf668T\nSB42vTAFBgMrZXADQQDs76j81LPh+lgnnf4L0ROUqB66YiBx9SyDTjm83Ya4KC+2\nLEP6Mw1//X2DX89f1chy7RxCpFS3eXb7U/p+GPwA\n-----END CERTIFICATE-----",
					"odh-ca-bundle.crt": "-----BEGIN CERTIFICATE-----\nMIGrMF+gAwIBAgIBATAFBgMrZXAwADAeFw0yNDExMTMyMzI4NDJaFw0yNTExMTMy\nMzI4NDJaMAAwKjAFBgMrZXADIQAw01381TUVSxaCvjQckcw3RTcg+bsVMgNZU8eF\nXa/f3jAFBgMrZXADQQBeJZHSiMOYqa/tXUrQTfNIcklHuvieGyBRVSrX3bVUV2uM\nDBkZLsZt65rCk1A8NG+xkA6j3eIMAA9vBKJ0ht8F\n-----END CERTIFICATE-----",
				})
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

			updatedImage := "registry.redhat.io/ubi8/ubi:updated"
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
			// TODO(RHOAIENG-15907): adding sleep to reduce flakiness
			time.Sleep(2 * time.Second)
			configMapName := "workbench-trusted-ca-bundle"
			checkCertConfigMap(ctx, notebook.Namespace, configMapName, "ca-bundle.crt", 3)
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

		expectedNotebookOAuthNetworkPolicy := createOAuthNetworkPolicy(notebook.Name, notebook.Namespace, npProtocol, NotebookOAuthPort)

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
				key := types.NamespacedName{Name: Name + "-oauth-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(expectedNotebookOAuthNetworkPolicy, CompareNotebookNetworkPolicies))
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
				key := types.NamespacedName{Name: Name + "-oauth-np", Namespace: Namespace}
				return cli.Get(ctx, key, notebookOAuthNetworkPolicy)
			}, duration, interval).Should(Succeed())
			Expect(*notebookOAuthNetworkPolicy).To(BeMatchingK8sResource(expectedNotebookOAuthNetworkPolicy, CompareNotebookNetworkPolicies))
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

	When("Creating a Notebook with OAuth", func() {
		const (
			Name      = "test-notebook-oauth"
			Namespace = "default"
		)

		notebook := createNotebook(Name, Namespace)
		notebook.SetLabels(map[string]string{
			"app.kubernetes.io/instance": Name,
		})
		notebook.SetAnnotations(map[string]string{
			"notebooks.opendatahub.io/inject-oauth":     "true",
			"notebooks.opendatahub.io/foo":              "bar",
			"notebooks.opendatahub.io/oauth-logout-url": "https://example.notebook-url/notebook/" + Namespace + "/" + Name,
		})
		notebook.Spec = nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  Name,
						Image: "registry.redhat.io/ubi8/ubi:latest",
					}},
					Volumes: []corev1.Volume{
						{
							Name: "notebook-data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: Name + "-data",
								},
							},
						},
					},
				},
			},
		}

		expectedNotebook := nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/instance": Name,
				},
				Annotations: map[string]string{
					"notebooks.opendatahub.io/inject-oauth":     "true",
					"notebooks.opendatahub.io/foo":              "bar",
					"notebooks.opendatahub.io/oauth-logout-url": "https://example.notebook-url/notebook/" + Namespace + "/" + Name,
					"kubeflow-resource-stopped":                 "odh-notebook-controller-lock",
				},
			},
			Spec: nbv1.NotebookSpec{
				Template: nbv1.NotebookTemplateSpec{
					Spec: corev1.PodSpec{
						ServiceAccountName: Name,
						Containers: []corev1.Container{
							{
								Name:  Name,
								Image: "registry.redhat.io/ubi8/ubi:latest",
							},
							createOAuthContainer(Name, Namespace),
						},
						Volumes: []corev1.Volume{
							{
								Name: "notebook-data",
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
										ClaimName: Name + "-data",
									},
								},
							},
							{
								Name: "oauth-config",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  Name + "-oauth-config",
										DefaultMode: ptr.To[int32](420),
									},
								},
							},
							{
								Name: "oauth-client",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  Name + "-oauth-client",
										DefaultMode: ptr.To[int32](420),
									},
								},
							},
							{
								Name: "tls-certificates",
								VolumeSource: corev1.VolumeSource{
									Secret: &corev1.SecretVolumeSource{
										SecretName:  Name + "-tls",
										DefaultMode: ptr.To[int32](420),
									},
								},
							},
						},
					},
				},
			},
		}

		It("Should inject the OAuth proxy as a sidecar container", func() {
			By("By creating a new Notebook")
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			By("By checking that the webhook has injected the sidecar container")
			Expect(*notebook).To(BeMatchingK8sResource(expectedNotebook, CompareNotebooks))
		})

		It("Should remove the reconciliation lock annotation", func() {
			By("By checking that the annotation lock annotation is not present")
			delete(expectedNotebook.Annotations, culler.STOP_ANNOTATION)
			Eventually(func() (nbv1.Notebook, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, notebook)
				return *notebook, err
			}, duration, interval).Should(BeMatchingK8sResource(expectedNotebook, CompareNotebooks))
		})

		It("Should reconcile the Notebook when modified", func() {
			By("By simulating a manual Notebook modification")
			notebook.Spec.Template.Spec.ServiceAccountName = "foo"
			notebook.Spec.Template.Spec.Containers[1].Image = "bar"
			notebook.Spec.Template.Spec.Volumes[1].VolumeSource = corev1.VolumeSource{}
			Expect(cli.Update(ctx, notebook)).Should(Succeed())

			By("By checking that the webhook has restored the Notebook spec")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(Succeed())
			Expect(*notebook).To(BeMatchingK8sResource(expectedNotebook, CompareNotebooks))
		})

		serviceAccount := &corev1.ServiceAccount{}
		expectedServiceAccount := createOAuthServiceAccount(Name, Namespace)

		It("Should create a Service Account for the notebook", func() {
			By("By checking that the controller has created the Service Account")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(*serviceAccount).To(BeMatchingK8sResource(expectedServiceAccount, CompareNotebookServiceAccounts))
		})

		It("Should recreate the Service Account when deleted", func() {
			By("By deleting the notebook Service Account")
			Expect(cli.Delete(ctx, serviceAccount)).Should(Succeed())

			By("By checking that the controller has recreated the Service Account")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, serviceAccount)
			}, duration, interval).Should(Succeed())
			Expect(*serviceAccount).To(BeMatchingK8sResource(expectedServiceAccount, CompareNotebookServiceAccounts))
		})

		service := &corev1.Service{}
		expectedService := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name + "-tls",
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
				Annotations: map[string]string{
					"service.beta.openshift.io/serving-cert-secret-name": Name + "-tls",
				},
			},
			Spec: corev1.ServiceSpec{
				Ports: []corev1.ServicePort{{
					Name:       OAuthServicePortName,
					Port:       OAuthServicePort,
					TargetPort: intstr.FromString(OAuthServicePortName),
					Protocol:   corev1.ProtocolTCP,
				}},
			},
		}

		It("Should create a Service to expose the OAuth proxy", func() {
			By("By checking that the controller has created the Service")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-tls", Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(*service).To(BeMatchingK8sResource(expectedService, CompareNotebookServices))
		})

		It("Should recreate the Service when deleted", func() {
			By("By deleting the notebook Service")
			Expect(cli.Delete(ctx, service)).Should(Succeed())

			By("By checking that the controller has recreated the Service")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-tls", Namespace: Namespace}
				return cli.Get(ctx, key, service)
			}, duration, interval).Should(Succeed())
			Expect(*service).To(BeMatchingK8sResource(expectedService, CompareNotebookServices))
		})

		secret := &corev1.Secret{}

		It("Should create a Secret with the OAuth proxy configuration", func() {
			By("By checking that the controller has created the Secret")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-config", Namespace: Namespace}
				return cli.Get(ctx, key, secret)
			}, duration, interval).Should(Succeed())

			By("By checking that the cookie secret format is correct")
			Expect(len(secret.Data["cookie_secret"])).Should(Equal(32))
		})

		It("Should recreate the Secret when deleted", func() {
			By("By deleting the notebook Secret")
			Expect(cli.Delete(ctx, secret)).Should(Succeed())

			By("By checking that the controller has recreated the Secret")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name + "-oauth-config", Namespace: Namespace}
				return cli.Get(ctx, key, secret)
			}, duration, interval).Should(Succeed())
		})

		route := &routev1.Route{}
		expectedRoute := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      Name,
				Namespace: Namespace,
				Labels: map[string]string{
					"notebook-name": Name,
				},
			},
			Spec: routev1.RouteSpec{
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   Name + "-tls",
					Weight: ptr.To[int32](100),
				},
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString(OAuthServicePortName),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationReencrypt,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
			Status: routev1.RouteStatus{
				Ingress: []routev1.RouteIngress{},
			},
		}

		It("Should create a Route to expose the traffic externally", func() {
			By("By checking that the controller has created the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should recreate the Route when deleted", func() {
			By("By deleting the notebook Route")
			Expect(cli.Delete(ctx, route)).Should(Succeed())

			By("By checking that the controller has recreated the Route")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, route)
			}, duration, interval).Should(Succeed())
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should reconcile the Route when modified", func() {
			By("By simulating a manual Route modification")
			patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"to":{"name":"foo"}}}`))
			Expect(cli.Patch(ctx, route, patch)).Should(Succeed())

			By("By checking that the controller has restored the Route spec")
			Eventually(func() (string, error) {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				err := cli.Get(ctx, key, route)
				if err != nil {
					return "", err
				}
				return route.Spec.To.Name, nil
			}, duration, interval).Should(Equal(Name + "-tls"))
			Expect(*route).To(BeMatchingK8sResource(expectedRoute, CompareNotebookRoutes))
		})

		It("Should delete the OAuth proxy objects", func() {
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

			By("By checking that the Notebook owns the Service Account object")
			Expect(serviceAccount.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Service object")
			Expect(service.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Secret object")
			Expect(secret.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By checking that the Notebook owns the Route object")
			Expect(route.GetObjectMeta().GetOwnerReferences()).To(ContainElement(expectedOwnerReference))

			By("By deleting the recently created Notebook")
			Expect(cli.Delete(ctx, notebook)).Should(Succeed())

			By("By checking that the Notebook is deleted")
			Eventually(func() error {
				key := types.NamespacedName{Name: Name, Namespace: Namespace}
				return cli.Get(ctx, key, notebook)
			}, duration, interval).Should(HaveOccurred())
		})
	})

	When("Creating notebook as part of Service Mesh", func() {

		const (
			name      = "test-notebook-mesh"
			namespace = "mesh-ns"
		)
		testNamespaces = append(testNamespaces, namespace)

		notebookOAuthNetworkPolicy := createOAuthNetworkPolicy(name, namespace, corev1.ProtocolTCP, NotebookOAuthPort)

		It("Should not add OAuth sidecar", func() {
			notebook := createNotebook(name, namespace)
			notebook.SetAnnotations(map[string]string{AnnotationServiceMesh: "true"})
			Expect(cli.Create(ctx, notebook)).Should(Succeed())

			actualNotebook := &nbv1.Notebook{}
			Eventually(func() error {
				key := types.NamespacedName{Name: name, Namespace: namespace}
				return cli.Get(ctx, key, actualNotebook)
			}, duration, interval).Should(Succeed())

			Expect(actualNotebook.Spec.Template.Spec.Containers).To(Not(ContainElement(createOAuthContainer(name, namespace))))
		})

		It("Should not define OAuth network policy", func() {
			policies := &netv1.NetworkPolicyList{}
			Eventually(func() error {
				return cli.List(ctx, policies, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(policies.Items).To(Not(ContainElement(notebookOAuthNetworkPolicy)))
		})

		It("Should not create routes", func() {
			routes := &routev1.RouteList{}
			Eventually(func() error {
				return cli.List(ctx, routes, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(routes.Items).To(BeEmpty())
		})

		It("Should not create OAuth Service Account", func() {
			oauthServiceAccount := createOAuthServiceAccount(name, namespace)

			serviceAccounts := &corev1.ServiceAccountList{}
			Eventually(func() error {
				return cli.List(ctx, serviceAccounts, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(serviceAccounts.Items).ToNot(ContainElement(oauthServiceAccount))
		})

		It("Should not create OAuth secret", func() {
			secrets := &corev1.SecretList{
				TypeMeta: metav1.TypeMeta{
					Kind:       "SecretList",
					APIVersion: "v1",
				},
			}
			Eventually(func() error {
				return cli.List(ctx, secrets, client.InNamespace(namespace))
			}, duration, interval).Should(Succeed())

			Expect(secrets.Items).To(BeEmpty())
		})

	})

	When("Checking ds-pipeline-config secret lifecycle", func() {
		const (
			notebookName          = "dspa-notebook"
			dsSecretName          = "ds-pipeline-config"
			Namespace             = "dspa-test-namespace"
			accessKeyKey          = "accesskey"
			secretKeyKey          = "secretkey"
			secretName            = "cos-secret"
			dashboardInstanceName = "default-dashboard"
		)

		testNamespaces = append(testNamespaces, Namespace)

		var (
			dspaObj      *dspav1.DataSciencePipelinesApplication
			s3CredSecret *corev1.Secret
			dashboard    *unstructured.Unstructured
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
			// Create all the necessary objects within dspa-test-namespace namespace: DSPA CR, Dashboard CR, and Dashboard Secret.
			// These components require the 'ds-pipeline-config' secret to be generated beforehand.
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

			By("Creating a Dashboard CR")
			dashboard = &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "components.platform.opendatahub.io/v1alpha1",
					"kind":       "Dashboard",
					"metadata": map[string]interface{}{
						"name":      dashboardInstanceName,
						"namespace": Namespace,
					},
				},
			}
			dashboard.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "components.platform.opendatahub.io",
				Version: "v1alpha1",
				Kind:    "Dashboard",
			})
			Expect(cli.Create(ctx, dashboard)).To(Succeed())

			By("Patching the Dashboard status.url and Ready condition to simulate readiness")
			// Fetch the Dashboard to get a valid resourceVersion
			Expect(cli.Get(ctx, client.ObjectKeyFromObject(dashboard), dashboard)).To(Succeed())
			// Create a patch with updated status.url and conditions
			dashboardPatch := dashboard.DeepCopy()
			err := unstructured.SetNestedField(dashboardPatch.Object, "https://dashboard.example.com", "status", "url")
			Expect(err).ToNot(HaveOccurred())
			readyCondition := map[string]interface{}{
				"type":   "Ready",
				"status": "True",
				"reason": "ComponentsAvailable",
			}
			err = unstructured.SetNestedSlice(dashboardPatch.Object, []interface{}{readyCondition}, "status", "conditions")
			Expect(err).ToNot(HaveOccurred())
			patch := client.MergeFrom(dashboard)
			Expect(cli.Status().Patch(ctx, dashboardPatch, patch)).To(Succeed())

			By("Waiting the Dashboard object is present and Ready")
			time.Sleep(10 * time.Millisecond)

			By("Creating Notebook")
			notebook := createNotebook(notebookName, Namespace)
			Expect(cli.Create(ctx, notebook)).To(Succeed())

			By("Waiting for ds-pipeline-config Secret to be created")
			Eventually(func() error {
				return cli.Get(ctx, types.NamespacedName{Name: dsSecretName, Namespace: Namespace}, &corev1.Secret{})
			}, 30*time.Second, 2*time.Second).Should(Succeed())

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
			err = cli.Get(ctx, client.ObjectKey{Name: notebookName, Namespace: Namespace}, notebook)
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
				g.Expect(updatedSecret.ResourceVersion).ToNot(Equal(fetchedSecret), "Secret resource version should change on DSPA update")
			}, 10*time.Second, 500*time.Millisecond).Should(Succeed())

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

			// Delete Dashboard
			if dashboard != nil {
				_ = cli.Delete(ctx, dashboard)
				Eventually(func() error {
					return cli.Get(ctx, client.ObjectKeyFromObject(dashboard), dashboard)
				}, time.Second*5, time.Millisecond*250).ShouldNot(Succeed())
			}

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
					Image: "registry.redhat.io/ubi8/ubi:latest",
				}}}},
		},
	}
}

func getRouteFromList(route *routev1.Route, notebook *nbv1.Notebook, name, namespace string) (*routev1.Route, error) {
	routeList := &routev1.RouteList{}
	opts := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels{"notebook-name": name},
	}

	err := cli.List(ctx, routeList, opts...)
	if err != nil {
		return nil, err
	}

	// Get the route from the list
	for _, nRoute := range routeList.Items {
		if metav1.IsControlledBy(&nRoute, notebook) {
			*route = nRoute
			return route, nil
		}
	}
	return nil, errors.New("Route not found")
}

func createOAuthServiceAccount(name, namespace string) corev1.ServiceAccount {
	return corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"notebook-name": name,
			},
			Annotations: map[string]string{
				"serviceaccounts.openshift.io/oauth-redirectreference.first": "" +
					`{"kind":"OAuthRedirectReference","apiVersion":"v1","reference":{"kind":"Route","name":"` + name + `"}}`,
			},
		},
	}
}

func createOAuthContainer(name, namespace string) corev1.Container {
	return corev1.Container{
		Name:            "oauth-proxy",
		Image:           OAuthProxyImage,
		ImagePullPolicy: corev1.PullAlways,
		Env: []corev1.EnvVar{{
			Name: "NAMESPACE",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.namespace",
				},
			},
		}},
		Args: []string{
			"--provider=openshift",
			"--https-address=:8443",
			"--http-address=",
			"--openshift-service-account=" + name,
			"--cookie-secret-file=/etc/oauth/config/cookie_secret",
			"--cookie-expire=24h0m0s",
			"--tls-cert=/etc/tls/private/tls.crt",
			"--tls-key=/etc/tls/private/tls.key",
			"--upstream=http://localhost:8888",
			"--upstream-ca=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			"--email-domain=*",
			"--skip-provider-button",
			`--client-id=` + name + `-` + namespace + `-oauth-client`,
			"--client-secret-file=/etc/oauth/client/secret",
			"--scope=user:info user:check-access",
			`--openshift-sar={"verb":"get","resource":"notebooks","resourceAPIGroup":"kubeflow.org",` +
				`"resourceName":"` + name + `","namespace":"$(NAMESPACE)"}`,
			"--logout-url=https://example.notebook-url/notebook/" + namespace + "/" + name,
		},
		Ports: []corev1.ContainerPort{{
			Name:          OAuthServicePortName,
			ContainerPort: 8443,
			Protocol:      corev1.ProtocolTCP,
		}},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 30,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   "/oauth/healthz",
					Port:   intstr.FromString(OAuthServicePortName),
					Scheme: corev1.URISchemeHTTPS,
				},
			},
			InitialDelaySeconds: 5,
			TimeoutSeconds:      1,
			PeriodSeconds:       5,
			SuccessThreshold:    1,
			FailureThreshold:    3,
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				"cpu":    resource.MustParse("100m"),
				"memory": resource.MustParse("64Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      "oauth-client",
				MountPath: "/etc/oauth/client",
			},
			{
				Name:      "oauth-config",
				MountPath: "/etc/oauth/config",
			},
			{
				Name:      "tls-certificates",
				MountPath: "/etc/tls/private",
			},
		},
	}
}

func createOAuthNetworkPolicy(name, namespace string, npProtocol corev1.Protocol, port int32) netv1.NetworkPolicy {
	return netv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-oauth-np",
			Namespace: namespace,
		},
		Spec: netv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"notebook-name": name,
				},
			},
			Ingress: []netv1.NetworkPolicyIngressRule{
				{
					Ports: []netv1.NetworkPolicyPort{
						{
							Protocol: &npProtocol,
							Port: &intstr.IntOrString{
								IntVal: port,
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

// createOAuthConfigmap creates a ConfigMap
// this function can be used to create any kinda of ConfigMap
func createOAuthConfigmap(name, namespace string, label map[string]string, configMapData map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    label,
		},
		Data: configMapData,
	}
}

// checkCertConfigMap checks the content of a config map defined by the name and namespace
// It tries to parse the given certFileName and checks that all certificates can be parsed there and that the number of the certificates matches what we expect.
func checkCertConfigMap(ctx context.Context, namespace string, configMapName string, certFileName string, expNumberCerts int) {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: configMapName}
	Expect(cli.Get(ctx, key, configMap)).Should(Succeed())

	// Attempt to decode PEM encoded certificates so we are sure all are readable as expected
	certData := configMap.Data[certFileName]
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
			_, err := x509.ParseCertificate(block.Bytes)
			Expect(err).ShouldNot(HaveOccurred())
			certificatesFound++
		}
	}
	Expect(certificatesFound).Should(Equal(expNumberCerts), "Number of parsed certificates don't match expected one:\n"+certData)
}
