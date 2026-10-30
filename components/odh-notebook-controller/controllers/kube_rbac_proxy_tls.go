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
	"crypto/fips140"
	"crypto/tls"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	nbv1 "github.com/kubeflow/kubeflow/components/notebook-controller/api/v1"
	configv1 "github.com/openshift/api/config/v1"
	tlspkg "github.com/openshift/controller-runtime-common/pkg/tls"
	libgocrypto "github.com/openshift/library-go/pkg/crypto"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	tlsMinVersionFlag       = "--tls-min-version="
	tlsCipherSuitesFlag     = "--tls-cipher-suites="
	tlsCurvePreferencesFlag = "--tls-curve-preferences="
)

// tlsFIPSEnabled reports whether this process is in FIPS mode.
// Tests replace it to cover group filtering without a FIPS runtime.
var tlsFIPSEnabled = fips140.Enabled

// groupCurveIDs maps the OpenShift groups this sidecar can represent to Go
// crypto/tls CurveIDs. Unsupported groups, including the ML-KEM hybrids that
// are not in this map, are dropped.
var groupCurveIDs = map[configv1.TLSGroup]tls.CurveID{
	configv1.TLSGroupSecP256r1:      tls.CurveP256,      // 23
	configv1.TLSGroupSecP384r1:      tls.CurveP384,      // 24
	configv1.TLSGroupSecP521r1:      tls.CurveP521,      // 25
	configv1.TLSGroupX25519:         tls.X25519,         // 29
	configv1.TLSGroupX25519MLKEM768: tls.X25519MLKEM768, // 4588
}

// fipsTLSGroups is the NIST-curve allowlist. X25519 and X25519MLKEM768 are
// not FIPS-approved and are dropped when tlsFIPSEnabled reports true.
var fipsTLSGroups = map[configv1.TLSGroup]struct{}{
	configv1.TLSGroupSecP256r1: {},
	configv1.TLSGroupSecP384r1: {},
	configv1.TLSGroupSecP521r1: {},
}

// runtimeCipherIDs is the set of cipher suite IDs this Go runtime can
// negotiate, excluding RC4. TLS 1.3-only IDs are filtered separately.
var runtimeCipherIDs = func() map[uint16]struct{} {
	ids := make(map[uint16]struct{}, len(tls.CipherSuites())+len(tls.InsecureCipherSuites()))
	for _, cs := range tls.CipherSuites() {
		ids[cs.ID] = struct{}{}
	}
	for _, cs := range tls.InsecureCipherSuites() {
		if !strings.Contains(cs.Name, "RC4") {
			ids[cs.ID] = struct{}{}
		}
	}
	return ids
}()

// kubeRbacProxyTLS is the sidecar TLS configuration resolved from the cluster.
type kubeRbacProxyTLS struct {
	// Args are flags appended to the kube-rbac-proxy container.
	Args []string
	// CurvePreferences are numeric CurveIDs in profile order after dropping
	// unsupported and FIPS-disallowed groups. A non-empty list is also passed
	// as --tls-curve-preferences.
	CurvePreferences []string
}

// kubeRbacProxyAdmissionConfig resolves sidecar TLS flags before admission
// mutates the notebook. An error leaves the notebook unchanged.
func kubeRbacProxyAdmissionConfig(ctx context.Context, reader client.Reader, base KubeRbacProxyConfig) (KubeRbacProxyConfig, error) {
	resolved, err := loadKubeRbacProxyTLS(ctx, reader)
	if err != nil {
		return KubeRbacProxyConfig{}, fmt.Errorf("resolving kube-rbac-proxy TLS settings: %w", err)
	}
	base.TLSArgs = resolved.Args
	return base, nil
}

// loadKubeRbacProxyTLS reads the cluster APIServer and resolves sidecar flags.
// A missing APIServer, or a cluster without the OpenShift config API, uses
// the hardened Intermediate profile. Any other read error is returned so a
// strict cluster is not silently switched to Intermediate.
func loadKubeRbacProxyTLS(ctx context.Context, reader client.Reader) (kubeRbacProxyTLS, error) {
	if reader == nil {
		return intermediateKubeRbacProxyTLS()
	}
	apiServer := &configv1.APIServer{}
	err := reader.Get(ctx, client.ObjectKey{Name: tlspkg.APIServerName}, apiServer)
	if err != nil {
		if openShiftAPIServerUnavailable(err) {
			return intermediateKubeRbacProxyTLS()
		}
		return kubeRbacProxyTLS{}, fmt.Errorf("reading APIServer TLS profile: %w", err)
	}
	return resolveKubeRbacProxyTLS(apiServer.Spec.TLSSecurityProfile, apiServer.Spec.TLSAdherence)
}

func openShiftAPIServerUnavailable(err error) bool {
	if apierrs.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return true
	}
	for err != nil {
		if runtime.IsNotRegisteredError(err) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

// resolveKubeRbacProxyTLS selects Intermediate unless the cluster requires
// every component to honor the configured profile. Unknown adherence is
// treated as strict. A missing profile uses Intermediate regardless of adherence.
func resolveKubeRbacProxyTLS(profile *configv1.TLSSecurityProfile, adherence configv1.TLSAdherencePolicy) (kubeRbacProxyTLS, error) {
	if profile == nil || profile.Type == "" || !useConfiguredTLSProfile(adherence) {
		return intermediateKubeRbacProxyTLS()
	}
	spec, err := strictProfileSpec(profile)
	if err != nil {
		return kubeRbacProxyTLS{}, err
	}
	return tlsProfileSpecToProxyTLS(spec, true)
}

func useConfiguredTLSProfile(adherence configv1.TLSAdherencePolicy) bool {
	switch adherence {
	case configv1.TLSAdherencePolicyNoOpinion, configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly:
		return false
	default:
		return true
	}
}

func strictProfileSpec(profile *configv1.TLSSecurityProfile) (configv1.TLSProfileSpec, error) {
	if profile.Type == configv1.TLSProfileCustomType {
		if profile.Custom == nil {
			return configv1.TLSProfileSpec{}, fmt.Errorf("strict TLS profile cannot be represented: %w", tlspkg.ErrCustomProfileNil)
		}
		return profile.Custom.TLSProfileSpec, nil
	}
	spec, ok := configv1.TLSProfiles[profile.Type]
	if !ok || spec == nil {
		return configv1.TLSProfileSpec{}, fmt.Errorf("strict TLS profile type %q is not representable", profile.Type)
	}
	return *spec, nil
}

func intermediateKubeRbacProxyTLS() (kubeRbacProxyTLS, error) {
	spec := configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	if spec == nil {
		return kubeRbacProxyTLS{}, fmt.Errorf("intermediate TLS profile is not defined")
	}
	return tlsProfileSpecToProxyTLS(*spec, false)
}

func tlsProfileSpecToProxyTLS(spec configv1.TLSProfileSpec, strict bool) (kubeRbacProxyTLS, error) {
	minVersion := string(spec.MinTLSVersion)
	if minVersion == "" {
		return kubeRbacProxyTLS{}, fmt.Errorf("TLS profile minimum version cannot be represented")
	}
	parsed, err := libgocrypto.TLSVersion(minVersion)
	if err != nil {
		return kubeRbacProxyTLS{}, fmt.Errorf("TLS profile minimum version %q cannot be represented: %w", spec.MinTLSVersion, err)
	}

	curves, err := curvePreferenceIDs(spec.Groups, strict)
	if err != nil {
		return kubeRbacProxyTLS{}, err
	}

	args := []string{tlsMinVersionFlag + minVersion}
	if parsed < tls.VersionTLS13 {
		ciphers, cipherErr := tls12CipherNames(spec.Ciphers)
		if cipherErr != nil {
			return kubeRbacProxyTLS{}, cipherErr
		}
		args = append(args, tlsCipherSuitesFlag+strings.Join(ciphers, ","))
	}
	if len(curves) > 0 {
		args = append(args, curvePreferenceFlag(curves))
	}
	return kubeRbacProxyTLS{Args: args, CurvePreferences: curves}, nil
}

func curvePreferenceFlag(ids []string) string {
	return tlsCurvePreferencesFlag + strings.Join(ids, ",")
}

// curvePreferenceIDs maps groups to numeric CurveIDs, dropping unsupported
// and FIPS-disallowed groups. An empty group list is no opinion. Under a
// strict profile, a non-empty list that produces no usable ID cannot be
// represented. The Intermediate fallback omits the curve flag instead.
func curvePreferenceIDs(groups []configv1.TLSGroup, strict bool) ([]string, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	fips := tlsFIPSEnabled()
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		id, ok := groupCurveIDs[group]
		if !ok {
			continue
		}
		if fips {
			if _, allowed := fipsTLSGroups[group]; !allowed {
				continue
			}
		}
		ids = append(ids, strconv.Itoa(int(id)))
	}
	if len(ids) == 0 && strict {
		return nil, fmt.Errorf("strict TLS profile groups cannot be represented")
	}
	return ids, nil
}

func tls12CipherNames(names []string) ([]string, error) {
	ciphers := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		iana, ok := ianaCipherForTLS12(name)
		if !ok {
			continue
		}
		ciphers = append(ciphers, iana)
	}
	if len(ciphers) == 0 {
		return nil, fmt.Errorf("TLS profile has no cipher suite usable with TLS 1.2")
	}
	if !hasHTTP2RequiredCipher(ciphers) {
		return nil, fmt.Errorf("TLS profile cipher list cannot be served by kube-rbac-proxy with HTTP/2: missing an HTTP/2-required AES_128_GCM_SHA256 suite")
	}
	return ciphers, nil
}

func ianaCipherForTLS12(name string) (string, bool) {
	id, err := libgocrypto.CipherSuite(name)
	if err != nil {
		converted := libgocrypto.OpenSSLToIANACipherSuites([]string{name})
		if len(converted) != 1 {
			return "", false
		}
		id, err = libgocrypto.CipherSuite(converted[0])
		if err != nil {
			return "", false
		}
	}
	if !cipherIDUsableForTLS12(id) {
		return "", false
	}
	return tls.CipherSuiteName(id), true
}

func cipherIDUsableForTLS12(id uint16) bool {
	if id >= 0x1300 && id <= 0x13ff {
		return false
	}
	_, ok := runtimeCipherIDs[id]
	return ok
}

func hasHTTP2RequiredCipher(ciphers []string) bool {
	for _, cipher := range ciphers {
		if cipher == "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256" || cipher == "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256" {
			return true
		}
	}
	return false
}

func mergeKubeRbacProxyTLSArgs(args, tlsArgs []string) []string {
	merged := make([]string, 0, len(args)+len(tlsArgs))
	for _, arg := range args {
		if kubeRbacProxyTLSFlag(arg) {
			continue
		}
		merged = append(merged, arg)
	}
	return append(merged, tlsArgs...)
}

func kubeRbacProxyTLSFlag(arg string) bool {
	return strings.HasPrefix(arg, tlsMinVersionFlag) ||
		strings.HasPrefix(arg, tlsCipherSuitesFlag) ||
		strings.HasPrefix(arg, tlsCurvePreferencesFlag)
}

func kubeRbacProxyContainerIndex(notebook *nbv1.Notebook) int {
	containers := notebook.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == ContainerNameKubeRbacProxy {
			return i
		}
	}
	return -1
}

func (r *OpenshiftNotebookReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// reconcileKubeRbacProxyTLS writes the resolved TLS flags onto the Notebook
// pod template. notebook-controller copies that template onto the StatefulSet,
// which rolls the proxy. The ODH controller does not update StatefulSets.
// The proxy itself does not watch APIServer. An error is returned before the
// notebook is updated.
func (r *OpenshiftNotebookReconciler) reconcileKubeRbacProxyTLS(ctx context.Context, notebook *nbv1.Notebook) error {
	if !KubeRbacProxyInjectionIsEnabled(notebook.ObjectMeta) {
		return nil
	}
	resolved, err := loadKubeRbacProxyTLS(ctx, r.apiReader())
	if err != nil {
		return fmt.Errorf("resolving kube-rbac-proxy TLS settings: %w", err)
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &nbv1.Notebook{}
		if err := r.Get(ctx, types.NamespacedName{Name: notebook.Name, Namespace: notebook.Namespace}, current); err != nil {
			return err
		}
		idx := kubeRbacProxyContainerIndex(current)
		if idx < 0 {
			return nil
		}
		updated := mergeKubeRbacProxyTLSArgs(current.Spec.Template.Spec.Containers[idx].Args, resolved.Args)
		if slices.Equal(current.Spec.Template.Spec.Containers[idx].Args, updated) {
			notebook.ResourceVersion = current.ResourceVersion
			return nil
		}
		current.Spec.Template.Spec.Containers[idx].Args = updated
		if err := r.Update(ctx, current); err != nil {
			return err
		}
		if local := kubeRbacProxyContainerIndex(notebook); local >= 0 {
			notebook.Spec.Template.Spec.Containers[local].Args = append([]string(nil), updated...)
		}
		notebook.ResourceVersion = current.ResourceVersion
		r.Log.Info("Updating kube-rbac-proxy TLS arguments to match the cluster security profile",
			"notebook", notebook.Name, "namespace", notebook.Namespace)
		return nil
	})
}
