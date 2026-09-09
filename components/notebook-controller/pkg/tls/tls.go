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

// openSSLToGoCipher maps OpenShift TLS profile cipher names to Go TLS IDs.
// TLS 1.3 cipher names (TLS_AES_*, TLS_CHACHA20_*) are intentionally excluded:
// Go's TLS 1.3 implementation ignores tls.Config.CipherSuites and uses its own
// built-in suites, so including them would silently drop TLS 1.2 suites and
// cause handshake failures for non-TLS-1.3 connections.
var openSSLToGoCipher = map[string]uint16{
	"ECDHE-ECDSA-AES128-GCM-SHA256":        tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-RSA-AES128-GCM-SHA256":          tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	"ECDHE-ECDSA-AES256-GCM-SHA384":        tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-RSA-AES256-GCM-SHA384":          tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	"ECDHE-ECDSA-CHACHA20-POLY1305-SHA256": tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305-SHA256":   tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-ECDSA-CHACHA20-POLY1305":        tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	"ECDHE-RSA-CHACHA20-POLY1305":          tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
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
// On clusters where the API is not available or temporarily unavailable,
// it returns hardened Intermediate defaults.
func Resolve(ctx context.Context, cfg *rest.Config) (Result, error) {
	var result Result

	resolveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return result, fmt.Errorf("creating dynamic client for TLS profile: %w", err)
	}

	obj, err := dynClient.Resource(apiServerGVR).Get(resolveCtx, "cluster", metav1.GetOptions{})
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
		case apierrors.IsForbidden(err),
			apierrors.IsUnauthorized(err):
			log.Error(err, "Permission denied reading TLS profile, using hardened defaults; watcher will retry")
			result.APIAvailable = true
		default:
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
		log.Info("Failed to parse TLS profile from APIServer, using hardened defaults", "error", err)
		result.TLSOpts = append(result.TLSOpts, intermediateWithALPN)
		return result, nil
	}

	minVersion, ciphers := parseProfile(profile)
	if ciphers != nil && len(ciphers) == 0 {
		return result, fmt.Errorf("custom TLS profile specified ciphers but none are supported by Go")
	}

	result.TLSOpts = append(result.TLSOpts, func(c *tls.Config) {
		c.MinVersion = minVersion
		if len(ciphers) > 0 {
			c.CipherSuites = ciphers
		}
		c.NextProtos = []string{"h2", "http/1.1"}
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
	c.MinVersion = tls.VersionTLS12
	c.CipherSuites = intermediateCiphers
	c.NextProtos = []string{"h2", "http/1.1"}
}

func parseProfile(profile *tlsSecurityProfile) (uint16, []uint16) {
	if profile == nil || profile.Type == "" {
		return tls.VersionTLS12, intermediateCiphers
	}

	switch profile.Type {
	case "Intermediate":
		return tls.VersionTLS12, intermediateCiphers
	case "Modern":
		return tls.VersionTLS13, nil
	case "Old":
		return tls.VersionTLS10, nil
	case "Custom":
		if profile.Custom == nil {
			log.Info("Custom TLS profile type specified but custom block is nil, falling back to Intermediate")
			return tls.VersionTLS12, intermediateCiphers
		}
		return parseCustomProfile(profile.Custom)
	default:
		log.Info("Unknown TLS profile type, falling back to Intermediate", "type", profile.Type)
		return tls.VersionTLS12, intermediateCiphers
	}
}

func parseCustomProfile(custom *struct {
	Ciphers       []string `json:"ciphers"`
	MinTLSVersion string   `json:"minTLSVersion"`
}) (uint16, []uint16) {
	minVersion, ok := tlsVersionMap[custom.MinTLSVersion]
	if !ok {
		log.Info("Unknown minTLSVersion in custom profile, defaulting to TLS 1.2", "minTLSVersion", custom.MinTLSVersion)
		minVersion = tls.VersionTLS12
	}

	if len(custom.Ciphers) == 0 {
		return minVersion, nil
	}

	// Go's TLS 1.3 implementation always uses its built-in cipher suites;
	// tls.Config.CipherSuites has no effect for TLS 1.3 connections. Skip
	// cipher enforcement when the minimum version is TLS 1.3.
	if minVersion >= tls.VersionTLS13 {
		log.Info("Custom TLS profile specifies TLS 1.3 minimum: cipher list is ignored (Go enforces TLS 1.3 suites natively)")
		return minVersion, nil
	}

	ciphers := make([]uint16, 0, len(custom.Ciphers))
	for _, name := range custom.Ciphers {
		if id, ok := openSSLToGoCipher[name]; ok {
			ciphers = append(ciphers, id)
		} else {
			log.Info("Dropping unsupported cipher from custom TLS profile", "cipher", name)
		}
	}
	return minVersion, ciphers
}
