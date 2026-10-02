// Package k8s is a deliberately small Kubernetes API client, carrying only what
// the operator plane needs: reaching a Service through the API server's proxy
// subresource, and reading/scaling the Deployments and Pods behind it.
//
// It exists instead of a client-go dependency because that would add tens of
// megabytes of transitive dependencies to a CLI that needs four verbs. The
// trade-off is that exec/attach (which require SPDY or WebSocket upgrades) are
// out of scope here.
package k8s

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// kubeconfig mirrors the subset of a kubeconfig file this client understands.
type kubeconfig struct {
	APIVersion     string `yaml:"apiVersion"`
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			Username              string `yaml:"username"`
			Password              string `yaml:"password"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKey             string `yaml:"client-key"`
			ClientKeyData         string `yaml:"client-key-data"`
			Exec                  *struct {
				Command string `yaml:"command"`
			} `yaml:"exec"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// RestConfig is everything needed to call an API server.
type RestConfig struct {
	Server    string
	Namespace string

	CAData   []byte
	CertData []byte
	KeyData  []byte

	BearerToken string
	Username    string
	Password    string

	Insecure bool

	// Source records where this config came from, for error messages.
	Source string
}

// in-cluster service account paths, mounted into every pod by kubelet.
const (
	inClusterTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAPath        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// LoadConfig resolves a Kubernetes client config.
//
// Resolution order: an explicit kubeconfig path, then $KUBECONFIG (first entry),
// then the in-cluster service account, then ~/.kube/config. contextName selects a
// context other than current-context.
func LoadConfig(kubeconfigPath, contextName string) (*RestConfig, error) {
	candidates := []string{}
	if kubeconfigPath != "" {
		candidates = append(candidates, kubeconfigPath)
	} else if env := os.Getenv("KUBECONFIG"); env != "" {
		// KUBECONFIG may list several files; this client uses the first that exists.
		candidates = append(candidates, filepath.SplitList(env)...)
	}

	for _, path := range candidates {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return loadKubeconfigFile(path, contextName)
		}
	}

	// Only fall back to the in-cluster config when no kubeconfig was named: an
	// explicit --kubeconfig that does not exist should be an error, not a silent
	// switch to the ambient service account.
	if kubeconfigPath == "" {
		if cfg, err := loadInCluster(); err == nil {
			return cfg, nil
		}
		if home, err := os.UserHomeDir(); err == nil {
			def := filepath.Join(home, ".kube", "config")
			if _, err := os.Stat(def); err == nil {
				return loadKubeconfigFile(def, contextName)
			}
		}
	}

	if kubeconfigPath != "" {
		return nil, fmt.Errorf("kubeconfig %q not found", kubeconfigPath)
	}
	return nil, errors.New("no Kubernetes config found: set --kubeconfig, $KUBECONFIG, or ~/.kube/config " +
		"(the operator plane needs cluster access; Gate-only commands do not)")
}

func loadInCluster() (*RestConfig, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster")
	}
	token, err := os.ReadFile(inClusterTokenPath)
	if err != nil {
		return nil, err
	}
	cfg := &RestConfig{
		Server:      "https://" + host + ":" + port,
		BearerToken: strings.TrimSpace(string(token)),
		Namespace:   "default",
		Source:      "in-cluster service account",
	}
	if ca, err := os.ReadFile(inClusterCAPath); err == nil {
		cfg.CAData = ca
	}
	if ns, err := os.ReadFile(inClusterNamespacePath); err == nil {
		cfg.Namespace = strings.TrimSpace(string(ns))
	}
	return cfg, nil
}

func loadKubeconfigFile(path, contextName string) (*RestConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return nil, fmt.Errorf("parsing kubeconfig %s: %w", path, err)
	}

	wanted := contextName
	if wanted == "" {
		wanted = kc.CurrentContext
	}
	if wanted == "" {
		return nil, fmt.Errorf("kubeconfig %s has no current-context; pass --kube-context", path)
	}

	var clusterName, userName, namespace string
	found := false
	for _, c := range kc.Contexts {
		if c.Name == wanted {
			clusterName, userName, namespace = c.Context.Cluster, c.Context.User, c.Context.Namespace
			found = true
			break
		}
	}
	if !found {
		names := make([]string, 0, len(kc.Contexts))
		for _, c := range kc.Contexts {
			names = append(names, c.Name)
		}
		return nil, fmt.Errorf("context %q not found in %s (have: %s)", wanted, path, strings.Join(names, ", "))
	}

	cfg := &RestConfig{Namespace: namespace, Source: path + " (context " + wanted + ")"}

	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		cfg.Server = c.Cluster.Server
		cfg.Insecure = c.Cluster.InsecureSkipTLSVerify
		if c.Cluster.CertificateAuthorityData != "" {
			data, err := base64.StdEncoding.DecodeString(c.Cluster.CertificateAuthorityData)
			if err != nil {
				return nil, fmt.Errorf("decoding certificate-authority-data for cluster %q: %w", clusterName, err)
			}
			cfg.CAData = data
		} else if c.Cluster.CertificateAuthority != "" {
			data, err := os.ReadFile(resolveRelative(path, c.Cluster.CertificateAuthority))
			if err != nil {
				return nil, fmt.Errorf("reading certificate-authority for cluster %q: %w", clusterName, err)
			}
			cfg.CAData = data
		}
	}
	if cfg.Server == "" {
		return nil, fmt.Errorf("cluster %q not found in %s", clusterName, path)
	}

	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		if u.User.Exec != nil && u.User.Exec.Command != "" {
			return nil, fmt.Errorf("kubeconfig user %q uses an exec credential plugin (%s), which this client does not run; "+
				"use a kubeconfig with a token or client certificate, or set --service-url to reach the services directly",
				userName, u.User.Exec.Command)
		}
		cfg.BearerToken = u.User.Token
		cfg.Username, cfg.Password = u.User.Username, u.User.Password
		if u.User.TokenFile != "" {
			data, err := os.ReadFile(resolveRelative(path, u.User.TokenFile))
			if err != nil {
				return nil, fmt.Errorf("reading tokenFile for user %q: %w", userName, err)
			}
			cfg.BearerToken = strings.TrimSpace(string(data))
		}
		if u.User.ClientCertificateData != "" {
			data, err := base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
			if err != nil {
				return nil, fmt.Errorf("decoding client-certificate-data for user %q: %w", userName, err)
			}
			cfg.CertData = data
		} else if u.User.ClientCertificate != "" {
			data, err := os.ReadFile(resolveRelative(path, u.User.ClientCertificate))
			if err != nil {
				return nil, fmt.Errorf("reading client-certificate for user %q: %w", userName, err)
			}
			cfg.CertData = data
		}
		if u.User.ClientKeyData != "" {
			data, err := base64.StdEncoding.DecodeString(u.User.ClientKeyData)
			if err != nil {
				return nil, fmt.Errorf("decoding client-key-data for user %q: %w", userName, err)
			}
			cfg.KeyData = data
		} else if u.User.ClientKey != "" {
			data, err := os.ReadFile(resolveRelative(path, u.User.ClientKey))
			if err != nil {
				return nil, fmt.Errorf("reading client-key for user %q: %w", userName, err)
			}
			cfg.KeyData = data
		}
	}

	return cfg, nil
}

// resolveRelative resolves a path referenced from a kubeconfig relative to that
// kubeconfig's directory, as kubectl does.
func resolveRelative(kubeconfigPath, ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(filepath.Dir(kubeconfigPath), ref)
}
