/*
Copyright The Kubeflow Authors.

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

package tls

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// apiServer returns an unstructured APIServer object for the "cluster" name;
// a nil spec omits the spec block entirely.
func apiServer(spec map[string]interface{}) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "APIServer",
		"metadata":   map[string]interface{}{"name": apiserverName},
	}}
	if spec != nil {
		obj.Object["spec"] = spec
	}
	return obj
}

const (
	profileTypeCustom  = "Custom"
	ianaECDHERSAAES128 = "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
)

// profileSpec wraps a tlsSecurityProfile value in an APIServer spec.
func profileSpec(profile interface{}) map[string]interface{} {
	return map[string]interface{}{"tlsSecurityProfile": profile}
}

// typedProfile builds a tlsSecurityProfile map with the given type.
func typedProfile(typeName string) map[string]interface{} {
	return map[string]interface{}{"type": typeName}
}

func customProfile(ciphers []interface{}, minTLSVersion string) map[string]interface{} {
	profile := typedProfile(profileTypeCustom)
	profile["custom"] = map[string]interface{}{
		"ciphers":       ciphers,
		"minTLSVersion": minTLSVersion,
	}
	return profile
}

func newFakeClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	return fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
}

// effectiveConfig applies the resolved TLSOpts to a fresh tls.Config and
// returns it, so assertions run against the effective serving configuration.
func effectiveConfig(t *testing.T, result Result) *tls.Config {
	t.Helper()
	cfg := &tls.Config{}
	for _, opt := range result.TLSOpts {
		opt(cfg)
	}
	return cfg
}

func resolve(t *testing.T, client *fake.FakeDynamicClient) (Result, *tls.Config, error) {
	t.Helper()
	result, err := resolveFrom(context.Background(), client)
	return result, effectiveConfig(t, result), err
}

func TestResolveDeniedReadsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", apierrors.NewForbidden(apiServerGVR.GroupResource(), "cluster", errors.New("denied"))},
		{"unauthorized", apierrors.NewUnauthorized("unauthorized")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient()
			client.PrependReactor("get", "apiservers", func(_ k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})

			result, cfg, err := resolve(t, client)
			if err == nil {
				t.Fatal("expected Resolve to fail on denied read, got nil")
			}
			if !apierrors.IsForbidden(err) && !apierrors.IsUnauthorized(err) {
				t.Errorf("expected the denied-read error to propagate, got %v", err)
			}
			if len(result.TLSOpts) != 0 {
				t.Error("expected no TLSOpts to be installed on denied read")
			}
			if cfg.MinVersion != 0 || cfg.CipherSuites != nil {
				t.Error("expected an untouched tls.Config on denied read, got defaults substituted")
			}
		})
	}
}

func TestResolveLenientReadErrorsFallBackToIntermediate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		err              error
		wantAPIAvailable bool
	}{
		{"not found", apierrors.NewNotFound(apiServerGVR.GroupResource(), "cluster"), false},
		{"no resource match", &meta.NoResourceMatchError{PartialResource: apiServerGVR}, false},
		{"service unavailable", apierrors.NewServiceUnavailable("apiserver down"), true},
		{"too many requests", apierrors.NewTooManyRequests("throttled", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newFakeClient()
			client.PrependReactor("get", "apiservers", func(_ k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, tc.err
			})

			result, cfg, err := resolve(t, client)
			if err != nil {
				t.Fatalf("expected lenient handling of %s, got error: %v", tc.name, err)
			}
			if cfg.MinVersion != tls.VersionTLS12 || !slices.Equal(cfg.CipherSuites, intermediateCiphers) {
				t.Errorf("expected Intermediate fallback, got MinVersion=%d CipherSuites=%v", cfg.MinVersion, cfg.CipherSuites)
			}
			if !slices.Equal(cfg.NextProtos, nextProtos) {
				t.Errorf("expected ALPN h2 + http/1.1, got %v", cfg.NextProtos)
			}
			if result.APIAvailable != tc.wantAPIAvailable {
				t.Errorf("expected APIAvailable=%v, got %v", tc.wantAPIAvailable, result.APIAvailable)
			}
		})
	}
}

func TestResolveProfileReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       map[string]interface{}
		wantErrSub string
	}{
		{
			name:       "malformed tlsSecurityProfile shape",
			spec:       profileSpec("not-an-object"),
			wantErrSub: "parsing TLS profile",
		},
		{
			name:       "custom type with nil custom block",
			spec:       profileSpec(typedProfile(profileTypeCustom)),
			wantErrSub: "custom block is nil",
		},
		{
			name: "unknown minTLSVersion",
			spec: profileSpec(customProfile(
				[]interface{}{ianaECDHERSAAES128}, "VersionTLS99")),
			wantErrSub: `unknown minTLSVersion "VersionTLS99"`,
		},
		{
			name: "custom ciphers none supported by Go",
			spec: profileSpec(customProfile(
				[]interface{}{"TLS_BOGUS_CIPHER", "FAKE-CIPHER"}, "VersionTLS12")),
			wantErrSub: "none of the specified ciphers are supported by Go",
		},
		{
			name:       "custom cipher list empty below TLS 1.3",
			spec:       profileSpec(customProfile(nil, "VersionTLS12")),
			wantErrSub: "no cipher suites for minimum TLS version below 1.3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := resolve(t, newFakeClient(apiServer(tc.spec)))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("expected error containing %q, got %q", tc.wantErrSub, err)
			}
			if len(result.TLSOpts) != 0 {
				t.Error("expected no TLSOpts to be installed on profile failure")
			}
		})
	}
}

func TestResolveProfileUnknownTypeUsesIntermediateFallback(t *testing.T) {
	_, cfg, err := resolve(t, newFakeClient(apiServer(profileSpec(typedProfile("Bogus")))))
	if err != nil {
		t.Fatalf("expected Intermediate fallback, got %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS12)
	}
}

func TestResolveOld(t *testing.T) {
	result, cfg, err := resolve(t, newFakeClient(apiServer(profileSpec(typedProfile("Old")))))
	if err != nil {
		t.Fatalf("expected Old to resolve, got error: %v", err)
	}
	if !result.APIAvailable {
		t.Error("expected APIAvailable true")
	}
	if cfg.MinVersion != tls.VersionTLS10 {
		t.Errorf("expected TLS 1.0 minimum, got 0x%04X", cfg.MinVersion)
	}
	if len(cfg.CipherSuites) == 0 {
		t.Fatal("expected an explicit cipher suite list for Old, got none")
	}
	if !slices.Contains(cfg.CipherSuites, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256) {
		t.Errorf("expected ECDHE-RSA-AES128-GCM-SHA256 in the Old cipher list, got %v", cfg.CipherSuites)
	}
	insecureTableSeen := false
	for _, id := range cfg.CipherSuites {
		if !runtimeCipherIDs[id] {
			t.Errorf("cipher 0x%04X is not negotiable by this Go runtime", id)
		}
		if isTLS13CipherSuite(id) {
			t.Errorf("TLS 1.3-only cipher 0x%04X must be excluded below a TLS 1.3 minimum", id)
		}
	}
	// The Old profile's 3DES and RSA key exchange suites only appear in
	// tls.InsecureCipherSuites(); resolving them proves the lookup uses the
	// union of both runtime tables instead of over-pruning.
	for _, cs := range tls.InsecureCipherSuites() {
		if slices.Contains(cfg.CipherSuites, cs.ID) {
			insecureTableSeen = true
			break
		}
	}
	if !insecureTableSeen {
		t.Error("expected suites from tls.InsecureCipherSuites() in the Old cipher list")
	}
	if !slices.Equal(cfg.NextProtos, nextProtos) {
		t.Errorf("expected ALPN h2 + http/1.1, got %v", cfg.NextProtos)
	}
}

func TestResolveModern(t *testing.T) {
	_, cfg, err := resolve(t, newFakeClient(apiServer(profileSpec(typedProfile("Modern")))))
	if err != nil {
		t.Fatalf("expected Modern to resolve, got error: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("expected TLS 1.3 minimum, got 0x%04X", cfg.MinVersion)
	}
	if cfg.CipherSuites != nil {
		t.Errorf("expected nil CipherSuites (Go-managed TLS 1.3 suites), got %v", cfg.CipherSuites)
	}
}

func TestResolveIntermediate(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec map[string]interface{}
	}{
		{"no spec", nil},
		{"nil profile", map[string]interface{}{"tlsSecurityProfile": nil}},
		{"empty profile object", profileSpec(map[string]interface{}{})},
		{"empty type", profileSpec(typedProfile(""))},
		{"intermediate type", profileSpec(typedProfile("Intermediate"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, cfg, err := resolve(t, newFakeClient(apiServer(tc.spec)))
			if err != nil {
				t.Fatalf("expected Intermediate to resolve, got error: %v", err)
			}
			if !result.APIAvailable {
				t.Error("expected APIAvailable true")
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("expected TLS 1.2 minimum, got 0x%04X", cfg.MinVersion)
			}
			if !slices.Equal(cfg.CipherSuites, intermediateCiphers) {
				t.Errorf("expected the intermediate cipher list, got %v", cfg.CipherSuites)
			}
		})
	}
}

func TestResolveCustomCiphers(t *testing.T) {
	t.Run("mixed list keeps usable subset with both name styles", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"TLS_BOGUS_CIPHER",
			ianaECDHERSAAES128,                   // IANA name
			"ECDHE-RSA-CHACHA20-POLY1305-SHA256", // OpenSSL name
		}, "VersionTLS12"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected custom profile to resolve, got error: %v", err)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256}
		if cfg.MinVersion != tls.VersionTLS12 || !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected MinVersion TLS 1.2 and %v, got MinVersion=%d CipherSuites=%v", want, cfg.MinVersion, cfg.CipherSuites)
		}
	})

	t.Run("RC4 names are rejected as unusable", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"TLS_RSA_WITH_RC4_128_SHA",
			ianaECDHERSAAES128,
		}, "VersionTLS10"))

		_, _, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected the mixed list to resolve with the RC4 entry dropped, got error: %v", err)
		}
		specRC4Only := profileSpec(customProfile([]interface{}{"TLS_RSA_WITH_RC4_128_SHA"}, "VersionTLS10"))
		_, _, err = resolve(t, newFakeClient(apiServer(specRC4Only)))
		if err == nil {
			t.Fatal("expected an all-RC4 cipher list to be rejected, got success")
		}
	})

	t.Run("runtime-supported insecure suites are accepted", func(t *testing.T) {
		// The ECDHE RSA 3DES suite is implemented by this runtime, so a
		// Custom profile may name it: every suite this runtime negotiates is
		// admitted except RC4 (RFC 7465).
		spec := profileSpec(customProfile([]interface{}{
			"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA",
			ianaECDHERSAAES128,
		}, "VersionTLS12"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected the mixed list to resolve, got error: %v", err)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
		if !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected %v, got %v", want, cfg.CipherSuites)
		}
	})

	t.Run("3DES-only profile applies the suite to the config", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA",
		}, "VersionTLS12"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected the 3DES-only list to resolve, got error: %v", err)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA}
		if cfg.MinVersion != tls.VersionTLS12 || !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected MinVersion TLS 1.2 and %v, got MinVersion=%d CipherSuites=%v", want, cfg.MinVersion, cfg.CipherSuites)
		}
	})

	t.Run("3DES OpenSSL alias is accepted the same way", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"ECDHE-RSA-DES-CBC3-SHA",
		}, "VersionTLS12"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected the OpenSSL alias to resolve, got error: %v", err)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA}
		if !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected %v, got %v", want, cfg.CipherSuites)
		}
	})

	t.Run("mixed list keeps 3DES and still drops RC4", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA",
			"TLS_RSA_WITH_RC4_128_SHA",
		}, "VersionTLS12"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected the mixed list to resolve with the RC4 entry dropped, got error: %v", err)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA}
		if !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected %v, got %v", want, cfg.CipherSuites)
		}
	})

	t.Run("TLS 1.3-only names excluded below a TLS 1.3 minimum", func(t *testing.T) {
		spec := profileSpec(customProfile([]interface{}{
			"TLS_AES_128_GCM_SHA256",
			ianaECDHERSAAES128,
		}, "VersionTLS11"))

		_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
		if err != nil {
			t.Fatalf("expected custom profile to resolve, got error: %v", err)
		}
		if cfg.MinVersion != tls.VersionTLS11 {
			t.Errorf("expected the configured TLS 1.1 minimum to be honored exactly, got 0x%04X", cfg.MinVersion)
		}
		want := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
		if !slices.Equal(cfg.CipherSuites, want) {
			t.Errorf("expected %v (TLS 1.3-only name dropped), got %v", want, cfg.CipherSuites)
		}
	})

	t.Run("TLS 1.3 minimum ignores cipher list", func(t *testing.T) {
		for _, ciphers := range [][]interface{}{{ianaECDHERSAAES128}, nil} {
			spec := profileSpec(customProfile(ciphers, "VersionTLS13"))

			_, cfg, err := resolve(t, newFakeClient(apiServer(spec)))
			if err != nil {
				t.Fatalf("expected custom profile to resolve, got error: %v", err)
			}
			if cfg.MinVersion != tls.VersionTLS13 || cfg.CipherSuites != nil {
				t.Errorf("expected TLS 1.3 minimum with nil CipherSuites, got MinVersion=%d CipherSuites=%v", cfg.MinVersion, cfg.CipherSuites)
			}
		}
	})
}
