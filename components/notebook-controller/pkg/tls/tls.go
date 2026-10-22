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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("tls")

// openSSLToGoCipher maps the OpenSSL-style cipher names used by the OpenShift
// API to Go TLS IDs. IANA-style names (Go's suite names) are resolved through
// the runtime tables in cipherIDByName and need no alias here.
var openSSLToGoCipher = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256":        tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":          tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384":        tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":          tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305-SHA256": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305-SHA256":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-CHACHA20-POLY1305":        tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":          tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	// Remaining OpenSSL names from the OpenShift TLS profiles (Mozilla
	// server-side TLS guidelines), mirroring library-go's OpenSSL-to-IANA
	// table for the suites Go can negotiate.
	"ECDHE-ECDSA-AES128-SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256, // tls-lint:ignore
	"ECDHE-RSA-AES128-SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,   // tls-lint:ignore
	"ECDHE-ECDSA-AES128-SHA":    tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,    // tls-lint:ignore
	"ECDHE-RSA-AES128-SHA":      tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,      // tls-lint:ignore
	"ECDHE-ECDSA-AES256-SHA":    tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,    // tls-lint:ignore
	"ECDHE-RSA-AES256-SHA":      tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,      // tls-lint:ignore
	"AES128-GCM-SHA256":         tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	"AES256-GCM-SHA384":         tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	"AES128-SHA256":             tls.TLS_RSA_WITH_AES_128_CBC_SHA256,     // tls-lint:ignore
	"AES128-SHA":                tls.TLS_RSA_WITH_AES_128_CBC_SHA,        // tls-lint:ignore
	"AES256-SHA":                tls.TLS_RSA_WITH_AES_256_CBC_SHA,        // tls-lint:ignore
	"DES-CBC3-SHA":              tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,       // tls-lint:ignore
	"ECDHE-RSA-DES-CBC3-SHA":    tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA, // tls-lint:ignore
}

// oldProfileCiphers is the cipher list of the OpenShift "Old" TLS profile,
// from openshift/api config/v1 TLSProfiles (Mozilla server-side TLS "old"
// guidelines). The TLS 1.3 names are pruned at runtime when the minimum is
// below TLS 1.3: Go negotiates its built-in TLS 1.3 suites regardless.
var oldProfileCiphers = []string{
	"TLS_AES_128_GCM_SHA256",
	"TLS_AES_256_GCM_SHA384",
	"TLS_CHACHA20_POLY1305_SHA256",
	"ECDHE-ECDSA-AES128-GCM-SHA256",
	"ECDHE-RSA-AES128-GCM-SHA256",
	"ECDHE-ECDSA-AES256-GCM-SHA384",
	"ECDHE-RSA-AES256-GCM-SHA384",
	"ECDHE-ECDSA-CHACHA20-POLY1305",
	"ECDHE-RSA-CHACHA20-POLY1305",
	"ECDHE-ECDSA-AES128-SHA256",
	"ECDHE-RSA-AES128-SHA256",
	"ECDHE-ECDSA-AES128-SHA",
	"ECDHE-RSA-AES128-SHA",
	"ECDHE-ECDSA-AES256-SHA",
	"ECDHE-RSA-AES256-SHA",
	"AES128-GCM-SHA256",
	"AES256-GCM-SHA384",
	"AES128-SHA256",
	"AES128-SHA",
	"AES256-SHA",
	"DES-CBC3-SHA",
}

var intermediateCiphers = []uint16{
	tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
}

var tlsVersionMap = map[string]uint16{
	"VersionTLS10": tls.VersionTLS10,
	"VersionTLS11": tls.VersionTLS11,
	"VersionTLS12": tls.VersionTLS12,
	"VersionTLS13": tls.VersionTLS13,
}

var apiServerGVR = schema.GroupVersionResource{
	Group:    "config.openshift.io",
	Version:  "v1",
	Resource: "apiservers",
}

// apiserverName is the name of the singleton APIServer resource that holds
// the cluster TLS security profile.
const apiserverName = "cluster"

// nextProtos enables HTTP/2 with HTTP/1.1 fallback on the TLS servers.
var nextProtos = []string{"h2", "http/1.1"}

// runtimeCipherIDs is the set of cipher suite IDs this Go runtime can
// negotiate, excluding RC4, which RFC 7465 prohibits outright. The union of
// tls.CipherSuites and tls.InsecureCipherSuites is required: the Old
// profile's 3DES and RSA key exchange suites only appear in the insecure
// table. Custom profiles may name any suite in this set; built-in profile
// handling is unaffected because Old, Intermediate, and Modern only name
// secure suites plus the Old profile's legacy list.
var runtimeCipherIDs = func() map[uint16]bool {
	ids := make(map[uint16]bool)
	for _, cs := range tls.CipherSuites() {
		ids[cs.ID] = true
	}
	for _, cs := range tls.InsecureCipherSuites() {
		if !strings.Contains(cs.Name, "RC4") {
			ids[cs.ID] = true
		}
	}
	return ids
}()

// cipherIDByName maps cipher names to Go TLS IDs. Go's suite names are the
// IANA names, so a lookup against the runtime tables doubles as runtime
// validation: a cipher absent from them cannot be negotiated by this binary.
// OpenSSL-style names used by the OpenShift API are kept as aliases.
var cipherIDByName = func() map[string]uint16 {
	m := make(map[string]uint16, len(openSSLToGoCipher))
	for name, id := range openSSLToGoCipher {
		m[name] = id
	}
	for _, cs := range tls.CipherSuites() {
		m[cs.Name] = cs.ID
	}
	for _, cs := range tls.InsecureCipherSuites() {
		if runtimeCipherIDs[cs.ID] {
			m[cs.Name] = cs.ID
		}
	}
	return m
}()

type tlsSecurityProfile struct {
	Type   string `json:"type"`
	Custom *struct {
		Ciphers       []string `json:"ciphers"`
		MinTLSVersion string   `json:"minTLSVersion"`
	} `json:"custom,omitempty"`
}

// Result holds the resolved TLS configuration.
type Result struct {
	TLSOpts      []func(*tls.Config)
	APIAvailable bool
	RawProfile   interface{}
}

// Resolve reads the cluster TLS profile from the APIServer resource using
// dynamic client (no openshift/api dependency) and returns TLS option
// functions for controller-runtime servers.
// On clusters where the API is not available or temporarily unavailable, it
// returns hardened Intermediate defaults. Denied reads, malformed profiles
// and profiles that cannot be honored are returned as errors so controller
// startup fails instead of running with an unknown TLS posture.
func Resolve(ctx context.Context, cfg *rest.Config) (Result, error) {
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("creating dynamic client for TLS profile: %w", err)
	}
	return resolveFrom(ctx, dynClient)
}

// resolveFrom reads the cluster TLS profile via dynClient. It is split from
// Resolve so tests can drive it with a fake dynamic client.
func resolveFrom(ctx context.Context, dynClient dynamic.Interface) (Result, error) {
	var result Result

	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	obj, err := dynClient.Resource(apiServerGVR).Get(resolveCtx, apiserverName, metav1.GetOptions{})
	if err != nil {
		switch {
		case meta.IsNoMatchError(err):
			log.Info("TLS profile API not available (non-OpenShift cluster)")
		case apierrors.IsNotFound(err):
			log.Info("APIServer resource not found, using hardened defaults")
		case apierrors.IsServiceUnavailable(err),
			apierrors.IsTimeout(err),
			apierrors.IsServerTimeout(err),
			apierrors.IsTooManyRequests(err),
			errors.Is(err, context.DeadlineExceeded):
			log.Info("Transient API error reading TLS profile, using hardened defaults", "error", err)
			result.APIAvailable = true
		default:
			// Includes denied reads (IsForbidden/IsUnauthorized): fail closed
			// rather than run with an unverified TLS posture.
			return result, fmt.Errorf("reading APIServer TLS profile: %w", err)
		}
		result.TLSOpts = append(result.TLSOpts, intermediateWithALPN)
		return result, nil
	}

	result.APIAvailable = true
	rawSpec, _, _ := unstructured.NestedMap(obj.Object, "spec", "tlsSecurityProfile")
	result.RawProfile = rawSpec

	profile, err := extractTLSProfile(obj)
	if err != nil {
		return result, fmt.Errorf("parsing TLS profile from APIServer: %w", err)
	}

	minVersion, ciphers, err := parseProfile(profile)
	if err != nil {
		return result, fmt.Errorf("resolving TLS profile: %w", err)
	}

	result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
		// Go 1.22+ defaults the server minimum to TLS 1.2 only when tls.Config.MinVersion is unset; the explicit minimum set here needs no GODEBUG.
		c.MinVersion = minVersion
		if len(ciphers) > 0 {
			c.CipherSuites = ciphers
		}
		c.NextProtos = nextProtos
	})
	return result, nil
}

func extractTLSProfile(obj *unstructured.Unstructured) (*tlsSecurityProfile, error) {
	spec, ok := obj.Object["spec"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	profileRaw, ok := spec["tlsSecurityProfile"]
	if !ok || profileRaw == nil {
		return nil, nil
	}
	data, err := json.Marshal(profileRaw)
	if err != nil {
		return nil, fmt.Errorf("marshaling tlsSecurityProfile: %w", err)
	}
	var profile tlsSecurityProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("unmarshaling tlsSecurityProfile: %w", err)
	}
	return &profile, nil
}

func intermediateWithALPN(c *tls.Config) {
	// Go 1.22+ defaults the server minimum to TLS 1.2 only when tls.Config.MinVersion is unset; the explicit minimum set here needs no GODEBUG.
	c.MinVersion = tls.VersionTLS12
	c.CipherSuites = intermediateCiphers
	c.NextProtos = nextProtos
}

func parseProfile(profile *tlsSecurityProfile) (uint16, []uint16, error) {
	if profile == nil || profile.Type == "" {
		// No tlsSecurityProfile set on the cluster: the OpenShift default is
		// Intermediate, so this leniency is safe.
		return tls.VersionTLS12, intermediateCiphers, nil
	}

	switch profile.Type {
	case "Intermediate":
		return tls.VersionTLS12, intermediateCiphers, nil
	case "Modern":
		// Go manages TLS 1.3 cipher suites natively; nil CipherSuites keeps
		// that behavior.
		return tls.VersionTLS13, nil, nil
	case "Old":
		ciphers, err := resolveCiphers(oldProfileCiphers, tls.VersionTLS10)
		if err != nil {
			return 0, nil, err
		}
		return tls.VersionTLS10, ciphers, nil
	case "Custom":
		if profile.Custom == nil {
			return 0, nil, errors.New("custom TLS profile type specified but custom block is nil")
		}
		return parseCustomProfile(profile.Custom)
	default:
		// OpenShift's compatibility behavior treats unknown built-in types as
		// Intermediate until this component can understand the new profile.
		return tls.VersionTLS12, intermediateCiphers, nil
	}
}

func parseCustomProfile(custom *struct {
	Ciphers       []string `json:"ciphers"`
	MinTLSVersion string   `json:"minTLSVersion"`
}) (uint16, []uint16, error) {
	minVersion, ok := tlsVersionMap[custom.MinTLSVersion]
	if !ok {
		return 0, nil, fmt.Errorf("unknown minTLSVersion %q in custom TLS profile", custom.MinTLSVersion)
	}

	// Go's TLS 1.3 implementation always uses its built-in cipher suites;
	// tls.Config.CipherSuites has no effect for TLS 1.3 connections. Skip
	// cipher enforcement when the minimum version is TLS 1.3.
	if minVersion >= tls.VersionTLS13 {
		log.Info("Custom TLS profile specifies TLS 1.3 minimum: cipher list is ignored (Go enforces TLS 1.3 suites natively)")
		return minVersion, nil, nil
	}
	if len(custom.Ciphers) == 0 {
		return 0, nil, errors.New("custom TLS profile has no cipher suites for minimum TLS version below 1.3")
	}

	ciphers, err := resolveCiphers(custom.Ciphers, minVersion)
	if err != nil {
		return 0, nil, err
	}
	return minVersion, ciphers, nil
}

// resolveCiphers maps cipher names, in IANA form (Go's suite names) or
// OpenSSL form, to the suite IDs this Go runtime can negotiate, dropping the
// rest. TLS 1.3 cipher names are intentionally excluded below a TLS 1.3
// minimum: Go's TLS 1.3 implementation ignores tls.Config.CipherSuites and
// uses its own built-in suites, so including them would silently drop TLS 1.2
// suites and cause handshake failures for non-TLS 1.3 connections.
func resolveCiphers(names []string, minVersion uint16) ([]uint16, error) {
	ciphers := make([]uint16, 0, len(names))
	var dropped []string
	for _, name := range names {
		id, ok := cipherIDByName[name]
		if ok && runtimeCipherIDs[id] && (minVersion >= tls.VersionTLS13 || !isTLS13CipherSuite(id)) {
			ciphers = append(ciphers, id)
		} else {
			dropped = append(dropped, name)
		}
	}
	if len(dropped) > 0 {
		log.Info("Dropping ciphers from TLS profile not usable by this Go runtime", "ciphers", dropped)
	}
	if len(ciphers) == 0 {
		return nil, fmt.Errorf("none of the specified ciphers are supported by Go: %v", names)
	}
	return ciphers, nil
}

// isTLS13CipherSuite reports whether id is a TLS 1.3-only suite
// (IDs 0x1300-0x13ff).
func isTLS13CipherSuite(id uint16) bool {
	return id >= 0x1300 && id <= 0x13ff
}
