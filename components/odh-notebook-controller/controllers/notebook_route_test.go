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
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("HTTPRoute Name Generation", func() {
	Context("generateHTTPRouteName function", func() {
		It("should use direct format for short names", func() {
			// Short namespace and notebook names
			routeName := generateHTTPRouteName("default", "notebook")
			Expect(routeName).To(Equal("nb-default-notebook"))
			Expect(len(routeName)).To(BeNumerically("<=", HTTPRouteSubDomainMaxLen))
		})

		It("should use direct format when exactly at 63-char limit", func() {
			// Create namespace that results in exactly 63 chars
			// Format: nb-{ns}-{nb} = 3 + ns + 1 + nb
			// For notebook (8 chars): 3 + ns + 1 + 8 = ns + 12
			// To get 63: ns = 51
			namespace := "ns-" + createStringOfLength(48) // 51 chars total
			routeName := generateHTTPRouteName(namespace, "notebook")

			Expect(len(routeName)).To(Equal(63))
			Expect(routeName).To(Equal("nb-" + namespace + "-notebook"))
		})

		It("should use hash format when name exceeds 63 chars (RHOAIENG-49720)", func() {
			// This is the bug scenario - namespace > 50 chars with typical notebook name
			namespace := "ns-" + createStringOfLength(49) // 52 chars, exceeds boundary
			routeName := generateHTTPRouteName(namespace, "notebook")

			// Should use hash format
			Expect(routeName).To(HavePrefix("nb-"))
			Expect(len(routeName)).To(Equal(15)) // nb- + 12-char hash
			Expect(routeName).NotTo(ContainSubstring(namespace))
		})

		It("should use hash format for very long namespace and notebook names", func() {
			namespace := "very-long-namespace-name-that-exceeds-fifty-four-characters-limit"
			notebook := "my-workbench-with-long-name"

			routeName := generateHTTPRouteName(namespace, notebook)

			// Should use hash format
			Expect(routeName).To(HavePrefix("nb-"))
			Expect(len(routeName)).To(Equal(15)) // nb- + 12-char hash
			Expect(len(routeName)).To(BeNumerically("<=", HTTPRouteSubDomainMaxLen))
		})

		It("should be deterministic - same inputs produce same outputs (RHOAIENG-49720)", func() {
			namespace := "test-namespace-with-fifty-five-characters-exactly-here"
			notebook := "workbench"

			routeName1 := generateHTTPRouteName(namespace, notebook)
			routeName2 := generateHTTPRouteName(namespace, notebook)
			routeName3 := generateHTTPRouteName(namespace, notebook)

			Expect(routeName1).To(Equal(routeName2))
			Expect(routeName2).To(Equal(routeName3))
		})

		It("should produce unique names for different namespace/notebook combinations", func() {
			names := make(map[string]bool)

			testCases := []struct {
				namespace string
				notebook  string
			}{
				{"namespace-a-very-long-name-that-exceeds-sixty-three", "notebook"},
				{"namespace-a-very-long-name-that-exceeds-sixty-three", "notebook2"},
				{"namespace-b-very-long-name-that-exceeds-sixty-three", "notebook"},
				{"namespace-a-very-long-name-that-exceeds-sixty-four", "notebook"},
			}

			for _, tc := range testCases {
				routeName := generateHTTPRouteName(tc.namespace, tc.notebook)
				Expect(names[routeName]).To(BeFalse(), "Hash collision detected for %s/%s", tc.namespace, tc.notebook)
				names[routeName] = true
			}

			Expect(names).To(HaveLen(len(testCases)))
		})

		It("should always stay under 63-character limit", func() {
			// Test various extreme cases
			testCases := []struct {
				namespace string
				notebook  string
			}{
				{"short", "nb"},
				{"a", "a"},
				{createStringOfLength(63), "notebook"},
				{createStringOfLength(100), createStringOfLength(50)},
				{createStringOfLength(253), createStringOfLength(253)}, // Max DNS label
			}

			for _, tc := range testCases {
				routeName := generateHTTPRouteName(tc.namespace, tc.notebook)
				Expect(len(routeName)).To(BeNumerically("<=", HTTPRouteSubDomainMaxLen),
					"Route name too long for ns=%s, nb=%s", tc.namespace, tc.notebook)
			}
		})

		It("should handle namespace and notebook with special characters", func() {
			// Kubernetes allows alphanumeric + dash
			namespace := "test-namespace-123"
			notebook := "my-notebook-456"

			routeName := generateHTTPRouteName(namespace, notebook)
			Expect(routeName).To(Equal("nb-test-namespace-123-my-notebook-456"))
		})

		It("should handle the exact boundary case from bug report (54-char namespace)", func() {
			// From the bug report: "54 characters do not work and 53 do"
			// With 8-char notebook name:
			// - 53-char namespace: 3 + 53 + 1 + 8 = 65 chars (exceeds, uses hash)
			// - 54-char namespace: 3 + 54 + 1 + 8 = 66 chars (exceeds, uses hash)

			// Test with actual 53 and 54 char namespaces
			routeName53 := generateHTTPRouteName(createStringOfLength(53), "notebook")
			routeName54 := generateHTTPRouteName(createStringOfLength(54), "notebook")

			// Both should use hash (exceed 63)
			Expect(len(routeName53)).To(Equal(15))
			Expect(len(routeName54)).To(Equal(15))

			// Verify they produce different hashes (different inputs)
			Expect(routeName53).NotTo(Equal(routeName54))
		})
	})
})

// Helper function to create a string of specific length
func createStringOfLength(length int) string {
	if length <= 0 {
		return ""
	}
	result := make([]byte, length)
	for i := 0; i < length; i++ {
		result[i] = 'a'
	}
	return string(result)
}
