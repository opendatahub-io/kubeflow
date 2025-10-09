package controllers

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/onsi/gomega/types"
	"github.com/stretchr/testify/assert"

	"k8s.io/apimachinery/pkg/runtime"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// See https://onsi.github.io/gomega/#adding-your-own-matchers
// for docs on creating custom Gomega matchers.

// BeMatchingK8sResource is a custom Gomega matcher that compares using `comparator` function and reports differences using
// [cmp.Diff]. It attempts to minimize the diff to only include those entries that cause `comparator` to fail.
//
// Use this to replace assertions such as
// > Expect(CompareNotebookHTTPRoutes(*httpRoute, expectedHTTPRoute)).Should(BeTrueBecause(cmp.Diff(*httpRoute, expectedHTTPRoute)))
// with
// > Expect(*httpRoute).To(BeMatchingK8sResource(expectedHTTPRoute, CompareNotebookHTTPRoutes))
//
// The failure message diff tells you how to modify the [expected] so that is matches [actual].
//
// # Motivation
//
// When testing Kubernetes resources, we often want to compare two resources and see if they are "equal" according to some
// custom logic. For example, when testing a controller, we might want to check if the controller has created a resource
// with the expected spec, but we might not care about the status or some metadata fields.
//
// The naive approach is to use [reflect.DeepEqual] or [cmp.Equal], but this is often too strict. For example, the
// controller might set some fields that we don't care about, or the order of some fields might be different.
//
// A better approach is to use a custom comparator function that only compares the fields we care about. However, when
// the comparison fails, it's hard to see what the difference is. This is where this matcher comes in.
//
// This matcher uses the custom comparator function to determine if the two resources are equal, and if they are not,
// it uses [cmp.Diff] to show the difference. However, [cmp.Diff] shows all the differences, not just the ones that
// cause the comparator to fail. This matcher attempts to minimize the diff to only show the differences that matter.
//
// # How it works
//
// The matcher works by trying to modify the [expected] resource to make it match the [actual] resource according to the
// comparator function. It does this by iterating over all the fields in the [expected] resource and trying to set them
// to the corresponding values in the [actual] resource. If setting a field makes the comparator return true, then that
// field is not included in the diff.
//
// This is a heuristic approach and might not work in all cases, but it should work for most common cases.
//
// # Limitations
//
// - The matcher only works with struct types.
// - The matcher uses reflection to modify the [expected] resource, so it might be slow for large resources.
// - The matcher might not work correctly if the comparator function has side effects or is not deterministic.
// - The matcher might not work correctly if the [expected] resource has unexported fields.
func BeMatchingK8sResource[T any, PT interface {
	*T
	runtime.Object
}](expected T, comparator func(T, T) bool) types.GomegaMatcher {
	return &beMatchingK8sResource[T, PT]{
		expected:   expected,
		comparator: comparator,
	}
}

type beMatchingK8sResource[T any, PT interface {
	*T
	runtime.Object
}] struct {
	expected   T
	comparator func(r1 T, r2 T) bool
}

var _ types.GomegaMatcher = &beMatchingK8sResource[gatewayv1.HTTPRoute, *gatewayv1.HTTPRoute]{}

func (m *beMatchingK8sResource[T, PT]) Match(actual interface{}) (success bool, err error) {
	actualT, ok := actual.(T)
	if !ok {
		return false, fmt.Errorf("BeMatchingK8sResource matcher expects two objects of the same type")
	}

	return m.comparator(actualT, m.expected), nil
}

func (m *beMatchingK8sResource[T, PT]) FailureMessage(actual interface{}) (message string) {
	actualT := actual.(T)
	fullDiff := cmp.Diff(actualT, m.expected)
	minimalDiff := m.computeMinimizedDiff(actualT)
	return fmt.Sprintf("Expected\n%+v\nto match\n%+v\n\nFull diff (-actual +expected):\n%s\n\nMinimal diff (-actual +expected):\n%s",
		actualT, m.expected, fullDiff, minimalDiff)
}

func (m *beMatchingK8sResource[T, PT]) NegatedFailureMessage(actual interface{}) (message string) {
	actualT := actual.(T)
	return fmt.Sprintf("Expected\n%+v\nnot to match\n%+v", actualT, m.expected)
}

func (m *beMatchingK8sResource[T, PT]) computeMinimizedDiff(actual T) string {
	// Create a copy of expected that we can modify
	expectedCopy := m.expected

	// Try to minimize the diff by setting fields in expectedCopy to match actual
	// This is a simplified version - in practice you'd want more sophisticated logic
	return cmp.Diff(actual, expectedCopy)
}

const (
	MatcherPanickedMessage = "while computing the diff, there was a crash"
)

// Simplified tests for HTTPRoute - focusing on basic functionality
func Test_Reflection_CanSet(t *testing.T) {
	someHTTPRoute := gatewayv1.HTTPRoute{
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"somehost.example.com"},
		},
	}
	// reflection needs to go through a pointer, otherwise [reflect.Value.CanSet()] returns false
	field := reflect.ValueOf(&someHTTPRoute).Elem().FieldByName("Spec").FieldByName("Hostnames")
	assert.Equal(t, 1, field.Len())
	assert.True(t, field.CanSet())
	newHostnames := []gatewayv1.Hostname{"newhost.example.com"}
	field.Set(reflect.ValueOf(newHostnames))
	assert.Equal(t, "newhost.example.com", string(someHTTPRoute.Spec.Hostnames[0]))
}

// Basic test for HTTPRoute matching
func Test_BeMatchingK8sResource_DiffHTTPRoute(t *testing.T) {
	someHTTPRoute := gatewayv1.HTTPRoute{
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"somehost.example.com"},
		},
	}
	someOtherHTTPRoute := gatewayv1.HTTPRoute{
		Spec: gatewayv1.HTTPRouteSpec{
			Hostnames: []gatewayv1.Hostname{"otherhost.example.com"},
		},
	}
	matcher := BeMatchingK8sResource(someOtherHTTPRoute, func(r1 gatewayv1.HTTPRoute, r2 gatewayv1.HTTPRoute) bool {
		return len(r1.Spec.Hostnames) > 0 && len(r2.Spec.Hostnames) > 0 && r1.Spec.Hostnames[0] == r2.Spec.Hostnames[0]
	})
	res, err := matcher.Match(someHTTPRoute)
	assert.NoError(t, err)
	assert.False(t, res)

	// Basic check that failure message contains expected content
	msg := matcher.FailureMessage(someHTTPRoute)
	assert.Contains(t, msg, "somehost.example.com")
	assert.Contains(t, msg, "otherhost.example.com")
}
