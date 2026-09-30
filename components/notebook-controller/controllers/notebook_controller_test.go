package controllers

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestNbNameFromInvolvedObject(t *testing.T) {
	testPod := &corev1.Pod{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-notebook-0",
			Namespace: "test-namespace",
			Labels: map[string]string{
				"notebook-name": "test-notebook",
			},
		},
	}

	podEvent := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name: "pod-event",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "test-notebook-0",
			Namespace: "test-namespace",
		},
	}

	testSts := &appsv1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-notebook",
			Namespace: "test",
		},
	}

	stsEvent := &corev1.Event{
		ObjectMeta: v1.ObjectMeta{
			Name: "sts-event",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "StatefulSet",
			Name:      "test-notebook",
			Namespace: "test-namespace",
		},
	}

	tests := []struct {
		name           string
		event          *corev1.Event
		expectedNbName string
	}{
		{
			name:           "pod event",
			event:          podEvent,
			expectedNbName: "test-notebook",
		},
		{
			name:           "statefulset event",
			event:          stsEvent,
			expectedNbName: "test-notebook",
		},
	}
	objects := []client.Object{testPod, testSts}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
			nbName, err := nbNameFromInvolvedObject(c, &test.event.InvolvedObject)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if nbName != test.expectedNbName {
				t.Fatalf("Got %v, Expected %v", nbName, test.expectedNbName)
			}
		})
	}
}

func TestCreateNotebookStatus(t *testing.T) {

	tests := []struct {
		name             string
		currentNb        nbv1.Notebook
		pod              corev1.Pod
		sts              appsv1.StatefulSet
		expectedNbStatus nbv1.NotebookStatus
	}{
		{
			name: "NotebookStatusInitialization",
			currentNb: nbv1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1.NotebookStatus{},
			},
			pod: corev1.Pod{},
			sts: appsv1.StatefulSet{},
			expectedNbStatus: nbv1.NotebookStatus{
				Conditions:     []nbv1.NotebookCondition{},
				ReadyReplicas:  int32(0),
				ContainerState: corev1.ContainerState{},
			},
		},
		{
			name: "NotebookStatusReadyReplicas",
			currentNb: nbv1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1.NotebookStatus{},
			},
			pod: corev1.Pod{},
			sts: appsv1.StatefulSet{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas: int32(1),
				},
			},
			expectedNbStatus: nbv1.NotebookStatus{
				Conditions:     []nbv1.NotebookCondition{},
				ReadyReplicas:  int32(1),
				ContainerState: corev1.ContainerState{},
			},
		},
		{
			name: "NotebookContainerState",
			currentNb: nbv1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1.NotebookStatus{},
			},
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "test",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{
									StartedAt: v1.Time{},
								},
							},
						},
					},
				},
			},
			sts: appsv1.StatefulSet{},
			expectedNbStatus: nbv1.NotebookStatus{
				Conditions:    []nbv1.NotebookCondition{},
				ReadyReplicas: int32(0),
				ContainerState: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{
						StartedAt: v1.Time{},
					},
				},
			},
		},
		{
			name: "mirroringPodConditions",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:               "Running",
							LastProbeTime:      v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
							LastTransitionTime: v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
						},
						{
							Type:               "Waiting",
							LastProbeTime:      v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
							LastTransitionTime: v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
							Reason:             "PodInitializing",
						},
					},
				},
			},
			sts: appsv1.StatefulSet{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: appsv1.StatefulSetStatus{
					ReadyReplicas: int32(1),
				},
			},
			expectedNbStatus: nbv1.NotebookStatus{
				Conditions: []nbv1.NotebookCondition{
					{
						Type:               "Running",
						LastProbeTime:      v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
						LastTransitionTime: v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
					},
					{
						Type:               "Waiting",
						LastProbeTime:      v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
						LastTransitionTime: v1.Date(2022, time.Month(8), 30, 1, 10, 30, 0, time.UTC),
						Reason:             "PodInitializing",
					},
				},
				ReadyReplicas:  int32(1),
				ContainerState: corev1.ContainerState{},
			},
		},
		{
			name: "unschedulablePod",
			pod: corev1.Pod{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:               "PodScheduled",
							LastProbeTime:      v1.Date(2022, time.Month(4), 21, 1, 10, 30, 0, time.UTC),
							LastTransitionTime: v1.Date(2022, time.Month(4), 21, 1, 10, 30, 0, time.UTC),
							Message:            "0/1 nodes are available: 1 Insufficient cpu.",
							Status:             "false",
							Reason:             "Unschedulable",
						},
					},
				},
			},
			sts: appsv1.StatefulSet{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: appsv1.StatefulSetStatus{},
			},
			expectedNbStatus: nbv1.NotebookStatus{
				Conditions: []nbv1.NotebookCondition{
					{
						Type:               "PodScheduled",
						LastProbeTime:      v1.Date(2022, time.Month(4), 21, 1, 10, 30, 0, time.UTC),
						LastTransitionTime: v1.Date(2022, time.Month(4), 21, 1, 10, 30, 0, time.UTC),
						Message:            "0/1 nodes are available: 1 Insufficient cpu.",
						Status:             "false",
						Reason:             "Unschedulable",
					},
				},
				ReadyReplicas:  int32(0),
				ContainerState: corev1.ContainerState{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := createMockReconciler()
			req := ctrl.Request{}
			status, err := createNotebookStatus(r, &test.currentNb, &test.sts, &test.pod, req)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if !reflect.DeepEqual(status, test.expectedNbStatus) {
				t.Errorf("\nExpect: %v; \nOutput: %v", test.expectedNbStatus, status)
			}
		})
	}

}

func createMockReconciler() *NotebookReconciler {
	reconciler := &NotebookReconciler{
		Scheme: runtime.NewScheme(),
		Log:    ctrl.Log,
	}
	return reconciler
}

func TestNotebookOwnerReferenceUsesStorageVersion(t *testing.T) {
	s := runtime.NewScheme()
	if err := nbv1.AddToScheme(s); err != nil {
		t.Fatalf("add notebook v1 to scheme: %v", err)
	}

	nb := &nbv1.Notebook{
		ObjectMeta: v1.ObjectMeta{
			Name:      "test-notebook",
			Namespace: "demo",
			UID:       "94751738-d4f8-4712-9192-e473a7c98d4b",
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "test-notebook",
						Image: "busybox",
					}},
				},
			},
		},
	}

	ss := generateStatefulSet(nb, false)
	if err := ctrl.SetControllerReference(nb, ss, s); err != nil {
		t.Fatalf("set controller reference: %v", err)
	}
	if len(ss.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ownerReference, got %d", len(ss.OwnerReferences))
	}
	if got := ss.OwnerReferences[0].APIVersion; got != nbv1.GroupVersion.String() {
		t.Fatalf("ownerReference.apiVersion = %q, want %q", got, nbv1.GroupVersion.String())
	}

	svc := generateService(nb)
	if err := ctrl.SetControllerReference(nb, svc, s); err != nil {
		t.Fatalf("set controller reference on service: %v", err)
	}
	if got := svc.OwnerReferences[0].APIVersion; got != nbv1.GroupVersion.String() {
		t.Fatalf("service ownerReference.apiVersion = %q, want %q", got, nbv1.GroupVersion.String())
	}
}

func TestIsControlledByNotebookIgnoresAPIVersion(t *testing.T) {
	nb := &nbv1.Notebook{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-notebook",
			UID:  "94751738-d4f8-4712-9192-e473a7c98d4b",
		},
	}
	controller := true
	sts := &appsv1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			Name: "test-notebook",
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: "kubeflow.org/v1beta1",
				Kind:       "Notebook",
				Name:       "test-notebook",
				UID:        nb.UID,
				Controller: &controller,
			}},
		},
	}
	if !isControlledByNotebook(sts, nb) {
		t.Fatal("expected stale v1beta1 ownerReference to still match the Notebook UID")
	}
}

func TestCopyOwnerReferencesUpgradesAPIVersion(t *testing.T) {
	controller := true
	desired := &appsv1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: "kubeflow.org/v1",
				Kind:       "Notebook",
				Name:       "test-notebook",
				UID:        "94751738-d4f8-4712-9192-e473a7c98d4b",
				Controller: &controller,
			}},
		},
	}
	existing := &appsv1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			OwnerReferences: []v1.OwnerReference{{
				APIVersion: "kubeflow.org/v1beta1",
				Kind:       "Notebook",
				Name:       "test-notebook",
				UID:        "94751738-d4f8-4712-9192-e473a7c98d4b",
				Controller: &controller,
			}},
		},
	}
	if !copyOwnerReferences(desired, existing) {
		t.Fatal("expected ownerReference copy to report a change")
	}
	if got := existing.OwnerReferences[0].APIVersion; got != "kubeflow.org/v1" {
		t.Fatalf("ownerReference.apiVersion = %q, want kubeflow.org/v1", got)
	}
}
