package suite_test

import (
	"context"
	"crypto/tls"
	"fmt"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/opendatahub-io/kubeflow/components/odh-notebook-controller/controllers"
	io_prometheus_client "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"net"
	"os"
	"path/filepath"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"time"

	"github.com/google/go-cmp/cmp"
)

var _ = It("Upgrade from RHOAI 2.13", func() {
	err := os.Setenv("USE_ISTIO", "false")
	Expect(err).To(Succeed())
	err = os.Setenv("ADD_FSGROUP", "false")
	Expect(err).To(Succeed())

	By("deploying the dumped 2.13 YAMLs", func() {
		dyn, err := dynamic.NewForConfig(cfg)
		Expect(err).To(Succeed())

		// prepare a RESTMapper to find GVRs
		dc, err := discovery.NewDiscoveryClientForConfig(cfg)
		Expect(err).To(Succeed())
		mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

		// create the test namespace
		ns := &v1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "developer",
			},
		}
		err = cli.Create(context.Background(), ns)
		Expect(err).To(Succeed())

		applyYamlDirectory(dyn, mapper, "data/2.13.0/notebook.kubeflow.org/*.yaml")
		applyYamlDirectory(dyn, mapper, "data/2.13.0/statefulset.apps/*.yaml")
		applyYamlDirectory(dyn, mapper, "data/2.13.0/service/*.yaml")
		applyYamlDirectory(dyn, mapper, "data/2.13.0/configmap/*.yaml")
		applyYamlDirectory(dyn, mapper, "data/2.13.0/route.route.openshift.io/*.yaml")
		// there's still some point applying pods, even though we don't have statefulset controller around to keep pods in sync
		//  all pod checks need to be done on the statefulset, and we must (somehow) prevent the situation where statefulset
		//  is changed for a very short time there and back, enough to restart the pod twice, that would be sneaky!
		// the notebook-controller does check whether pods are around, so we'll some to it, otherwise
		//  brace yourself and see `No Pods are currently running for` log message that we don't want to have around
		applyYamlDirectory(dyn, mapper, "data/2.13.0/pod/*.yaml")
	})

	// install webhooks now
	webhookInstall := &envTest.WebhookInstallOptions
	webhookInstall.Paths = []string{filepath.Join("..", "config", "webhook")}
	webhookInstall.IgnoreErrorIfPathMissing = false

	subctx, subctxCancel := context.WithCancel(ctx)

	err = webhookInstall.Install(cfg)
	Expect(err).To(Succeed())

	// start controllers (and the webhook webserver)
	startTheControllers(mgr, webhookInstall, subctx)
	Expect(err).To(Succeed())

	By("creates new notebook in the lived-in namespace", func() {
		nbContext := setupThothMinimalOAuthNotebook()
		testNotebook := &nbv1.Notebook{
			ObjectMeta: *nbContext.nbObjectMeta,
			Spec:       *nbContext.nbSpec,
		}
		err = cli.Create(context.Background(), testNotebook)
		Expect(err).To(Succeed())
	})

	// wait for controllers to stabilize reconciling all the YAMLs
	waitForReconcilations()

	By("compares notebook on cluster and notebook in YAMLs has same spec", func() {
		var notebookCluster appsv1.StatefulSet
		err = cli.Get(ctx, client.ObjectKey{
			Namespace: "developer",
			Name:      "wb1"}, &notebookCluster)
		Expect(err).To(Succeed())

		// fetch the original yaml from file
		content, err := os.ReadFile("data/2.13.0/statefulset.apps/wb1.yaml")
		Expect(err).To(Succeed())

		// https://godoc.org/k8s.io/apimachinery/pkg/runtime#Decoder
		deserializer := codecFactory.UniversalDeserializer()

		var notebookFile appsv1.StatefulSet
		_, _, err = deserializer.Decode(content, nil, &notebookFile)
		Expect(err).To(Succeed())

		str := cmp.Diff(notebookFile.Spec.Template.Spec, notebookCluster.Spec.Template.Spec)
		Expect(str).To(BeEmpty())
	})

	By("check that newly started notebook has current OAuthProxy image (while old one need not)", func() {
		var notebook appsv1.StatefulSet
		Expect(cli.Get(ctx, client.ObjectKey{
			Namespace: "developer",
			Name:      "thoth-minimal-oauth-notebook"}, &notebook)).To(Succeed())

		var oauthProxyContainer v1.Container
		for _, container := range notebook.Spec.Template.Spec.Containers {
			if container.Name == "oauth-proxy" {
				oauthProxyContainer = container
			}
		}
		Expect(oauthProxyContainer.Image).To(Equal(controllers.OAuthProxyImage))
	})

	subctxCancel()
})

func waitForReconcilations() {
	EventuallyConsistently(ECCheck[[]*io_prometheus_client.MetricFamily]{
		State: nil,
		Fetch: func(g Gomega, s *[]*io_prometheus_client.MetricFamily) {
			metrics, err := metrics.Registry.Gather()
			Expect(err).To(Succeed())
			*s = metrics
		},
		Check: func(g Gomega, s *[]*io_prometheus_client.MetricFamily) {
			metricFamilies, err := metrics.Registry.Gather()
			Expect(err).To(Succeed())

			// these counters need to remain at zero for ConsistentDuration
			metric := lookupMetricWithLabel(metricFamilies, "workqueue_depth", "name", "notebook")
			g.Expect(metric.Gauge.GetValue()).To(Equal(0.0))
			metric = lookupMetricWithLabel(metricFamilies, "workqueue_unfinished_work_seconds", "name", "notebook")
			g.Expect(metric.Gauge.GetValue()).To(Equal(0.0))
			metric = lookupMetricWithLabel(metricFamilies, "controller_runtime_webhook_requests_in_flight", "webhook", "/mutate-notebook-v1")
			g.Expect(metric.Gauge.GetValue()).To(Equal(0.0))

			// and these counters are to stay constant
			g.Expect(sumUpMetricsWithLabel(metricFamilies, "workqueue_adds_total", "webhook", "/mutate-notebook-v1")).To(
				Equal(sumUpMetricsWithLabel(*s, "workqueue_adds_total", "webhook", "/mutate-notebook-v1")))
			g.Expect(sumUpMetricsWithLabel(metricFamilies, "controller_runtime_reconcile_total", "", "")).To(
				Equal(sumUpMetricsWithLabel(*s, "controller_runtime_reconcile_total", "", "")))
			g.Expect(sumUpMetricsWithLabel(metricFamilies, "rest_client_requests_total", "", "")).To(
				Equal(sumUpMetricsWithLabel(*s, "rest_client_requests_total", "", "")))
		},
		EventualTimeout:    timeout,
		ConsistentInterval: 250 * time.Millisecond,
		ConsistentDuration: interval,
	})
}

func lookupMetricWithLabel(metricFamilies []*io_prometheus_client.MetricFamily, metricName, labelName, LabelValue string) (result *io_prometheus_client.Metric) {
	for _, metric := range lookupMetricsWithLabel(metricFamilies, metricName, labelName, LabelValue) {
		if result != nil {
			panic("lookupMetricWithLabel: there's more than one metric of the name that's matching given label")
		}
		result = metric
	}
	return
}

// lookupMetricsWithLabel returns zero or more metrics of the given name that have the given label.
// when label name and value are empty strings, then all metrics of the given name are returned
func lookupMetricsWithLabel(metricFamilies []*io_prometheus_client.MetricFamily, metricName, labelName, labelValue string) (result []*io_prometheus_client.Metric) {
	result = make([]*io_prometheus_client.Metric, 0)
	for _, metricFamily := range metricFamilies {
		if metricFamily.GetName() != metricName {
			continue
		}
		for _, metric := range metricFamily.GetMetric() {
			for _, labelPair := range metric.GetLabel() {
				if (labelName == "" && labelValue == "") || (labelPair.GetName() == labelName && labelPair.GetValue() == labelValue) {
					result = append(result, metric)
					break
				}
			}
		}
	}
	return
}

func sumUpMetricsWithLabel(metricFamilies []*io_prometheus_client.MetricFamily, metricName, labelName, LabelValue string) (result float64) {
	for _, metric := range lookupMetricsWithLabel(metricFamilies, metricName, labelName, LabelValue) {
		result += metricToFloat64(metric)
	}
	return
}

func metricToFloat64(metric *io_prometheus_client.Metric) float64 {
	if metric.Gauge != nil {
		return metric.Gauge.GetValue()
	}
	if metric.Counter != nil {
		return metric.Counter.GetValue()
	}
	if metric.Untyped != nil {
		return metric.Untyped.GetValue()
	}
	panic("metricToFloat64: argument is of unsupported type")
}

func applyYamlDirectory(dyn *dynamic.DynamicClient, mapper *restmapper.DeferredDiscoveryRESTMapper, globPattern string) {
	glob, err := filepath.Glob(globPattern)
	Expect(err).To(Succeed())
	for _, filename := range glob {
		content, err := os.ReadFile(filename)
		Expect(err).To(Succeed())

		obj := &unstructured.Unstructured{}
		// decode YAML into unstructured.Unstructured
		dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
		_, gvk, err := dec.Decode(content, nil, obj)
		Expect(err).To(Succeed())

		// remove resourceVersion
		obj.SetResourceVersion("")

		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		Expect(err).To(Succeed())
		created, err := dyn.Resource(mapping.Resource).Namespace("developer").Create(context.TODO(), obj, metav1.CreateOptions{})
		Expect(err).To(Succeed())
		// configmaps don't have status
		if obj.Object["status"] == nil {
			continue
		}
		obj.SetUID(created.GetUID())
		obj.SetResourceVersion(created.GetResourceVersion())
		_, err = dyn.Resource(mapping.Resource).Namespace("developer").UpdateStatus(context.TODO(), obj, metav1.UpdateOptions{})
		Expect(err).To(Succeed())
	}
}

func startTheControllers(mgr manager.Manager, webhookInstallOptions *envtest.WebhookInstallOptions, ctx context.Context) {
	// Start the manager
	go func() {
		defer GinkgoRecover()
		err := mgr.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "Failed to run manager")
	}()

	// Wait for the webhook server to get ready
	dialer := &net.Dialer{Timeout: 1 * time.Second}
	addrPort := fmt.Sprintf("%s:%d", webhookInstallOptions.LocalServingHost, webhookInstallOptions.LocalServingPort)
	Eventually(func() error {
		conn, err := tls.DialWithDialer(dialer, "tcp", addrPort, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	}).Should(Succeed())
}

// Add spec and metadata for Notebook objects
func setupThothMinimalOAuthNotebook() notebookContext {
	notebookTestNamespace := "developer"

	testNotebookName := "thoth-minimal-oauth-notebook"

	testNotebook := &nbv1.Notebook{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{"notebooks.opendatahub.io/inject-oauth": "true"},
			Name:        testNotebookName,
			Namespace:   notebookTestNamespace,
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:       "thoth-minimal-oauth-notebook",
							Image:      "quay.io/thoth-station/s2i-minimal-notebook:v0.2.2",
							WorkingDir: "/opt/app-root/src",
							Ports: []v1.ContainerPort{
								{
									Name:          "notebook-port",
									ContainerPort: 8888,
									Protocol:      "TCP",
								},
							},
							EnvFrom: []v1.EnvFromSource{},
							Env: []v1.EnvVar{
								{
									Name:  "JUPYTER_NOTEBOOK_PORT",
									Value: "8888",
								},
								{
									Name:  "NOTEBOOK_ARGS",
									Value: "--ServerApp.port=8888 --NotebookApp.token='' --NotebookApp.password='' --ServerApp.base_url=/notebook/" + notebookTestNamespace + "/" + testNotebookName,
								},
							},
							Resources: v1.ResourceRequirements{
								Limits: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
								Requests: map[v1.ResourceName]resource.Quantity{
									v1.ResourceCPU:    resource.MustParse("1"),
									v1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
							LivenessProbe: &v1.Probe{
								ProbeHandler: v1.ProbeHandler{
									HTTPGet: &v1.HTTPGetAction{
										Path:   "/notebook/" + notebookTestNamespace + "/" + testNotebookName + "/api",
										Port:   intstr.FromString("notebook-port"),
										Scheme: "HTTP",
									},
								},
								InitialDelaySeconds: 5,
								TimeoutSeconds:      1,
								PeriodSeconds:       5,
								SuccessThreshold:    1,
								FailureThreshold:    3,
							},
						},
					},
				},
			},
		},
	}

	thothMinimalOAuthNbContext := notebookContext{
		nbObjectMeta:   &testNotebook.ObjectMeta,
		nbSpec:         &testNotebook.Spec,
		deploymentMode: OAuthProxy,
	}
	return thothMinimalOAuthNbContext
}

// notebookContext holds information about test notebook
// Any notebook that needs to be added to the e2e test suite should be defined in
// the notebookContext struct.
type notebookContext struct {
	// metadata for Notebook object
	nbObjectMeta *metav1.ObjectMeta
	// metadata for Notebook Spec
	nbSpec         *nbv1.NotebookSpec
	deploymentMode DeploymentMode
}

// DeploymentMode indicates what infra scenarios should be verified by the test
// with default being OAuthProxy scenario.
type DeploymentMode int

const (
	OAuthProxy DeploymentMode = iota
	ServiceMesh
)
