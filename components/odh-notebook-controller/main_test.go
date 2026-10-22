/*
Copyright 2026.

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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"testing"

	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	apiserversResource  = "apiservers"
	configAPIGroup      = "config.openshift.io"
	notACipher          = "not-a-cipher"
	tls13Cipher         = "TLS_AES_128_GCM_SHA256"
	tls12Cipher         = "ECDHE-RSA-AES128-GCM-SHA256"
	rc4Cipher           = "TLS_RSA_WITH_RC4_128_SHA"
	desEDECipher        = "TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA"
	desEDECipherOpenSSL = "ECDHE-RSA-DES-CBC3-SHA"
)

// applyTLSOpts applies TLS option callbacks to a fresh tls.Config the same way
// the manager applies them to its servers.
func applyTLSOpts(t *testing.T, opts []func(*tls.Config)) *tls.Config {
	t.Helper()
	cfg := &tls.Config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func TestBootstrapTLSDeniedReadFailsStartup(t *testing.T) {
	apiservers := schema.GroupResource{Group: configAPIGroup, Resource: apiserversResource}
	cases := map[string]struct {
		getErr     func(call int) error
		wantErrSub string
	}{
		"forbidden profile read stops startup": {
			getErr:     func(int) error { return k8serr.NewForbidden(apiservers, "cluster", errors.New("denied")) },
			wantErrSub: "reading APIServer TLS profile",
		},
		"unauthorized profile read stops startup": {
			getErr:     func(int) error { return k8serr.NewUnauthorized("Unauthorized") },
			wantErrSub: "reading APIServer TLS profile",
		},
		"forbidden adherence read stops startup": {
			getErr: func(call int) error {
				if call > 1 {
					return k8serr.NewForbidden(apiservers, "cluster", errors.New("denied"))
				}
				return nil
			},
			wantErrSub: "reading APIServer TLS adherence policy",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
					calls++
					if err := tc.getErr(calls); err != nil {
						return err
					}
					apiServer := obj.(*configv1.APIServer)
					apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{Type: configv1.TLSProfileIntermediateType}
					return nil
				},
			}).Build()

			boot, err := bootstrapTLS(context.Background(), fakeClient)
			if err == nil {
				t.Fatal("Expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("Expected error to mention %q, got: %v", tc.wantErrSub, err)
			}
			if boot != nil {
				t.Error("Expected no bootstrap result on error")
			}
		})
	}
}

func TestBootstrapTLSFallbacksKeepHardenedDefaults(t *testing.T) {
	cases := map[string]struct {
		getErr               error
		wantOpenShiftAPI     bool
		wantAdherenceFetched bool
	}{
		"non-OpenShift cluster": {
			getErr:               &apimeta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: configAPIGroup, Resource: apiserversResource}},
			wantOpenShiftAPI:     false,
			wantAdherenceFetched: false,
		},
		"APIServer resource not found": {
			getErr:               k8serr.NewNotFound(schema.GroupResource{Group: configAPIGroup, Resource: apiserversResource}, "cluster"),
			wantOpenShiftAPI:     false,
			wantAdherenceFetched: false,
		},
		"transient API error": {
			getErr:               k8serr.NewServiceUnavailable("apiserver unavailable"),
			wantOpenShiftAPI:     true,
			wantAdherenceFetched: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
					return tc.getErr
				},
			}).Build()

			boot, err := bootstrapTLS(context.Background(), fakeClient)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if boot.hasOpenShiftConfigAPI != tc.wantOpenShiftAPI {
				t.Errorf("hasOpenShiftConfigAPI = %v, want %v", boot.hasOpenShiftConfigAPI, tc.wantOpenShiftAPI)
			}
			if boot.adherenceFetched != tc.wantAdherenceFetched {
				t.Errorf("adherenceFetched = %v, want %v", boot.adherenceFetched, tc.wantAdherenceFetched)
			}
			cfg := applyTLSOpts(t, boot.opts)
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS12)
			}
			if !slices.Equal(cfg.CipherSuites, intermediateCiphers) {
				t.Errorf("CipherSuites = %#x, want the intermediate defaults", cfg.CipherSuites)
			}
			if !slices.Equal(cfg.NextProtos, nextProtos) {
				t.Errorf("NextProtos = %v, want %v", cfg.NextProtos, nextProtos)
			}
		})
	}
}

func TestBootstrapTLSUnknownProfileTypeUsesIntermediateFallback(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			obj.(*configv1.APIServer).Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{Type: "Unknown"}
			return nil
		},
	}).Build()

	boot, err := bootstrapTLS(context.Background(), fakeClient)
	if err != nil {
		t.Fatalf("expected Intermediate fallback, got %v", err)
	}
	cfg := applyTLSOpts(t, boot.opts)
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS12)
	}
}

func TestBootstrapTLSSuccess(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
			apiServer := obj.(*configv1.APIServer)
			apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType}
			apiServer.Spec.TLSAdherence = configv1.TLSAdherencePolicyStrictAllComponents
			return nil
		},
	}).Build()

	boot, err := bootstrapTLS(context.Background(), fakeClient)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	if !boot.hasOpenShiftConfigAPI {
		t.Error("Expected the OpenShift config API to be detected")
	}
	if !boot.adherenceFetched {
		t.Error("Expected the TLS adherence policy to be fetched")
	}
	if boot.adherence != configv1.TLSAdherencePolicyStrictAllComponents {
		t.Errorf("adherence = %q, want %q", boot.adherence, configv1.TLSAdherencePolicyStrictAllComponents)
	}
	if boot.profile.MinTLSVersion != configv1.VersionTLS13 {
		t.Errorf("profile minimum = %q, want %q", boot.profile.MinTLSVersion, configv1.VersionTLS13)
	}

	// Modern keeps Go-managed TLS 1.3 suites: no explicit CipherSuites.
	cfg := applyTLSOpts(t, boot.opts)
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS13)
	}
	if len(cfg.CipherSuites) != 0 {
		t.Errorf("CipherSuites = %#x, want none (TLS 1.3 suites are Go-managed)", cfg.CipherSuites)
	}
	if !slices.Equal(cfg.NextProtos, nextProtos) {
		t.Errorf("NextProtos = %v, want %v", cfg.NextProtos, nextProtos)
	}
}

func TestBootstrapTLSEmptyCiphersFollowAdherencePolicy(t *testing.T) {
	cases := map[string]struct {
		policy        configv1.TLSAdherencePolicy
		policyReadErr error
		minVersion    configv1.TLSProtocolVersion
		wantMin       uint16
		wantErr       bool
	}{
		"strict rejects":                {policy: configv1.TLSAdherencePolicyStrictAllComponents, wantErr: true},
		"unknown rejects":               {policy: "unknown", wantErr: true},
		"no opinion accepts":            {policy: configv1.TLSAdherencePolicyNoOpinion},
		"legacy accepts":                {policy: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly},
		"unreadable policy rejects":     {policyReadErr: k8serr.NewServiceUnavailable("policy unavailable"), wantErr: true},
		"TLS 1.3 accepts empty ciphers": {policy: configv1.TLSAdherencePolicyStrictAllComponents, minVersion: configv1.VersionTLS13, wantMin: tls.VersionTLS13},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			calls := 0
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
					calls++
					if calls > 1 && tc.policyReadErr != nil {
						return tc.policyReadErr
					}
					apiServer := obj.(*configv1.APIServer)
					apiServer.Spec.TLSSecurityProfile = &configv1.TLSSecurityProfile{
						Type: configv1.TLSProfileCustomType,
						Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
							MinTLSVersion: tc.minVersion,
						}},
					}
					if apiServer.Spec.TLSSecurityProfile.Custom.MinTLSVersion == "" {
						apiServer.Spec.TLSSecurityProfile.Custom.MinTLSVersion = configv1.VersionTLS12
					}
					apiServer.Spec.TLSAdherence = tc.policy
					return nil
				},
			}).Build()

			boot, err := bootstrapTLS(context.Background(), fakeClient)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected empty TLS <=1.2 cipher list to be rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("expected empty cipher list to be accepted, got: %v", err)
			}
			cfg := applyTLSOpts(t, boot.opts)
			wantMin := tc.wantMin
			if wantMin == 0 {
				wantMin = tls.VersionTLS12
			}
			if cfg.MinVersion != wantMin || len(cfg.CipherSuites) != 0 {
				t.Errorf("expected unrestricted profile with MinVersion=%#x, got MinVersion=%#x CipherSuites=%v", wantMin, cfg.MinVersion, cfg.CipherSuites)
			}
		})
	}
}

func TestValidateProfileSpec(t *testing.T) {
	t.Run("rejects profiles that cannot be honored", func(t *testing.T) {
		cases := map[string]struct {
			profile    configv1.TLSProfileSpec
			wantErrSub string
		}{
			"unknown minimum version": {
				profile:    configv1.TLSProfileSpec{MinTLSVersion: "VersionTLS99"},
				wantErrSub: "VersionTLS99",
			},
			"missing minimum version": {
				profile:    configv1.TLSProfileSpec{},
				wantErrSub: "minimum version must be set",
			},
			"unsupported-only cipher list": {
				profile: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
					Ciphers:       []string{notACipher},
				},
				wantErrSub: notACipher,
			},
			"TLS 1.3-only ciphers with TLS 1.2 minimum": {
				profile: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
					Ciphers:       []string{tls13Cipher},
				},
				wantErrSub: tls13Cipher,
			},
			"RC4-only cipher list": {
				profile: configv1.TLSProfileSpec{
					MinTLSVersion: configv1.VersionTLS12,
					Ciphers:       []string{rc4Cipher},
				},
				wantErrSub: rc4Cipher,
			},
		}
		for name, tc := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := validateProfileSpec(tc.profile, configv1.TLSAdherencePolicyNoOpinion, true); err == nil {
					t.Fatal("Expected an error, got nil")
				} else if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Errorf("Expected error to mention %q, got: %v", tc.wantErrSub, err)
				}
			})
		}
	})

	t.Run("mixed cipher list keeps its usable subset", func(t *testing.T) {
		profile := configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{tls13Cipher, tls12Cipher, notACipher, rc4Cipher},
		}
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		want := []string{tls12Cipher}
		if !slices.Equal(validated.Ciphers, want) {
			t.Errorf("Ciphers = %v, want %v", validated.Ciphers, want)
		}
	})

	t.Run("3DES ECDHE suite is runtime-supported and accepted alone", func(t *testing.T) {
		profile := configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{desEDECipher},
		}
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		if !slices.Equal(validated.Ciphers, []string{desEDECipher}) {
			t.Errorf("Ciphers = %v, want [%s]", validated.Ciphers, desEDECipher)
		}
	})

	t.Run("3DES ECDHE OpenSSL alias is accepted the same way", func(t *testing.T) {
		profile := configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{desEDECipherOpenSSL},
		}
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		if len(validated.Ciphers) != 1 {
			t.Fatalf("Ciphers = %v, want exactly one resolved suite", validated.Ciphers)
		}
	})

	t.Run("mixed list keeps 3DES ECDHE and still drops RC4", func(t *testing.T) {
		profile := configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{desEDECipher, rc4Cipher},
		}
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		if !slices.Contains(validated.Ciphers, desEDECipher) {
			t.Errorf("Ciphers = %v, want to keep %s", validated.Ciphers, desEDECipher)
		}
		for _, cipher := range validated.Ciphers {
			if cipher == rc4Cipher {
				t.Errorf("Ciphers = %v, want %s dropped (RFC 7465)", validated.Ciphers, rc4Cipher)
			}
		}
	})

	t.Run("old profile honors TLS 1.0 minimum and prunes TLS 1.3-only ciphers", func(t *testing.T) {
		validated, err := validateProfileSpec(*configv1.TLSProfiles[configv1.TLSProfileOldType], configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		if validated.MinTLSVersion != configv1.VersionTLS10 {
			t.Errorf("MinTLSVersion = %q, want %q (configured minimum must be honored exactly)", validated.MinTLSVersion, configv1.VersionTLS10)
		}
		for _, cipher := range validated.Ciphers {
			if cipher == tls13Cipher || cipher == "TLS_AES_256_GCM_SHA384" || cipher == "TLS_CHACHA20_POLY1305_SHA256" {
				t.Errorf("TLS 1.3-only cipher %q was not pruned", cipher)
			}
		}
		if len(validated.Ciphers) == 0 {
			t.Error("Expected a non-empty usable cipher list")
		}
	})

	t.Run("modern profile passes through unchanged", func(t *testing.T) {
		profile := *configv1.TLSProfiles[configv1.TLSProfileModernType]
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		if !slices.Equal(validated.Ciphers, profile.Ciphers) {
			t.Errorf("Ciphers = %v, want %v (TLS 1.3 suites are Go-managed)", validated.Ciphers, profile.Ciphers)
		}
	})
}

// TestValidatedProfileAppliedToTLSConfig mirrors how bootstrapTLS assembles
// its TLS options (validation, then the profile callback and next-protos) and
// applies them to a fresh tls.Config, without needing a cluster client.
func TestValidatedProfileAppliedToTLSConfig(t *testing.T) {
	apply := func(t *testing.T, profile configv1.TLSProfileSpec) *tls.Config {
		t.Helper()
		validated, err := validateProfileSpec(profile, configv1.TLSAdherencePolicyNoOpinion, true)
		if err != nil {
			t.Fatalf("Unexpected validation error: %v", err)
		}
		tlsConfigFn, _ := tlspkg.NewTLSConfigFromProfile(validated)
		return applyTLSOpts(t, []func(*tls.Config){tlsConfigFn, func(c *tls.Config) { c.NextProtos = nextProtos }})
	}

	t.Run("custom profile with restricted ciphers", func(t *testing.T) {
		cfg := apply(t, configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{tls12Cipher},
		})
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS12)
		}
		wantSuites := []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}
		if !slices.Equal(cfg.CipherSuites, wantSuites) {
			t.Errorf("CipherSuites = %#x, want %#x", cfg.CipherSuites, wantSuites)
		}
		if !slices.Equal(cfg.NextProtos, nextProtos) {
			t.Errorf("NextProtos = %v, want %v", cfg.NextProtos, nextProtos)
		}
	})

	t.Run("custom profile with only the 3DES ECDHE suite applies it to the config", func(t *testing.T) {
		cfg := apply(t, configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS12,
			Ciphers:       []string{desEDECipher},
		})
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS12)
		}
		wantSuites := []uint16{tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA}
		if !slices.Equal(cfg.CipherSuites, wantSuites) {
			t.Errorf("CipherSuites = %#x, want %#x", cfg.CipherSuites, wantSuites)
		}
	})

	t.Run("old profile honors TLS 1.0 minimum and prunes unusable ciphers", func(t *testing.T) {
		cfg := apply(t, *configv1.TLSProfiles[configv1.TLSProfileOldType])
		if cfg.MinVersion != tls.VersionTLS10 {
			t.Errorf("MinVersion = %#x, want %#x (configured minimum must be honored exactly)", cfg.MinVersion, tls.VersionTLS10)
		}
		if len(cfg.CipherSuites) == 0 {
			t.Fatal("Expected a non-empty pruned cipher suite list")
		}
		for _, id := range cfg.CipherSuites {
			if id >= 0x1300 && id <= 0x13ff {
				t.Errorf("CipherSuites contains TLS 1.3-only suite %#x", id)
			}
		}
	})

	t.Run("modern profile keeps Go-managed TLS 1.3 suites", func(t *testing.T) {
		cfg := apply(t, *configv1.TLSProfiles[configv1.TLSProfileModernType])
		if cfg.MinVersion != tls.VersionTLS13 {
			t.Errorf("MinVersion = %#x, want %#x", cfg.MinVersion, tls.VersionTLS13)
		}
		if len(cfg.CipherSuites) != 0 {
			t.Errorf("CipherSuites = %#x, want none (TLS 1.3 suites are Go-managed)", cfg.CipherSuites)
		}
	})
}
