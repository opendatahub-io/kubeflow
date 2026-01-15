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

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	dspav1 "github.com/opendatahub-io/data-science-pipelines-operator/api/v1"
	routev1 "github.com/openshift/api/route/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("getGatewayConfigOwnerName", func() {
	It("should return empty string when gateway instance is empty", func() {
		result := getGatewayConfigOwnerName(map[string]interface{}{})
		Expect(result).To(Equal(""))
	})

	It("should return empty string when gateway has no metadata", func() {
		gatewayInstance := map[string]interface{}{
			"spec": map[string]interface{}{},
		}
		result := getGatewayConfigOwnerName(gatewayInstance)
		Expect(result).To(Equal(""))
	})

	It("should return empty string when gateway has no ownerReferences", func() {
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "test-gateway",
			},
		}
		result := getGatewayConfigOwnerName(gatewayInstance)
		Expect(result).To(Equal(""))
	})

	It("should return empty string when ownerReferences has no GatewayConfig", func() {
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "SomeOtherKind",
						"name": "other-owner",
					},
				},
			},
		}
		result := getGatewayConfigOwnerName(gatewayInstance)
		Expect(result).To(Equal(""))
	})

	It("should return GatewayConfig name when found in ownerReferences", func() {
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
		}
		result := getGatewayConfigOwnerName(gatewayInstance)
		Expect(result).To(Equal("my-gateway-config"))
	})

	It("should return GatewayConfig name even with multiple owners", func() {
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "SomeOtherKind",
						"name": "other-owner",
					},
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
		}
		result := getGatewayConfigOwnerName(gatewayInstance)
		Expect(result).To(Equal("my-gateway-config"))
	})
})

var _ = Describe("getHostnameForPublicEndpoint", func() {
	var (
		testCtx    context.Context
		testScheme *runtime.Scheme
		log        = ctrl.Log.WithName("test")
	)

	BeforeEach(func() {
		testCtx = context.Background()
		testScheme = runtime.NewScheme()
		Expect(routev1.AddToScheme(testScheme)).To(Succeed())
	})

	It("should return empty string when gateway instance is empty", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
		result := getHostnameForPublicEndpoint(testCtx, map[string]interface{}{}, fakeClient, log)
		Expect(result).To(Equal(""))
	})

	It("should return empty string when gateway instance is nil map", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
		var nilMap map[string]interface{}
		result := getHostnameForPublicEndpoint(testCtx, nilMap, fakeClient, log)
		Expect(result).To(Equal(""))
	})

	It("should return hostname from Gateway CR when spec.listeners[0].hostname is valid", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
		gatewayInstance := map[string]interface{}{
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "gateway.example.com",
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("gateway.example.com"))
	})

	It("should try Route fallback when Gateway spec is missing", func() {
		// Create a route that will be found during fallback
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.example.com",
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(route).Build()

		// Gateway with metadata (for GatewayConfig owner) but no spec
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("route.example.com"))
	})

	It("should try Route fallback when Gateway listeners is empty", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.example.com",
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(route).Build()

		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("route.example.com"))
	})

	It("should try Route fallback when Gateway listener hostname is empty", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.example.com",
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(route).Build()

		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "", // Empty hostname
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("route.example.com"))
	})

	It("should try Route fallback when Gateway first listener has invalid format", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.example.com",
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(route).Build()

		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					"invalid-listener-format", // Not a map
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("route.example.com"))
	})

	It("should return empty string when no GatewayConfig owner and no hostname in Gateway", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		// Gateway with no ownerReferences (can't determine GatewayConfig for Route fallback)
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"name": "test-gateway",
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "", // Empty hostname
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal(""))
	})

	It("should return empty string when Route fallback finds no matching route", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "", // Empty hostname, triggers Route fallback
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal(""))
	})

	It("should prefer Gateway hostname over Route when both are available", func() {
		// Create a route that would be found during fallback
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.example.com",
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(route).Build()

		// Gateway with valid hostname - should be preferred
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "gateway.example.com",
					},
				},
			},
		}
		result := getHostnameForPublicEndpoint(testCtx, gatewayInstance, fakeClient, log)
		Expect(result).To(Equal("gateway.example.com"))
	})
})

var _ = Describe("getHostnameFromRoute", func() {
	var (
		testCtx    context.Context
		testScheme *runtime.Scheme
		log        = ctrl.Log.WithName("test")
	)

	BeforeEach(func() {
		testCtx = context.Background()
		testScheme = runtime.NewScheme()
		Expect(routev1.AddToScheme(testScheme)).To(Succeed())
	})

	It("should return empty string when gatewayConfigName is empty", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
		result, err := getHostnameFromRoute(testCtx, fakeClient, "", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return empty string when no routes exist", func() {
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()
		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return hostname when route with matching GatewayConfig owner exists", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "apps.example.com",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(route).
			Build()

		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal("apps.example.com"))
	})

	It("should return empty string when route has different GatewayConfig owner", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "different-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "apps.example.com",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(route).
			Build()

		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return empty string when route has no owner references", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
			},
			Spec: routev1.RouteSpec{
				Host: "apps.example.com",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(route).
			Build()

		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return empty string when route owner is not GatewayConfig kind", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "SomeOtherKind",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "apps.example.com",
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(route).
			Build()

		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})

	It("should return empty string when route has matching owner but empty host", func() {
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "", // Empty host
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(testScheme).
			WithObjects(route).
			Build()

		result, err := getHostnameFromRoute(testCtx, fakeClient, "my-gateway-config", log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(""))
	})
})

var _ = Describe("extractElyraRuntimeConfigInfo", func() {
	var (
		testCtx    context.Context
		testScheme *runtime.Scheme
		log        = ctrl.Log.WithName("test")
	)

	BeforeEach(func() {
		testCtx = context.Background()
		testScheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(testScheme)).To(Succeed())
		Expect(routev1.AddToScheme(testScheme)).To(Succeed())
	})

	createTestNotebook := func(name, namespace string) *nbv1.Notebook {
		return &nbv1.Notebook{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
		}
	}

	createTestDSPA := func(host, bucket, secretName, accessKey, secretKey, scheme string) *dspav1.DataSciencePipelinesApplication {
		dspa := &dspav1.DataSciencePipelinesApplication{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "dspa",
				Namespace: "test-namespace",
			},
			Spec: dspav1.DSPASpec{
				ObjectStorage: &dspav1.ObjectStorage{
					ExternalStorage: &dspav1.ExternalStorage{
						Host:   host,
						Bucket: bucket,
						Scheme: scheme,
						S3CredentialSecret: &dspav1.S3CredentialSecret{
							SecretName: secretName,
							AccessKey:  accessKey,
							SecretKey:  secretKey,
						},
					},
				},
			},
			Status: dspav1.DSPAStatus{
				Components: dspav1.ComponentStatus{
					APIServer: dspav1.ComponentDetailStatus{
						ExternalUrl: "https://api.example.com",
					},
				},
			},
		}
		return dspa
	}

	It("should return error when DSPA host is empty", func() {
		dspa := createTestDSPA("", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing or invalid 'host'"))
		Expect(result).To(BeNil())
	})

	It("should return error when DSPA bucket is empty", func() {
		dspa := createTestDSPA("minio.example.com", "", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing or invalid 'bucket'"))
		Expect(result).To(BeNil())
	})

	It("should return error when COS secret is not found", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		// No secret created
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("failed to get secret"))
		Expect(result).To(BeNil())
	})

	It("should return error when access key is missing from secret", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		// Secret without access key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"secretkey": []byte("mysecretkey"),
				// "accesskey" is missing
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing key 'accesskey'"))
		Expect(result).To(BeNil())
	})

	It("should return error when secret key is missing from secret", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		// Secret without secret key
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				// "secretkey" is missing
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("missing key 'secretkey'"))
		Expect(result).To(BeNil())
	})

	It("should use default https scheme when not specified", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		metadata := result["metadata"].(map[string]interface{})
		Expect(metadata["cos_endpoint"]).To(Equal("https://minio.example.com"))
	})

	It("should use custom scheme when specified", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "http")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		metadata := result["metadata"].(map[string]interface{})
		Expect(metadata["cos_endpoint"]).To(Equal("http://minio.example.com"))
	})

	It("should set public_api_endpoint when gateway has hostname", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "gateway.example.com",
					},
				},
			},
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		metadata := result["metadata"].(map[string]interface{})
		Expect(metadata["public_api_endpoint"]).To(Equal("https://gateway.example.com/external/elyra/test-namespace"))
	})

	It("should not set public_api_endpoint when gateway is empty", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		metadata := result["metadata"].(map[string]interface{})
		_, hasPublicEndpoint := metadata["public_api_endpoint"]
		Expect(hasPublicEndpoint).To(BeFalse())
	})

	It("should set public_api_endpoint from Route fallback when gateway has no hostname", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")

		// Gateway with ownerReferences to GatewayConfig but no hostname in listeners
		gatewayInstance := map[string]interface{}{
			"metadata": map[string]interface{}{
				"ownerReferences": []interface{}{
					map[string]interface{}{
						"kind": "GatewayConfig",
						"name": "my-gateway-config",
					},
				},
			},
			"spec": map[string]interface{}{
				"listeners": []interface{}{
					map[string]interface{}{
						"hostname": "", // Empty hostname triggers Route fallback
					},
				},
			},
		}

		// COS secret
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}

		// Route owned by the same GatewayConfig - should be used for public_api_endpoint
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-route",
				Namespace: gatewayNamespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						Kind: "GatewayConfig",
						Name: "my-gateway-config",
					},
				},
			},
			Spec: routev1.RouteSpec{
				Host: "route.apps.example.com",
			},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret, route).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		metadata := result["metadata"].(map[string]interface{})
		Expect(metadata["public_api_endpoint"]).To(Equal("https://route.apps.example.com/external/elyra/test-namespace"))
	})

	It("should populate all required Elyra config fields", func() {
		dspa := createTestDSPA("minio.example.com", "my-bucket", "cos-secret", "accesskey", "secretkey", "")
		notebook := createTestNotebook("notebook", "test-namespace")
		gatewayInstance := map[string]interface{}{}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cos-secret",
				Namespace: "test-namespace",
			},
			Data: map[string][]byte{
				"accesskey": []byte("myaccesskey"),
				"secretkey": []byte("mysecretkey"),
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(secret).Build()

		result, err := extractElyraRuntimeConfigInfo(testCtx, gatewayInstance, dspa, fakeClient, notebook, log)
		Expect(err).NotTo(HaveOccurred())
		Expect(result).NotTo(BeNil())

		// Check top-level fields
		Expect(result["display_name"]).To(Equal("Pipeline"))
		Expect(result["schema_name"]).To(Equal("kfp"))

		// Check metadata fields
		metadata := result["metadata"].(map[string]interface{})
		Expect(metadata["display_name"]).To(Equal("Pipeline"))
		Expect(metadata["engine"]).To(Equal("Argo"))
		Expect(metadata["runtime_type"]).To(Equal("KUBEFLOW_PIPELINES"))
		Expect(metadata["auth_type"]).To(Equal("KUBERNETES_SERVICE_ACCOUNT_TOKEN"))
		Expect(metadata["cos_auth_type"]).To(Equal("KUBERNETES_SECRET"))
		Expect(metadata["api_endpoint"]).To(Equal("https://api.example.com"))
		Expect(metadata["cos_endpoint"]).To(Equal("https://minio.example.com"))
		Expect(metadata["cos_bucket"]).To(Equal("my-bucket"))
		Expect(metadata["cos_username"]).To(Equal("myaccesskey"))
		Expect(metadata["cos_password"]).To(Equal("mysecretkey"))
		Expect(metadata["cos_secret"]).To(Equal("cos-secret"))
	})
})
