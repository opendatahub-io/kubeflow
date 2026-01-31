package controllers

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestCompareNotebookHTTPRoutes_IgnoresExtraLabels(t *testing.T) {
	r1 := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"notebook-name":      "nb",
				"notebook-namespace": "ns",
				"route-shard":        "blue",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{},
	}

	r2 := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"notebook-name":      "nb",
				"notebook-namespace": "ns",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{},
	}

	if !CompareNotebookHTTPRoutes(r1, r2) {
		t.Fatalf("expected routes to match even with extra labels")
	}
}

func TestCompareNotebookHTTPRoutes_DetectsManagedLabelDrift(t *testing.T) {
	r1 := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"notebook-name":      "nb",
				"notebook-namespace": "ns",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{},
	}

	r2 := gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"notebook-name":      "nb2",
				"notebook-namespace": "ns",
			},
		},
		Spec: gatewayv1.HTTPRouteSpec{},
	}

	if CompareNotebookHTTPRoutes(r1, r2) {
		t.Fatalf("expected routes to not match when managed label differs")
	}
}
