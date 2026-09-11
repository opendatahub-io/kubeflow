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

	reconcilehelper "github.com/kubeflow/kubeflow/components/common/reconcilehelper"
	nbv1beta1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1beta1"
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
		currentNb        nbv1beta1.Notebook
		pod              corev1.Pod
		sts              appsv1.StatefulSet
		expectedNbStatus nbv1beta1.NotebookStatus
	}{
		{
			name: "NotebookStatusInitialization",
			currentNb: nbv1beta1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1beta1.NotebookStatus{},
			},
			pod: corev1.Pod{},
			sts: appsv1.StatefulSet{},
			expectedNbStatus: nbv1beta1.NotebookStatus{
				Conditions:     []nbv1beta1.NotebookCondition{},
				ReadyReplicas:  int32(0),
				ContainerState: corev1.ContainerState{},
			},
		},
		{
			name: "NotebookStatusReadyReplicas",
			currentNb: nbv1beta1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1beta1.NotebookStatus{},
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
			expectedNbStatus: nbv1beta1.NotebookStatus{
				Conditions:     []nbv1beta1.NotebookCondition{},
				ReadyReplicas:  int32(1),
				ContainerState: corev1.ContainerState{},
			},
		},
		{
			name: "NotebookContainerState",
			currentNb: nbv1beta1.Notebook{
				ObjectMeta: v1.ObjectMeta{
					Name:      "test",
					Namespace: "kubeflow-user",
				},
				Status: nbv1beta1.NotebookStatus{},
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
			expectedNbStatus: nbv1beta1.NotebookStatus{
				Conditions:    []nbv1beta1.NotebookCondition{},
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
			expectedNbStatus: nbv1beta1.NotebookStatus{
				Conditions: []nbv1beta1.NotebookCondition{
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
			expectedNbStatus: nbv1beta1.NotebookStatus{
				Conditions: []nbv1beta1.NotebookCondition{
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

func TestCopyStatefulSetFieldsPreservesExtraLabels(t *testing.T) {
	one := int32(1)

	tests := []struct {
		name           string
		fromLabels     map[string]string
		toLabels       map[string]string
		wantLabels     map[string]string
		wantUpdate     bool
	}{
		{
			name:       "adds managed labels, preserves extra",
			fromLabels: map[string]string{"managed": "val1"},
			toLabels:   map[string]string{"extra": "keep"},
			wantLabels: map[string]string{"managed": "val1", "extra": "keep"},
			wantUpdate: true,
		},
		{
			name:       "no update when labels already match",
			fromLabels: map[string]string{"managed": "val1"},
			toLabels:   map[string]string{"managed": "val1", "extra": "keep"},
			wantLabels: map[string]string{"managed": "val1", "extra": "keep"},
			wantUpdate: false,
		},
		{
			name:       "updates changed managed label value",
			fromLabels: map[string]string{"managed": "new"},
			toLabels:   map[string]string{"managed": "old", "extra": "keep"},
			wantLabels: map[string]string{"managed": "new", "extra": "keep"},
			wantUpdate: true,
		},
		{
			name:       "from labels nil, to labels preserved",
			fromLabels: nil,
			toLabels:   map[string]string{"extra": "keep"},
			wantLabels: map[string]string{"extra": "keep"},
			wantUpdate: false,
		},
		{
			name:       "to labels nil, initialised from from",
			fromLabels: map[string]string{"managed": "val1"},
			toLabels:   nil,
			wantLabels: map[string]string{"managed": "val1"},
			wantUpdate: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from := &appsv1.StatefulSet{
				ObjectMeta: v1.ObjectMeta{Labels: tt.fromLabels},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{}}},
			}
			to := &appsv1.StatefulSet{
				ObjectMeta: v1.ObjectMeta{Labels: tt.toLabels},
				Spec:       appsv1.StatefulSetSpec{Replicas: &one, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{}}},
			}
			got := reconcilehelper.CopyStatefulSetFields(from, to)
			if got != tt.wantUpdate {
				t.Errorf("CopyStatefulSetFields() = %v, want %v", got, tt.wantUpdate)
			}
			if !reflect.DeepEqual(to.Labels, tt.wantLabels) {
				t.Errorf("Labels = %v, want %v", to.Labels, tt.wantLabels)
			}
		})
	}
}

func TestPodTemplateLabelMergePreservesExtraLabels(t *testing.T) {
	tests := []struct {
		name         string
		desiredPodLabels  map[string]string
		existingPodLabels map[string]string
		wantPodLabels     map[string]string
	}{
		{
			name:              "merges managed, preserves extra",
			desiredPodLabels:  map[string]string{"notebook-name": "nb1", "statefulset": "nb1"},
			existingPodLabels: map[string]string{"notebook-name": "nb1", "statefulset": "nb1", "kueue-queue": "default"},
			wantPodLabels:     map[string]string{"notebook-name": "nb1", "statefulset": "nb1", "kueue-queue": "default"},
		},
		{
			name:              "corrects drifted managed label",
			desiredPodLabels:  map[string]string{"notebook-name": "nb1"},
			existingPodLabels: map[string]string{"notebook-name": "wrong", "kueue-queue": "default"},
			wantPodLabels:     map[string]string{"notebook-name": "nb1", "kueue-queue": "default"},
		},
		{
			name:              "handles nil existing labels",
			desiredPodLabels:  map[string]string{"notebook-name": "nb1"},
			existingPodLabels: nil,
			wantPodLabels:     map[string]string{"notebook-name": "nb1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := tt.existingPodLabels
			for k, v := range tt.desiredPodLabels {
				if existing == nil || existing[k] != v {
					if existing == nil {
						existing = map[string]string{}
					}
					existing[k] = v
				}
			}
			if !reflect.DeepEqual(existing, tt.wantPodLabels) {
				t.Errorf("PodTemplate.Labels = %v, want %v", existing, tt.wantPodLabels)
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
