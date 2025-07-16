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
	"testing"

	v1 "k8s.io/api/core/v1"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
)

func TestFirstDifferenceReporter(t *testing.T) {
	for _, tt := range []struct {
		name string
		a    any
		b    any
		diff string
	}{
		{"", 42, 42, ""},
		{"", v1.Pod{Spec: v1.PodSpec{NodeName: "node1"}}, v1.Pod{Spec: v1.PodSpec{NodeName: "node2"}}, "{v1.Pod}.Spec.NodeName: node1 != node2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var reporter FirstDifferenceReporter
			eq := cmp.Equal(tt.a, tt.b, cmp.Reporter(&reporter))
			assert.Equal(t, tt.diff == "", eq)
			assert.Equal(t, tt.diff, reporter.String())
		})
	}
}

func TestGetStructDiff(t *testing.T) {
	var tests = []struct {
		name     string
		a        any
		b        any
		expected string
	}{
		{"simple numbers", 42, 42, ""},
		{"differing pods", v1.Pod{Spec: v1.PodSpec{NodeName: "node1"}}, v1.Pod{Spec: v1.PodSpec{NodeName: "node2"}}, "{v1.Pod}.Spec.NodeName: node1 != node2"},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			// global context isn´t used here (tests were failing)
			diff := getStructDiff(context.Background(), v.a, v.b)
			assert.Equal(t, diff, v.expected)
		})
	}
}
