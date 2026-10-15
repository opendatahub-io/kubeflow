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
	"strings"
	"testing"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolveProxyTLSConfig_NoProfile(t *testing.T) {
	config, err := ResolveProxyTLSConfig(configv1.TLSProfileSpec{}, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12, got %s", config.MinVersion)
	}
	if len(config.CipherSuites) == 0 {
		t.Error("expected non-empty cipher suites for Intermediate defaults")
	}
	// Verify no TLS 1.3 ciphers in the list
	for _, c := range config.CipherSuites {
		if strings.HasPrefix(c, "TLS_AES_") || strings.HasPrefix(c, "TLS_CHACHA20_") {
			t.Errorf("TLS 1.3 cipher should not be present: %s", c)
		}
	}
}

func TestResolveProxyTLSConfig_AdherenceNoOpinion(t *testing.T) {
	profile := *configv1.TLSProfiles[configv1.TLSProfileModernType]
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyNoOpinion, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// NoOpinion → Intermediate defaults, not the Modern profile
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12 (Intermediate default), got %s", config.MinVersion)
	}
}

func TestResolveProxyTLSConfig_AdherenceLegacy(t *testing.T) {
	profile := *configv1.TLSProfiles[configv1.TLSProfileModernType]
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Legacy → Intermediate defaults
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12 (Intermediate default), got %s", config.MinVersion)
	}
}

func TestResolveProxyTLSConfig_StrictIntermediate(t *testing.T) {
	profile := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12, got %s", config.MinVersion)
	}
	if len(config.CipherSuites) == 0 {
		t.Error("expected non-empty cipher suites for Intermediate profile")
	}
	// Should contain ECDHE ciphers
	found := false
	for _, c := range config.CipherSuites {
		if c == "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256" {
			found = true
		}
	}
	if !found {
		t.Error("expected TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 in cipher list")
	}
}

func TestResolveProxyTLSConfig_StrictModern(t *testing.T) {
	profile := *configv1.TLSProfiles[configv1.TLSProfileModernType]
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.MinVersion != "VersionTLS13" {
		t.Errorf("expected VersionTLS13, got %s", config.MinVersion)
	}
	// TLS 1.3 → no cipher suites (Go enforces natively)
	if len(config.CipherSuites) != 0 {
		t.Errorf("expected empty cipher suites for TLS 1.3, got %v", config.CipherSuites)
	}
}

func TestResolveProxyTLSConfig_StrictOld(t *testing.T) {
	profile := *configv1.TLSProfiles[configv1.TLSProfileOldType]
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.MinVersion != "VersionTLS10" {
		t.Errorf("expected VersionTLS10, got %s", config.MinVersion)
	}
	if len(config.CipherSuites) == 0 {
		t.Error("expected non-empty cipher suites for Old profile")
	}
}

func TestResolveProxyTLSConfig_StrictCustom(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		Ciphers:       []string{"ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-RSA-AES256-GCM-SHA384"},
		MinTLSVersion: configv1.VersionTLS12,
		Groups:        []configv1.TLSGroup{configv1.TLSGroupX25519, configv1.TLSGroupSecP256r1},
	}
	config, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12, got %s", config.MinVersion)
	}
	if len(config.CipherSuites) != 2 {
		t.Errorf("expected 2 cipher suites, got %d: %v", len(config.CipherSuites), config.CipherSuites)
	}
	if len(config.CurvePreferences) != 2 {
		t.Errorf("expected 2 curve preferences, got %d: %v", len(config.CurvePreferences), config.CurvePreferences)
	}
	if config.CurvePreferences[0] != "29" {
		t.Errorf("expected X25519=29, got %s", config.CurvePreferences[0])
	}
	if config.CurvePreferences[1] != "23" {
		t.Errorf("expected secp256r1=23, got %s", config.CurvePreferences[1])
	}
}

func TestResolveProxyTLSConfig_StrictInvalidCiphers(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		Ciphers:       []string{"UNKNOWN-CIPHER-1", "UNKNOWN-CIPHER-2"},
		MinTLSVersion: configv1.VersionTLS12,
	}
	_, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err == nil {
		t.Fatal("expected error for strict profile with no usable ciphers")
	}
	if !strings.Contains(err.Error(), "none are supported") {
		t.Errorf("expected 'none are supported' in error, got: %v", err)
	}
}

func TestResolveProxyTLSConfig_StrictInvalidGroups(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		Ciphers:       []string{"ECDHE-ECDSA-AES128-GCM-SHA256"},
		MinTLSVersion: configv1.VersionTLS12,
		Groups:        []configv1.TLSGroup{"unsupported-group"},
	}
	_, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err == nil {
		t.Fatal("expected error for strict profile with no usable groups")
	}
	if !strings.Contains(err.Error(), "none are supported") {
		t.Errorf("expected 'none are supported' in error, got: %v", err)
	}
}

func TestResolveProxyTLSConfig_StrictEmptyMinVersion(t *testing.T) {
	profile := configv1.TLSProfileSpec{
		Ciphers:       []string{"ECDHE-ECDSA-AES128-GCM-SHA256"},
		MinTLSVersion: "",
	}
	_, err := ResolveProxyTLSConfig(profile, configv1.TLSAdherencePolicyStrictAllComponents, true)
	if err == nil {
		t.Fatal("expected error for strict profile with empty min version")
	}
}

func TestResolveProxyTLSConfig_UnknownAdherence(t *testing.T) {
	// Unknown adherence → treat as strict
	profile := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	config, err := ResolveProxyTLSConfig(profile, "SomeFuturePolicy", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should use the actual profile, not Intermediate defaults
	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12, got %s", config.MinVersion)
	}
}

func TestConvertCiphers_TLS13OmitsCiphers(t *testing.T) {
	ciphers := convertCiphers(
		[]string{"TLS_AES_128_GCM_SHA256", "ECDHE-ECDSA-AES128-GCM-SHA256"},
		configv1.VersionTLS13,
	)
	if len(ciphers) != 0 {
		t.Errorf("expected empty ciphers for TLS 1.3, got %v", ciphers)
	}
}

func TestConvertCiphers_TLS12FiltersTLS13(t *testing.T) {
	ciphers := convertCiphers(
		[]string{
			"TLS_AES_128_GCM_SHA256",        // TLS 1.3 — should be filtered
			"ECDHE-ECDSA-AES128-GCM-SHA256", // TLS 1.2 — should be kept
			"ECDHE-RSA-AES256-GCM-SHA384",   // TLS 1.2 — should be kept
		},
		configv1.VersionTLS12,
	)
	if len(ciphers) != 2 {
		t.Fatalf("expected 2 ciphers, got %d: %v", len(ciphers), ciphers)
	}
	if ciphers[0] != "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256" {
		t.Errorf("expected TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, got %s", ciphers[0])
	}
	if ciphers[1] != "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384" {
		t.Errorf("expected TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, got %s", ciphers[1])
	}
}

func TestConvertCiphers_DeduplicatesAliases(t *testing.T) {
	ciphers := convertCiphers(
		[]string{
			"ECDHE-ECDSA-CHACHA20-POLY1305",        // alias
			"ECDHE-ECDSA-CHACHA20-POLY1305-SHA256",  // same IANA name
		},
		configv1.VersionTLS12,
	)
	if len(ciphers) != 1 {
		t.Errorf("expected 1 cipher after dedup, got %d: %v", len(ciphers), ciphers)
	}
}

func TestConvertCiphers_DropsUnsupported(t *testing.T) {
	ciphers := convertCiphers(
		[]string{
			"DHE-RSA-AES128-GCM-SHA256",     // Not in Go's crypto/tls
			"ECDHE-ECDSA-AES128-GCM-SHA256", // Supported
		},
		configv1.VersionTLS12,
	)
	if len(ciphers) != 1 {
		t.Errorf("expected 1 cipher (DHE dropped), got %d: %v", len(ciphers), ciphers)
	}
}

func TestConvertGroups(t *testing.T) {
	tests := []struct {
		name     string
		groups   []configv1.TLSGroup
		expected []string
	}{
		{
			name:     "nil groups",
			groups:   nil,
			expected: nil,
		},
		{
			name:     "empty groups",
			groups:   []configv1.TLSGroup{},
			expected: nil,
		},
		{
			name:   "all supported groups",
			groups: []configv1.TLSGroup{configv1.TLSGroupX25519MLKEM768, configv1.TLSGroupX25519, configv1.TLSGroupSecP256r1, configv1.TLSGroupSecP384r1},
			expected: []string{"4588", "29", "23", "24"},
		},
		{
			name:     "secp521r1",
			groups:   []configv1.TLSGroup{configv1.TLSGroupSecP521r1},
			expected: []string{"25"},
		},
		{
			name:   "drops unsupported groups",
			groups: []configv1.TLSGroup{configv1.TLSGroupX25519, "unsupported-group"},
			expected: []string{"29"},
		},
		{
			name:   "PQ hybrid groups not in mapping are dropped",
			groups: []configv1.TLSGroup{configv1.TLSGroupSecP256r1MLKEM768, configv1.TLSGroupSecP384r1MLKEM1024},
			expected: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertGroups(tt.groups)
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d groups, got %d: %v", len(tt.expected), len(got), got)
			}
			for i := range tt.expected {
				if got[i] != tt.expected[i] {
					t.Errorf("group[%d]: expected %s, got %s", i, tt.expected[i], got[i])
				}
			}
		})
	}
}

func TestProxyTLSConfig_Args(t *testing.T) {
	tests := []struct {
		name     string
		config   *ProxyTLSConfig
		expected []string
	}{
		{
			name:     "nil config",
			config:   nil,
			expected: nil,
		},
		{
			name:     "min version only",
			config:   &ProxyTLSConfig{MinVersion: "VersionTLS13"},
			expected: []string{"--tls-min-version=VersionTLS13"},
		},
		{
			name: "full TLS 1.2 config",
			config: &ProxyTLSConfig{
				MinVersion:       "VersionTLS12",
				CipherSuites:     []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
				CurvePreferences: []string{"29", "23"},
			},
			expected: []string{
				"--tls-min-version=VersionTLS12",
				"--tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
				"--tls-curve-preferences=29,23",
			},
		},
		{
			name: "TLS 1.3 no ciphers",
			config: &ProxyTLSConfig{
				MinVersion:       "VersionTLS13",
				CurvePreferences: []string{"4588", "29"},
			},
			expected: []string{
				"--tls-min-version=VersionTLS13",
				"--tls-curve-preferences=4588,29",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.config.Args()
			if len(got) != len(tt.expected) {
				t.Fatalf("expected %d args, got %d: %v", len(tt.expected), len(got), got)
			}
			for i := range tt.expected {
				if got[i] != tt.expected[i] {
					t.Errorf("arg[%d]: expected %q, got %q", i, tt.expected[i], got[i])
				}
			}
		})
	}
}

func TestProxyTLSConfig_Fingerprint(t *testing.T) {
	// Nil config → empty fingerprint
	var nilConfig *ProxyTLSConfig
	if fp := nilConfig.Fingerprint(); fp != "" {
		t.Errorf("expected empty fingerprint for nil config, got %q", fp)
	}

	// Same config → same fingerprint
	c1 := &ProxyTLSConfig{MinVersion: "VersionTLS12", CipherSuites: []string{"A", "B"}}
	c2 := &ProxyTLSConfig{MinVersion: "VersionTLS12", CipherSuites: []string{"A", "B"}}
	if c1.Fingerprint() != c2.Fingerprint() {
		t.Error("expected identical fingerprints for identical configs")
	}

	// Different config → different fingerprint
	c3 := &ProxyTLSConfig{MinVersion: "VersionTLS13"}
	if c1.Fingerprint() == c3.Fingerprint() {
		t.Error("expected different fingerprints for different configs")
	}

	// Fingerprint is hex and 16 chars
	fp := c1.Fingerprint()
	if len(fp) != 16 {
		t.Errorf("expected 16-char fingerprint, got %d: %q", len(fp), fp)
	}
}

func TestInjectKubeRbacProxy_IncludesTLSArgs(t *testing.T) {
	notebook := testNotebookWithAuth()
	tlsConfig := &ProxyTLSConfig{
		MinVersion:       "VersionTLS12",
		CipherSuites:     []string{"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256"},
		CurvePreferences: []string{"29", "23"},
	}
	kubeRbacProxyConfig := KubeRbacProxyConfig{
		ProxyImage: "test-rbac-image",
		TLS:        tlsConfig,
	}

	err := InjectKubeRbacProxy(notebook, kubeRbacProxyConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var proxyContainer *corev1.Container
	for i, c := range notebook.Spec.Template.Spec.Containers {
		if c.Name == ContainerNameKubeRbacProxy {
			proxyContainer = &notebook.Spec.Template.Spec.Containers[i]
			break
		}
	}
	if proxyContainer == nil {
		t.Fatal("kube-rbac-proxy container not found")
	}

	expectedArgs := map[string]bool{
		"--tls-min-version=VersionTLS12":                                       false,
		"--tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256":         false,
		"--tls-curve-preferences=29,23":                                        false,
	}
	for _, arg := range proxyContainer.Args {
		if _, ok := expectedArgs[arg]; ok {
			expectedArgs[arg] = true
		}
	}
	for arg, found := range expectedArgs {
		if !found {
			t.Errorf("expected arg %q not found in container args: %v", arg, proxyContainer.Args)
		}
	}
}

func TestInjectKubeRbacProxy_NilTLS(t *testing.T) {
	notebook := testNotebookWithAuth()
	kubeRbacProxyConfig := KubeRbacProxyConfig{
		ProxyImage: "test-rbac-image",
		TLS:        nil,
	}

	err := InjectKubeRbacProxy(notebook, kubeRbacProxyConfig)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var proxyContainer *corev1.Container
	for i, c := range notebook.Spec.Template.Spec.Containers {
		if c.Name == ContainerNameKubeRbacProxy {
			proxyContainer = &notebook.Spec.Template.Spec.Containers[i]
			break
		}
	}
	if proxyContainer == nil {
		t.Fatal("kube-rbac-proxy container not found")
	}

	// No TLS args should be present
	for _, arg := range proxyContainer.Args {
		if strings.HasPrefix(arg, "--tls-min-version=") ||
			strings.HasPrefix(arg, "--tls-cipher-suites=") ||
			strings.HasPrefix(arg, "--tls-curve-preferences=") {
			t.Errorf("unexpected TLS arg with nil TLS config: %s", arg)
		}
	}
}

func TestIntermediateProxyTLSConfig(t *testing.T) {
	config := intermediateProxyTLSConfig()

	if config.MinVersion != "VersionTLS12" {
		t.Errorf("expected VersionTLS12, got %s", config.MinVersion)
	}

	if len(config.CipherSuites) == 0 {
		t.Error("expected non-empty cipher suites")
	}

	// All ciphers should be IANA names (start with TLS_ECDHE_)
	for _, c := range config.CipherSuites {
		if !strings.HasPrefix(c, "TLS_") {
			t.Errorf("cipher %q is not in IANA format", c)
		}
	}

	// Intermediate profile includes groups
	if len(config.CurvePreferences) == 0 {
		t.Error("expected non-empty curve preferences for Intermediate profile")
	}

	// Verify args are generated correctly
	args := config.Args()
	if len(args) == 0 {
		t.Error("expected non-empty args")
	}
	if !strings.HasPrefix(args[0], "--tls-min-version=") {
		t.Errorf("expected first arg to be --tls-min-version, got %s", args[0])
	}
}

// testNotebookWithAuth creates a minimal notebook for kube-rbac-proxy injection tests.
func testNotebookWithAuth() *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-notebook",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationInjectAuth: "true",
			},
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-notebook",
							Image: "test-image",
						},
					},
				},
			},
		},
	}
}
