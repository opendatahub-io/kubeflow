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
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	pinnedKubeRbacProxyImage = "quay.io/opendatahub/odh-kube-rbac-proxy@sha256:86e7a39ec55433509a48f27db3128c7e4768724a6edd4f3ca4250e7cedc175c0"
	intermediateCipherArg    = "--tls-cipher-suites=TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256," +
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384," +
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256," +
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256"
	intermediateMinVersionArg = "--tls-min-version=VersionTLS12"
	intermediateCurveArg      = "--tls-curve-preferences=4588,29,23,24"
	tls13MinVersionArg        = "--tls-min-version=VersionTLS13"
	customCipherArg           = "--tls-cipher-suites=TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
	filteredCurveArg          = "--tls-curve-preferences=29,25"
	unsupportedCipherName     = "not-a-cipher"
	unsupportedTLSGroup       = configv1.TLSGroup("nope")
	secureListenArg           = "--secure-listen-address=0.0.0.0:8443"
	notebookUpstreamArg       = "--upstream=http://127.0.0.1:8888/"
	tlsCertFileArg            = "--tls-cert-file=/etc/tls/private/tls.crt"
	tlsKeyFileArg             = "--tls-private-key-file=/etc/tls/private/tls.key"
	authHeaderFieldsArg       = "--auth-header-fields-enabled=true"
)

func TestKubeRbacProxyCurveIDs(t *testing.T) {
	want := map[configv1.TLSGroup]tls.CurveID{
		configv1.TLSGroupSecP256r1:      23,
		configv1.TLSGroupSecP384r1:      24,
		configv1.TLSGroupSecP521r1:      25,
		configv1.TLSGroupX25519:         29,
		configv1.TLSGroupX25519MLKEM768: 4588,
	}
	if len(groupCurveIDs) != len(want) {
		t.Fatalf("curve map has %d groups, want %d", len(groupCurveIDs), len(want))
	}
	for group, id := range want {
		if groupCurveIDs[group] != id {
			t.Errorf("%s=%d, want %d", group, groupCurveIDs[group], id)
		}
	}
}

func TestResolveKubeRbacProxyTLS(t *testing.T) {
	withFIPS(t, false)
	intermediateCurves := []string{"4588", "29", "23", "24"}

	t.Run("intermediate fallback", func(t *testing.T) {
		cases := []struct {
			name      string
			profile   *configv1.TLSSecurityProfile
			adherence configv1.TLSAdherencePolicy
		}{
			{name: "nil profile", adherence: configv1.TLSAdherencePolicyStrictAllComponents},
			{name: "empty type", profile: &configv1.TLSSecurityProfile{}, adherence: configv1.TLSAdherencePolicyStrictAllComponents},
			{name: "no opinion modern", profile: modernTLSProfile(), adherence: configv1.TLSAdherencePolicyNoOpinion},
			{name: "legacy modern", profile: modernTLSProfile(), adherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly},
			{name: "legacy invalid custom", profile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType}, adherence: configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resolved, err := resolveKubeRbacProxyTLS(tc.profile, tc.adherence)
				if err != nil {
					t.Fatal(err)
				}
				assertIntermediateTLS(t, resolved)
				if !reflect.DeepEqual(resolved.CurvePreferences, intermediateCurves) {
					t.Fatalf("curves=%v, want %v", resolved.CurvePreferences, intermediateCurves)
				}
			})
		}
	})

	t.Run("missing API uses intermediate", func(t *testing.T) {
		for _, reader := range []client.Reader{nil, emptyAPIServerReader(t), unregisteredAPIServerReader(t)} {
			resolved, err := loadKubeRbacProxyTLS(context.Background(), reader)
			if err != nil {
				t.Fatal(err)
			}
			assertIntermediateTLS(t, resolved)
		}
	})

	t.Run("strict custom", func(t *testing.T) {
		resolved, err := resolveKubeRbacProxyTLS(customTLS12Profile(), configv1.TLSAdherencePolicyStrictAllComponents)
		if err != nil {
			t.Fatal(err)
		}
		assertResolvedArgs(t, resolved, intermediateMinVersionArg, customCipherArg, filteredCurveArg)
		if got := curvePreferenceFlag(resolved.CurvePreferences); got != filteredCurveArg {
			t.Fatalf("filtered curves = %s", got)
		}
	})

	t.Run("tls 1.3 omits cipher flag", func(t *testing.T) {
		for _, adherence := range []configv1.TLSAdherencePolicy{
			configv1.TLSAdherencePolicyStrictAllComponents,
			"SomethingElse",
		} {
			resolved, err := resolveKubeRbacProxyTLS(modernTLSProfile(), adherence)
			if err != nil {
				t.Fatalf("adherence %q: %v", adherence, err)
			}
			assertResolvedArgs(t, resolved, tls13MinVersionArg, intermediateCurveArg)
			if _, ok := flagWithPrefix(resolved.Args, tlsCipherSuitesFlag); ok {
				t.Fatalf("TLS 1.3 args include a cipher flag: %v", resolved.Args)
			}
			if !reflect.DeepEqual(resolved.CurvePreferences, intermediateCurves) {
				t.Fatalf("curves=%v, want %v", resolved.CurvePreferences, intermediateCurves)
			}
		}
	})

	t.Run("strict old stays representable", func(t *testing.T) {
		resolved, err := resolveKubeRbacProxyTLS(
			&configv1.TLSSecurityProfile{Type: configv1.TLSProfileOldType},
			configv1.TLSAdherencePolicyStrictAllComponents,
		)
		if err != nil {
			t.Fatal(err)
		}
		min, ok := flagWithPrefix(resolved.Args, tlsMinVersionFlag)
		if !ok || min != "--tls-min-version=VersionTLS10" {
			t.Fatalf("min version = %q", min)
		}
		ciphers, ok := flagWithPrefix(resolved.Args, tlsCipherSuitesFlag)
		if !ok || !strings.Contains(ciphers, "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256") {
			t.Fatalf("old cipher flag = %q", ciphers)
		}
		if strings.Contains(ciphers, "TLS_AES_") || strings.Contains(ciphers, "TLS_CHACHA20_") {
			t.Fatalf("TLS 1.3 cipher names leaked into the TLS 1.2 flag: %s", ciphers)
		}
	})

	t.Run("strict profile errors", func(t *testing.T) {
		cases := []struct {
			name    string
			profile *configv1.TLSSecurityProfile
		}{
			{name: "unknown type", profile: &configv1.TLSSecurityProfile{Type: "NotAProfile"}},
			{name: "nil custom", profile: &configv1.TLSSecurityProfile{Type: configv1.TLSProfileCustomType}},
			{name: "empty min version", profile: customProfile(configv1.TLSProfileSpec{Ciphers: []string{"ECDHE-RSA-AES128-GCM-SHA256"}})},
			{name: "all ciphers unusable", profile: customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{unsupportedCipherName, "RC4-SHA"},
				Groups:        []configv1.TLSGroup{configv1.TLSGroupSecP256r1},
			})},
			{name: "http2 cipher missing", profile: customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"ECDHE-RSA-AES256-GCM-SHA384"},
				Groups:        []configv1.TLSGroup{configv1.TLSGroupSecP256r1},
			})},
			{name: "all groups unusable", profile: customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS13,
				Groups:        []configv1.TLSGroup{unsupportedTLSGroup, configv1.TLSGroupSecP256r1MLKEM768},
			})},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := resolveKubeRbacProxyTLS(tc.profile, configv1.TLSAdherencePolicyStrictAllComponents); err == nil {
					t.Fatal("expected an error")
				}
			})
		}
	})

	t.Run("empty groups are no opinion", func(t *testing.T) {
		resolved, err := resolveKubeRbacProxyTLS(customProfile(configv1.TLSProfileSpec{
			MinTLSVersion: configv1.VersionTLS13,
		}), configv1.TLSAdherencePolicyStrictAllComponents)
		if err != nil {
			t.Fatal(err)
		}
		if len(resolved.CurvePreferences) != 0 {
			t.Fatalf("curves=%v, want none", resolved.CurvePreferences)
		}
		assertResolvedArgs(t, resolved, tls13MinVersionArg)
	})
}

func TestCurveFilteringDropsUnsupportedAndFIPSGroups(t *testing.T) {
	groups := []configv1.TLSGroup{
		configv1.TLSGroupX25519MLKEM768,
		configv1.TLSGroupX25519,
		unsupportedTLSGroup,
		configv1.TLSGroupSecP256r1MLKEM768,
		configv1.TLSGroupSecP256r1,
		configv1.TLSGroupSecP384r1,
		configv1.TLSGroupSecP521r1,
	}
	withFIPS(t, false)
	ids, err := curvePreferenceIDs(groups, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"4588", "29", "23", "24", "25"}) {
		t.Fatalf("curves=%v", ids)
	}

	withFIPS(t, true)
	ids, err = curvePreferenceIDs(groups, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"23", "24", "25"}) {
		t.Fatalf("FIPS curves=%v", ids)
	}

	if _, err := curvePreferenceIDs([]configv1.TLSGroup{configv1.TLSGroupX25519}, true); err == nil {
		t.Fatal("expected FIPS to reject an X25519-only strict profile")
	}
}

func TestMergeKubeRbacProxyTLSArgsKeepsListenerCertsAndAuth(t *testing.T) {
	base := []string{
		secureListenArg,
		tlsCertFileArg,
		tls13MinVersionArg,
		tlsKeyFileArg,
		"--tls-cipher-suites=OLD",
		"--tls-curve-preferences=23",
		authHeaderFieldsArg,
	}
	got := mergeKubeRbacProxyTLSArgs(base, []string{intermediateMinVersionArg, customCipherArg})
	want := []string{
		secureListenArg,
		tlsCertFileArg,
		tlsKeyFileArg,
		authHeaderFieldsArg,
		intermediateMinVersionArg,
		customCipherArg,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged args = %v, want %v", got, want)
	}
}

func TestOverlayRenderedKubeRbacProxyArgs(t *testing.T) {
	withFIPS(t, false)
	forEachProxyOverlay(t, func(t *testing.T, image string) {
		t.Run("intermediate fallback", func(t *testing.T) {
			nb := &nbv1.Notebook{}
			if err := InjectKubeRbacProxy(nb, KubeRbacProxyConfig{ProxyImage: image}); err != nil {
				t.Fatal(err)
			}
			container := proxyContainer(t, nb)
			if container.Image != image {
				t.Fatalf("image = %s", container.Image)
			}
			assertBaseSidecarContract(t, container)
			resolved, err := intermediateKubeRbacProxyTLS()
			if err != nil {
				t.Fatal(err)
			}
			assertArgsHaveTLS(t, container.Args, resolved)
		})

		t.Run("strict custom", func(t *testing.T) {
			resolved := mustResolve(t, customTLS12Profile(), configv1.TLSAdherencePolicyStrictAllComponents)
			container := injectResolved(t, image, resolved)
			assertBaseSidecarContract(t, container)
			assertArgsHaveTLS(t, container.Args, resolved)
			if got := curvePreferenceFlag(resolved.CurvePreferences); got != filteredCurveArg {
				t.Fatalf("filtered curves = %s", got)
			}
		})

		t.Run("tls 1.3 omits cipher flag", func(t *testing.T) {
			resolved := mustResolve(t, modernTLSProfile(), configv1.TLSAdherencePolicyStrictAllComponents)
			container := injectResolved(t, image, resolved)
			assertBaseSidecarContract(t, container)
			assertArgsHaveTLS(t, container.Args, resolved)
			if _, ok := flagWithPrefix(container.Args, tlsCipherSuitesFlag); ok {
				t.Fatalf("TLS 1.3 sidecar args include a cipher flag: %v", container.Args)
			}
		})

		t.Run("curve filtering", func(t *testing.T) {
			resolved := mustResolve(t, customTLS12Profile(), configv1.TLSAdherencePolicyStrictAllComponents)
			container := injectResolved(t, image, resolved)
			got, ok := flagWithPrefix(container.Args, tlsCurvePreferencesFlag)
			if !ok || got != filteredCurveArg {
				t.Fatalf("sidecar curve flag = %q, want %s", got, filteredCurveArg)
			}
			if got := curvePreferenceFlag(resolved.CurvePreferences); got != filteredCurveArg {
				t.Fatalf("filtered curve flag = %s", got)
			}
		})
	})
}

func TestInvalidStrictProfileLeavesPodTemplateUnchanged(t *testing.T) {
	withFIPS(t, false)
	forEachProxyOverlay(t, func(t *testing.T, image string) {
		profiles := []*configv1.TLSSecurityProfile{
			{Type: "NotAProfile"},
			{Type: configv1.TLSProfileCustomType},
			customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{unsupportedCipherName},
				Groups:        []configv1.TLSGroup{configv1.TLSGroupSecP256r1},
			}),
			customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"ECDHE-RSA-AES256-GCM-SHA384"},
				Groups:        []configv1.TLSGroup{configv1.TLSGroupSecP256r1},
			}),
			customProfile(configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS13,
				Groups:        []configv1.TLSGroup{unsupportedTLSGroup},
			}),
		}
		for _, profile := range profiles {
			nb := proxyNotebook(image, []string{tls13MinVersionArg, notebookUpstreamArg})
			before := nb.DeepCopy()
			server := apiServer(profile, configv1.TLSAdherencePolicyStrictAllComponents)
			reconciler := tlsReconciler(t, nb.DeepCopy(), server)
			if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err == nil {
				t.Fatal("expected strict profile resolution to fail")
			}
			stored := &nbv1.Notebook{}
			if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(nb), stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Spec.Template.Spec, stored.Spec.Template.Spec) {
				t.Fatalf("stored pod template changed: %#v", stored.Spec.Template.Spec)
			}
			if !reflect.DeepEqual(before.Spec.Template.Spec, nb.Spec.Template.Spec) {
				t.Fatal("in-memory pod template changed")
			}

			webhookNotebook := before.DeepCopy()
			webhook := &NotebookWebhook{
				APIReader:           reconciler.APIReader,
				KubeRbacProxyConfig: KubeRbacProxyConfig{ProxyImage: image},
			}
			if err := webhook.injectKubeRbacProxy(context.Background(), webhookNotebook); err == nil {
				t.Fatal("expected admission resolution to fail")
			}
			if !reflect.DeepEqual(before.Spec.Template.Spec, webhookNotebook.Spec.Template.Spec) {
				t.Fatal("webhook mutated the pod template")
			}
		}

		t.Run("denied read", func(t *testing.T) {
			nb := proxyNotebook(image, []string{tls13MinVersionArg})
			before := nb.Spec.Template.Spec.DeepCopy()
			reconciler := tlsReconciler(t, nb.DeepCopy())
			reconciler.APIReader = apiServerReader{get: func(client.Object) error {
				return apierrs.NewForbidden(schema.GroupResource{Group: "config.openshift.io", Resource: "apiservers"}, "cluster", errors.New("denied"))
			}}
			if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err == nil {
				t.Fatal("expected the denied APIServer read to fail")
			}
			if !reflect.DeepEqual(before, &nb.Spec.Template.Spec) {
				t.Fatal("denied read changed the pod template")
			}
		})

		t.Run("transient read", func(t *testing.T) {
			nb := proxyNotebook(image, []string{tls13MinVersionArg})
			before := nb.Spec.Template.Spec.DeepCopy()
			reconciler := tlsReconciler(t, nb.DeepCopy())
			reconciler.APIReader = apiServerReader{get: func(client.Object) error {
				return apierrs.NewServiceUnavailable("unavailable")
			}}
			if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err == nil {
				t.Fatal("expected the transient APIServer read to fail")
			}
			if !reflect.DeepEqual(before, &nb.Spec.Template.Spec) {
				t.Fatal("transient read changed the pod template")
			}
		})
	})
}

func TestProfileOrAdherenceChangeRollsKubeRbacProxy(t *testing.T) {
	withFIPS(t, false)
	forEachProxyOverlay(t, func(t *testing.T, image string) {
		intermediate, err := intermediateKubeRbacProxyTLS()
		if err != nil {
			t.Fatal(err)
		}
		nb := proxyNotebook(image, append([]string{
			secureListenArg,
			notebookUpstreamArg,
			tlsCertFileArg,
			tlsKeyFileArg,
		}, intermediate.Args...))
		server := apiServer(modernTLSProfile(), configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly)
		reconciler := tlsReconciler(t, nb.DeepCopy(), server)

		if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err != nil {
			t.Fatal(err)
		}
		legacySpec := statefulSetFromNotebook(nb)
		if !reflect.DeepEqual(proxyArgs(t, &legacySpec), nb.Spec.Template.Spec.Containers[0].Args) {
			t.Fatal("StatefulSet copy does not match the Notebook pod template")
		}
		if !reflect.DeepEqual(proxyArgs(t, &legacySpec), append([]string{
			secureListenArg,
			notebookUpstreamArg,
			tlsCertFileArg,
			tlsKeyFileArg,
		}, intermediate.Args...)) {
			t.Fatalf("legacy adherence changed proxy args: %v", proxyArgs(t, &legacySpec))
		}

		if err := setAPIServerProfile(reconciler, modernTLSProfile(), configv1.TLSAdherencePolicyStrictAllComponents); err != nil {
			t.Fatal(err)
		}
		if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err != nil {
			t.Fatal(err)
		}
		strictSpec := statefulSetFromNotebook(nb)
		if reflect.DeepEqual(legacySpec, strictSpec) {
			t.Fatal("adherence change did not change the StatefulSet pod template")
		}
		strictArgs := proxyArgs(t, &strictSpec)
		if _, ok := flagWithPrefix(strictArgs, tlsCipherSuitesFlag); ok {
			t.Fatalf("strict Modern args include a cipher flag: %v", strictArgs)
		}
		if _, ok := flagWithPrefix(strictArgs, tls13MinVersionArg); !ok {
			t.Fatalf("strict Modern args = %v", strictArgs)
		}
		if proxyContainerFromSpec(t, &strictSpec).Image != image {
			t.Fatal("profile change replaced the proxy image")
		}

		if err := setAPIServerProfile(reconciler, customTLS12Profile(), configv1.TLSAdherencePolicyStrictAllComponents); err != nil {
			t.Fatal(err)
		}
		if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err != nil {
			t.Fatal(err)
		}
		customSpec := statefulSetFromNotebook(nb)
		if reflect.DeepEqual(strictSpec, customSpec) {
			t.Fatal("profile change did not change the StatefulSet pod template")
		}
		customArgs := proxyArgs(t, &customSpec)
		if _, ok := flagWithPrefix(customArgs, customCipherArg); !ok {
			t.Fatalf("custom args = %v", customArgs)
		}
	})
}

func TestReadErrorDoesNotReplaceAStrictProfileWithIntermediate(t *testing.T) {
	nb := proxyNotebook(pinnedKubeRbacProxyImage, []string{tls13MinVersionArg})
	reconciler := tlsReconciler(t, nb.DeepCopy())
	reconciler.APIReader = apiServerReader{get: func(client.Object) error {
		return apierrs.NewUnauthorized("Unauthorized")
	}}
	if err := reconciler.reconcileKubeRbacProxyTLS(context.Background(), nb); err == nil {
		t.Fatal("expected unauthorized read to fail")
	}
	args := nb.Spec.Template.Spec.Containers[0].Args
	if !reflect.DeepEqual(args, []string{tls13MinVersionArg}) {
		t.Fatalf("args changed to %v", args)
	}
}

func withFIPS(t *testing.T, enabled bool) {
	t.Helper()
	previous := tlsFIPSEnabled
	tlsFIPSEnabled = func() bool { return enabled }
	t.Cleanup(func() { tlsFIPSEnabled = previous })
}

func forEachProxyOverlay(t *testing.T, fn func(t *testing.T, image string)) {
	t.Helper()
	images := make(map[string]string, 2)
	for _, overlay := range []string{"odh", "rhoai"} {
		images[overlay] = overlayKubeRbacProxyImage(t, overlay)
		if images[overlay] != pinnedKubeRbacProxyImage {
			t.Fatalf("%s kube-rbac-proxy image = %s, want %s", overlay, images[overlay], pinnedKubeRbacProxyImage)
		}
	}
	if images["odh"] != images["rhoai"] {
		t.Fatalf("overlay images differ: odh=%s rhoai=%s", images["odh"], images["rhoai"])
	}
	for _, overlay := range []string{"odh", "rhoai"} {
		t.Run(overlay, func(t *testing.T) {
			fn(t, images[overlay])
		})
	}
}

func overlayKubeRbacProxyImage(t *testing.T, overlay string) string {
	t.Helper()
	path := filepath.Join("..", "config", "overlays", overlay, "params.env")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "kube-rbac-proxy="); ok {
			return value
		}
	}
	t.Fatalf("kube-rbac-proxy image not found in %s", path)
	return ""
}

func modernTLSProfile() *configv1.TLSSecurityProfile {
	return &configv1.TLSSecurityProfile{Type: configv1.TLSProfileModernType}
}

func customTLS12Profile() *configv1.TLSSecurityProfile {
	return customProfile(configv1.TLSProfileSpec{
		MinTLSVersion: configv1.VersionTLS12,
		Ciphers: []string{
			"ECDHE-RSA-AES128-GCM-SHA256",
			"TLS_AES_128_GCM_SHA256",
			unsupportedCipherName,
		},
		Groups: []configv1.TLSGroup{
			configv1.TLSGroupX25519,
			unsupportedTLSGroup,
			configv1.TLSGroupSecP256r1MLKEM768,
			configv1.TLSGroupSecP521r1,
		},
	})
}

func customProfile(spec configv1.TLSProfileSpec) *configv1.TLSSecurityProfile {
	return &configv1.TLSSecurityProfile{
		Type: configv1.TLSProfileCustomType,
		Custom: &configv1.CustomTLSProfile{
			TLSProfileSpec: spec,
		},
	}
}

func apiServer(profile *configv1.TLSSecurityProfile, adherence configv1.TLSAdherencePolicy) *configv1.APIServer {
	return &configv1.APIServer{
		ObjectMeta: metav1.ObjectMeta{Name: tlspkg.APIServerName},
		Spec: configv1.APIServerSpec{
			TLSSecurityProfile: profile,
			TLSAdherence:       adherence,
		},
	}
}

func proxyNotebook(image string, args []string) *nbv1.Notebook {
	return &nbv1.Notebook{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "proxy-tls",
			Namespace: "ns",
			Annotations: map[string]string{
				AnnotationInjectAuth: trueString,
			},
		},
		Spec: nbv1.NotebookSpec{
			Template: nbv1.NotebookTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  ContainerNameKubeRbacProxy,
						Image: image,
						Args:  append([]string(nil), args...),
					}},
				},
			},
		},
	}
}

func tlsReconciler(t *testing.T, objects ...client.Object) *OpenshiftNotebookReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := nbv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := configv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &OpenshiftNotebookReconciler{Client: c, APIReader: c, Log: logr.Discard()}
}

func emptyAPIServerReader(t *testing.T) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := configv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

func unregisteredAPIServerReader(t *testing.T) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).Build()
}

type apiServerReader struct {
	get func(client.Object) error
}

func (r apiServerReader) Get(_ context.Context, _ client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	return r.get(obj)
}

func (r apiServerReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("unexpected list")
}

func setAPIServerProfile(reconciler *OpenshiftNotebookReconciler, profile *configv1.TLSSecurityProfile, adherence configv1.TLSAdherencePolicy) error {
	server := &configv1.APIServer{}
	if err := reconciler.APIReader.Get(context.Background(), client.ObjectKey{Name: tlspkg.APIServerName}, server); err != nil {
		return err
	}
	server.Spec.TLSSecurityProfile = profile
	server.Spec.TLSAdherence = adherence
	return reconciler.Update(context.Background(), server)
}

func mustResolve(t *testing.T, profile *configv1.TLSSecurityProfile, adherence configv1.TLSAdherencePolicy) kubeRbacProxyTLS {
	t.Helper()
	resolved, err := resolveKubeRbacProxyTLS(profile, adherence)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func injectResolved(t *testing.T, image string, resolved kubeRbacProxyTLS) corev1.Container {
	t.Helper()
	nb := &nbv1.Notebook{}
	if err := InjectKubeRbacProxy(nb, KubeRbacProxyConfig{ProxyImage: image, TLSArgs: resolved.Args}); err != nil {
		t.Fatal(err)
	}
	return proxyContainer(t, nb)
}

func proxyContainer(t *testing.T, nb *nbv1.Notebook) corev1.Container {
	t.Helper()
	idx := kubeRbacProxyContainerIndex(nb)
	if idx < 0 {
		t.Fatal("kube-rbac-proxy container not found")
	}
	return nb.Spec.Template.Spec.Containers[idx]
}

func statefulSetFromNotebook(nb *nbv1.Notebook) corev1.PodSpec {
	// notebook-controller generateStatefulSet copies Notebook.Spec.Template.Spec
	// onto the StatefulSet pod template. A difference here rolls the proxy.
	sts := appsv1.StatefulSet{
		Spec: appsv1.StatefulSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: *nb.Spec.Template.Spec.DeepCopy(),
			},
		},
	}
	return sts.Spec.Template.Spec
}

func proxyArgs(t *testing.T, spec *corev1.PodSpec) []string {
	t.Helper()
	return proxyContainerFromSpec(t, spec).Args
}

func proxyContainerFromSpec(t *testing.T, spec *corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, container := range spec.Containers {
		if container.Name == ContainerNameKubeRbacProxy {
			return container
		}
	}
	t.Fatal("kube-rbac-proxy container not found")
	return corev1.Container{}
}

func assertIntermediateTLS(t *testing.T, resolved kubeRbacProxyTLS) {
	t.Helper()
	assertResolvedArgs(t, resolved, intermediateMinVersionArg, intermediateCipherArg, intermediateCurveArg)
}

func assertResolvedArgs(t *testing.T, resolved kubeRbacProxyTLS, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(resolved.Args, want) {
		t.Fatalf("args = %v, want %v", resolved.Args, want)
	}
	assertNoDisallowedTLSFlags(t, resolved.Args)
}

func assertArgsHaveTLS(t *testing.T, args []string, resolved kubeRbacProxyTLS) {
	t.Helper()
	for _, want := range resolved.Args {
		if _, ok := flagWithPrefix(args, want); !ok && !containsArg(args, want) {
			t.Fatalf("args %v do not include %s", args, want)
		}
	}
	assertNoDisallowedTLSFlags(t, args)
}

func assertBaseSidecarContract(t *testing.T, container corev1.Container) {
	t.Helper()
	required := []string{
		secureListenArg,
		notebookUpstreamArg,
		"--proxy-endpoints-port=8444",
		"--config-file=/etc/kube-rbac-proxy/config-file.yaml",
		tlsCertFileArg,
		tlsKeyFileArg,
		authHeaderFieldsArg,
		"--auth-header-user-field-name=X-Auth-Request-User",
		"--auth-header-groups-field-name=X-Auth-Request-Groups",
	}
	for _, want := range required {
		if !containsArg(container.Args, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe} {
		if probe == nil || probe.HTTPGet == nil {
			t.Fatal("missing HTTPS health probe")
		}
		if probe.HTTPGet.Path != probeTimeoutHealthPath || probe.HTTPGet.Scheme != corev1.URISchemeHTTPS || probe.HTTPGet.Port.IntValue() != NotebookKubeRbacProxyHealthPort {
			t.Fatalf("health probe = %#v", probe.HTTPGet)
		}
	}
}

func assertNoDisallowedTLSFlags(t *testing.T, args []string) {
	t.Helper()
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--tls-cipher-suite="):
			t.Errorf("singular cipher flag %s", arg)
		case strings.HasPrefix(arg, "--metrics-tls-"):
			t.Errorf("metrics TLS flag %s", arg)
		case strings.HasPrefix(arg, "--tls-min-version=TLS"):
			t.Errorf("TLS version flag must use the Go constant name: %s", arg)
		}
	}
}

func flagWithPrefix(args []string, prefix string) (string, bool) {
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return arg, true
		}
	}
	return "", false
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
