package k8s

import (
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCA = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigReadsCurrentContext(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
current-context: lab
clusters:
- name: lab-cluster
  cluster:
    server: https://10.0.0.1:6443
    certificate-authority-data: `+base64.StdEncoding.EncodeToString([]byte(testCA))+`
users:
- name: lab-user
  user:
    token: secret-token
contexts:
- name: lab
  context:
    cluster: lab-cluster
    user: lab-user
    namespace: spinnaker
`)
	cfg, err := LoadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://10.0.0.1:6443" {
		t.Errorf("server = %q", cfg.Server)
	}
	if cfg.BearerToken != "secret-token" {
		t.Errorf("token = %q", cfg.BearerToken)
	}
	if cfg.Namespace != "spinnaker" {
		t.Errorf("namespace = %q", cfg.Namespace)
	}
	if string(cfg.CAData) != testCA {
		t.Errorf("CA data was not decoded from certificate-authority-data")
	}
}

func TestLoadConfigSelectsNamedContext(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
current-context: one
clusters:
- name: c1
  cluster: {server: https://one:6443}
- name: c2
  cluster: {server: https://two:6443}
users:
- name: u1
  user: {token: t1}
- name: u2
  user: {token: t2}
contexts:
- name: one
  context: {cluster: c1, user: u1}
- name: two
  context: {cluster: c2, user: u2}
`)
	cfg, err := LoadConfig(path, "two")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != "https://two:6443" || cfg.BearerToken != "t2" {
		t.Errorf("named context not honored: %+v", cfg)
	}
}

func TestLoadConfigMissingContextListsAvailable(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
current-context: one
clusters: [{name: c1, cluster: {server: https://one:6443}}]
users: [{name: u1, user: {token: t1}}]
contexts: [{name: one, context: {cluster: c1, user: u1}}]
`)
	_, err := LoadConfig(path, "nope")
	if err == nil {
		t.Fatal("expected an error for an unknown context")
	}
	// The error should be actionable: name what is actually available.
	if !strings.Contains(err.Error(), "one") {
		t.Errorf("error should list the available contexts, got: %v", err)
	}
}

// TestLoadConfigRejectsMissingExplicitPath checks that an explicit --kubeconfig
// that does not exist is an error rather than a silent fallback to the ambient
// in-cluster or ~/.kube/config credentials, which would target the wrong cluster.
func TestLoadConfigRejectsMissingExplicitPath(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent"), "")
	if err == nil {
		t.Fatal("expected an error for a missing explicit kubeconfig")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the file was not found", err)
	}
}

func TestLoadConfigRejectsExecCredentialPlugin(t *testing.T) {
	path := writeKubeconfig(t, `
apiVersion: v1
current-context: eks
clusters: [{name: c, cluster: {server: https://eks:443}}]
users:
- name: u
  user:
    exec:
      command: aws
contexts: [{name: eks, context: {cluster: c, user: u}}]
`)
	_, err := LoadConfig(path, "")
	if err == nil {
		t.Fatal("expected an error for an exec credential plugin")
	}
	// The message must point at the workaround rather than just failing.
	if !strings.Contains(err.Error(), "--service-url") {
		t.Errorf("error should suggest the alternative, got: %v", err)
	}
}

func TestLoadConfigReadsClientCertFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, []byte("CERT"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	// Relative paths are resolved against the kubeconfig's directory, as kubectl does.
	path := filepath.Join(dir, "config")
	body := `
apiVersion: v1
current-context: lab
clusters: [{name: c, cluster: {server: https://host:6443}}]
users:
- name: u
  user:
    client-certificate: client.crt
    client-key: client.key
contexts: [{name: lab, context: {cluster: c, user: u}}]
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.CertData) != "CERT" || string(cfg.KeyData) != "KEY" {
		t.Errorf("client cert/key not loaded from paths relative to the kubeconfig: %+v", cfg)
	}
}

func TestServiceProxyPath(t *testing.T) {
	got := ServiceProxyPath("spinnaker", "orca", 8083, "/health")
	want := "/api/v1/namespaces/spinnaker/services/orca:8083/proxy/health"
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestSetEscapedPathPreservesEncoding guards against double-escaping a path that
// already contains percent-encoded segments.
func TestSetEscapedPathPreservesEncoding(t *testing.T) {
	u, err := url.Parse("https://apiserver:6443")
	if err != nil {
		t.Fatal(err)
	}
	setEscapedPath(u, "/api/v1/namespaces/spinnaker/services/clouddriver:7002/proxy/manifests/managing/demo/deployment%20nginx")
	if !strings.HasSuffix(u.String(), "deployment%20nginx") {
		t.Errorf("url = %q, want the %%20 preserved rather than re-escaped", u.String())
	}
}
