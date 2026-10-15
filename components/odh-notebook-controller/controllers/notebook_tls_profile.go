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
	"crypto/sha256"
	"fmt"
	"strings"

	configv1 "github.com/openshift/api/config/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var tlsProfileLog = ctrl.Log.WithName("tls-profile-resolver")

// ProxyTLSConfig holds the resolved TLS configuration for kube-rbac-proxy
// sidecars injected into notebook workloads. The configuration is resolved
// from the OpenShift APIServer TLS profile and adherence policy.
type ProxyTLSConfig struct {
	// MinVersion is the minimum TLS version (e.g., "VersionTLS12", "VersionTLS13").
	MinVersion string

	// CipherSuites holds IANA cipher suite names for --tls-cipher-suites.
	// Empty when min version is TLS 1.3 (Go enforces TLS 1.3 suites natively).
	CipherSuites []string

	// CurvePreferences holds numeric Go CurveIDs for --tls-curve-preferences.
	CurvePreferences []string
}

// openSSLToIANA maps OpenSSL cipher names (as used in OpenShift TLS profiles)
// to IANA/Go cipher suite names (as accepted by kube-rbac-proxy --tls-cipher-suites).
// TLS 1.3 ciphers (TLS_AES_*) are intentionally excluded: Go's TLS 1.3
// implementation ignores CipherSuites and always uses its built-in suites.
var openSSLToIANA = map[string]string{
	"ECDHE-ECDSA-AES128-GCM-SHA256":        "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
	"ECDHE-RSA-AES128-GCM-SHA256":          "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
	"ECDHE-ECDSA-AES256-GCM-SHA384":        "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
	"ECDHE-RSA-AES256-GCM-SHA384":          "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
	"ECDHE-ECDSA-CHACHA20-POLY1305":        "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
	"ECDHE-RSA-CHACHA20-POLY1305":          "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	"ECDHE-ECDSA-CHACHA20-POLY1305-SHA256": "TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256",
	"ECDHE-RSA-CHACHA20-POLY1305-SHA256":   "TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256",
	"ECDHE-ECDSA-AES128-SHA256":            "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256",
	"ECDHE-RSA-AES128-SHA256":              "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256",
	"ECDHE-ECDSA-AES128-SHA":               "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
	"ECDHE-RSA-AES128-SHA":                 "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
	"ECDHE-ECDSA-AES256-SHA384":            "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384",
	"ECDHE-RSA-AES256-SHA384":              "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384",
	"ECDHE-ECDSA-AES256-SHA":               "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA",
	"ECDHE-RSA-AES256-SHA":                 "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
	"AES128-GCM-SHA256":                    "TLS_RSA_WITH_AES_128_GCM_SHA256",
	"AES256-GCM-SHA384":                    "TLS_RSA_WITH_AES_256_GCM_SHA384",
	"AES128-SHA256":                        "TLS_RSA_WITH_AES_128_CBC_SHA256",
	"AES256-SHA256":                        "TLS_RSA_WITH_AES_256_CBC_SHA256",
	"AES128-SHA":                           "TLS_RSA_WITH_AES_128_CBC_SHA",
	"AES256-SHA":                           "TLS_RSA_WITH_AES_256_CBC_SHA",
}

// tlsGroupToGoID maps OpenShift TLS group names to numeric Go CurveIDs
// as required by kube-rbac-proxy --tls-curve-preferences.
// These are the IANA-assigned values from the "TLS Supported Groups" registry.
var tlsGroupToGoID = map[configv1.TLSGroup]string{
	configv1.TLSGroupSecP256r1:       "23",
	configv1.TLSGroupSecP384r1:       "24",
	configv1.TLSGroupSecP521r1:       "25",
	configv1.TLSGroupX25519:          "29",
	configv1.TLSGroupX25519MLKEM768:  "4588",
}

// isTLS13Cipher returns true for TLS 1.3 cipher suite names.
// TLS 1.3 ciphers in the OpenShift profile start with "TLS_" and are not in the
// openSSLToIANA map (which only holds TLS 1.2 ciphers).
func isTLS13Cipher(name string) bool {
	return strings.HasPrefix(name, "TLS_")
}

// intermediateProxyTLSConfig returns the hardened Intermediate defaults.
// Used when the APIServer profile is missing, adherence is NoOpinion, or
// adherence is LegacyAdheringComponentsOnly.
func intermediateProxyTLSConfig() *ProxyTLSConfig {
	profile := configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	config := &ProxyTLSConfig{
		MinVersion: string(profile.MinTLSVersion),
	}
	config.CipherSuites = convertCiphers(profile.Ciphers, profile.MinTLSVersion)
	config.CurvePreferences = convertGroups(profile.Groups)
	return config
}

// ResolveProxyTLSConfig resolves the effective TLS configuration for
// kube-rbac-proxy sidecars based on the OpenShift APIServer TLS profile and
// adherence policy.
//
// Policy resolution (per RHOAIENG-95577):
//  1. Missing APIServer or profile: use hardened Intermediate defaults.
//  2. NoOpinion or LegacyAdheringComponentsOnly: use hardened Intermediate defaults.
//  3. StrictAllComponents: use the configured profile.
//  4. Unknown adherence value: treat as strict.
//  5. Invalid strict profile (e.g. no usable ciphers or groups): return error.
func ResolveProxyTLSConfig(
	profile configv1.TLSProfileSpec,
	adherence configv1.TLSAdherencePolicy,
	hasProfile bool,
) (*ProxyTLSConfig, error) {
	if !hasProfile {
		tlsProfileLog.Info("No TLS profile available, using hardened Intermediate defaults for proxy")
		return intermediateProxyTLSConfig(), nil
	}

	switch adherence {
	case configv1.TLSAdherencePolicyNoOpinion, configv1.TLSAdherencePolicyLegacyAdheringComponentsOnly:
		tlsProfileLog.Info("TLS adherence is NoOpinion or LegacyAdheringComponentsOnly, using hardened Intermediate defaults for proxy",
			"adherence", adherence)
		return intermediateProxyTLSConfig(), nil

	case configv1.TLSAdherencePolicyStrictAllComponents:
		tlsProfileLog.Info("TLS adherence is StrictAllComponents, applying configured profile to proxy")
		return resolveStrictProfile(profile)

	default:
		tlsProfileLog.Info("Unknown TLS adherence value, treating as strict", "adherence", adherence)
		return resolveStrictProfile(profile)
	}
}

// resolveStrictProfile converts a strict TLS profile into proxy CLI args.
// Returns an error if the profile is invalid or unrepresentable:
//   - No usable TLS 1.2 ciphers when min version < TLS 1.3
//   - Groups specified but none are supported
func resolveStrictProfile(profile configv1.TLSProfileSpec) (*ProxyTLSConfig, error) {
	minVersion := string(profile.MinTLSVersion)
	if minVersion == "" {
		return nil, fmt.Errorf("strict TLS profile has empty MinTLSVersion")
	}

	ciphers := convertCiphers(profile.Ciphers, profile.MinTLSVersion)

	// For TLS 1.2, we must have at least one usable cipher
	if profile.MinTLSVersion != configv1.VersionTLS13 && len(ciphers) == 0 && len(profile.Ciphers) > 0 {
		return nil, fmt.Errorf(
			"strict TLS profile specifies ciphers but none are supported by Go for TLS 1.2: %v",
			profile.Ciphers,
		)
	}

	curves := convertGroups(profile.Groups)

	// If groups were specified but none are usable, fail
	if len(profile.Groups) > 0 && len(curves) == 0 {
		return nil, fmt.Errorf(
			"strict TLS profile specifies groups but none are supported: %v",
			profile.Groups,
		)
	}

	return &ProxyTLSConfig{
		MinVersion:       minVersion,
		CipherSuites:     ciphers,
		CurvePreferences: curves,
	}, nil
}

// convertCiphers converts OpenShift cipher names to IANA cipher suite names
// suitable for kube-rbac-proxy --tls-cipher-suites.
// TLS 1.3 ciphers are excluded because Go enforces TLS 1.3 suites natively.
// When minVersion is TLS 1.3, no ciphers are returned.
func convertCiphers(profileCiphers []string, minVersion configv1.TLSProtocolVersion) []string {
	// Go's TLS 1.3 implementation always uses its built-in cipher suites;
	// tls.Config.CipherSuites has no effect for TLS 1.3 connections.
	if minVersion == configv1.VersionTLS13 {
		tlsProfileLog.Info("TLS 1.3 minimum: cipher list omitted (Go enforces TLS 1.3 suites natively)")
		return nil
	}

	var ciphers []string
	seen := make(map[string]bool)
	for _, name := range profileCiphers {
		// Skip TLS 1.3 ciphers
		if isTLS13Cipher(name) {
			continue
		}
		iana, ok := openSSLToIANA[name]
		if !ok {
			tlsProfileLog.Info("Dropping unsupported cipher from proxy TLS profile", "cipher", name)
			continue
		}
		// Deduplicate (some OpenSSL aliases map to the same IANA name)
		if seen[iana] {
			continue
		}
		seen[iana] = true
		ciphers = append(ciphers, iana)
	}
	return ciphers
}

// convertGroups converts OpenShift TLS group names to numeric Go CurveIDs.
// Unsupported or unknown groups are dropped with a log message.
func convertGroups(groups []configv1.TLSGroup) []string {
	if len(groups) == 0 {
		return nil
	}
	var ids []string
	for _, g := range groups {
		id, ok := tlsGroupToGoID[g]
		if !ok {
			tlsProfileLog.Info("Dropping unsupported group from proxy TLS profile", "group", g)
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// Args returns the kube-rbac-proxy CLI arguments for this TLS configuration.
func (c *ProxyTLSConfig) Args() []string {
	if c == nil {
		return nil
	}
	var args []string

	if c.MinVersion != "" {
		args = append(args, "--tls-min-version="+c.MinVersion)
	}

	if len(c.CipherSuites) > 0 {
		args = append(args, "--tls-cipher-suites="+strings.Join(c.CipherSuites, ","))
	}

	if len(c.CurvePreferences) > 0 {
		args = append(args, "--tls-curve-preferences="+strings.Join(c.CurvePreferences, ","))
	}

	return args
}

// Fingerprint returns a short, stable hash of the TLS configuration.
// Used by the reconciler to detect when existing notebook sidecar args
// are stale after a TLS profile or adherence policy change.
func (c *ProxyTLSConfig) Fingerprint() string {
	if c == nil {
		return ""
	}
	args := c.Args()
	h := sha256.Sum256([]byte(strings.Join(args, "\n")))
	return fmt.Sprintf("%x", h[:8])
}
