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
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
)

func TestPinnedKubeRbacProxyImageStarts(t *testing.T) {
	startPinnedImageTest(t)
	withFIPS(t, false)

	assertPinnedImageBinaries(t)
	assertPinnedImageSupportsCurvePreferences(t)

	intermediate, err := intermediateKubeRbacProxyTLS()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := flagWithPrefix(intermediate.Args, intermediateCurveArg); !ok {
		t.Fatalf("rendered Intermediate args omit %s: %v", tlsCurvePreferencesFlag, intermediate.Args)
	}
	t.Logf("starting pinned image with Intermediate args: %s", strings.Join(intermediate.Args, " "))
	startPinnedProxy(t, intermediate.Args)

	modern := mustResolve(t, modernTLSProfile(), configv1.TLSAdherencePolicyStrictAllComponents)
	if _, ok := flagWithPrefix(modern.Args, tlsCipherSuitesFlag); ok {
		t.Fatalf("TLS 1.3 args include a cipher flag: %v", modern.Args)
	}
	if _, ok := flagWithPrefix(modern.Args, intermediateCurveArg); !ok {
		t.Fatalf("rendered TLS 1.3 args omit %s: %v", tlsCurvePreferencesFlag, modern.Args)
	}
	t.Logf("starting pinned image with TLS 1.3 args: %s", strings.Join(modern.Args, " "))
	startPinnedProxy(t, modern.Args)
}

func startPinnedImageTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not installed")
	}
	if os.Getenv("RUN_KUBE_RBAC_PROXY_IMAGE_TEST") == "1" {
		return
	}
	if err := exec.Command("podman", "image", "exists", pinnedKubeRbacProxyImage).Run(); err != nil {
		t.Skip("pinned kube-rbac-proxy image is not local; set RUN_KUBE_RBAC_PROXY_IMAGE_TEST=1 to pull it")
	}
}

func assertPinnedImageBinaries(t *testing.T) {
	t.Helper()
	out, err := exec.Command("podman", "run", "--rm", "--entrypoint", "/bin/ls", pinnedKubeRbacProxyImage,
		"/usr/bin/kube-rbac-proxy").CombinedOutput()
	if err != nil {
		t.Fatalf("pinned image is missing kube-rbac-proxy: %v\n%s", err, out)
	}
}

func assertPinnedImageSupportsCurvePreferences(t *testing.T) {
	t.Helper()
	out, err := exec.Command("podman", "run", "--rm", pinnedKubeRbacProxyImage, "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("kube-rbac-proxy --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), strings.TrimSuffix(tlsCurvePreferencesFlag, "=")) {
		t.Fatalf("pinned image does not advertise %s\n%s", tlsCurvePreferencesFlag, out)
	}
}

func startPinnedProxy(t *testing.T, tlsArgs []string) {
	t.Helper()
	// The image runs as a non-root user, so the certificate directory has to
	// live under a world-traversable parent and the files must be world-readable.
	hostDir, err := os.MkdirTemp("/tmp", "krp-tls-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostDir) })
	if err := os.Chmod(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyCerts(hostDir); err != nil {
		t.Fatal(err)
	}

	listenPort := freeLocalPort(t)
	healthPort := freeLocalPort(t)
	name := fmt.Sprintf("krp-tls-%d", time.Now().UnixNano())
	args := append([]string{
		"run", "--name", name,
		"--network=host",
		"--security-opt", "label=disable",
		"-v", hostDir + ":/certs:ro",
		pinnedKubeRbacProxyImage,
		"--kubeconfig=/certs/kubeconfig",
		"--secure-listen-address=127.0.0.1:" + listenPort,
		notebookUpstreamArg,
		"--proxy-endpoints-port=" + healthPort,
		"--tls-cert-file=/certs/tls.crt",
		"--tls-private-key-file=/certs/tls.key",
	}, tlsArgs...)
	cmd := exec.Command("podman", args...)
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rm", "-f", name).Run()
	})
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	httpClient := &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test certificate is generated locally
		},
	}
	deadline := time.Now().Add(20 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("kube-rbac-proxy exited: %v\n%s", err, logs.String())
		default:
		}
		resp, err := httpClient.Get("https://127.0.0.1:" + healthPort + probeTimeoutHealthPath)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				_ = exec.Command("podman", "stop", "-t", "1", name).Run()
				return
			}
			lastErr = fmt.Errorf("status %s", resp.Status)
		} else {
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("healthz failed: %v\n%s", lastErr, logs.String())
}

func writeProxyCerts(dir string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: pemCertificateBlockType, Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
users:
- name: u
  user:
    token: dummy
contexts:
- name: c
  context:
    cluster: c
    user: u
current-context: c
`)
	files := map[string][]byte{
		"tls.crt":    certPEM,
		"tls.key":    keyPEM,
		"kubeconfig": kubeconfig,
	}
	for name, contents := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func freeLocalPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}
