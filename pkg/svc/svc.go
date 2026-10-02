// Package svc is the operator/platform plane: direct access to the individual
// Spinnaker microservices, behind Gate rather than through it.
//
// Gate's API is the user plane — applications, pipelines, executions. It says
// nothing about whether orca's queue is backing up, what configuration
// clouddriver actually resolved at startup, or which log category to turn up to
// find out why a deploy stalled. Those answers live on each service's own HTTP
// port, in its Spring Boot actuator endpoints and its internal controllers.
//
// Two transports reach them:
//
//   - the Kubernetes API server's Service proxy subresource (default), which
//     needs only the credentials kubectl already has — no port-forward, no open
//     ports, and it works from outside the cluster;
//   - a direct base URL per service (--service-url), for installs that are not
//     on Kubernetes or that already expose the services.
package svc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/k8s"
)

// Service describes one Spinnaker microservice.
type Service struct {
	// Name is both the service's Kubernetes Service name and its Spring
	// application name.
	Name string
	// Port is the service's HTTP port, as upstream configures it.
	Port int
	// Role is a one-line description, shown by `sc svc list`.
	Role string
	// BasePath is the servlet context path the service is mounted under. Gate is
	// routinely deployed under /api/v1 so one ingress can serve Deck and the API;
	// everything else sits at the root.
	BasePath string
	// OptionalByDefault marks services upstream ships disabled
	// (services.<name>.enabled: false), so their absence is not a fault.
	OptionalByDefault bool
}

// Catalog is the set of services a Spinnaker installation can run.
//
// Ports are the upstream defaults, as declared in the kustomize base's
// spinnaker.yml service block.
var Catalog = []Service{
	{Name: "clouddriver", Port: 7002, Role: "cloud provider integration and the infrastructure cache"},
	{Name: "deck", Port: 9000, Role: "web UI (static assets; no actuator)"},
	{Name: "echo", Port: 8089, Role: "events, notifications and scheduled/webhook triggers"},
	{Name: "fiat", Port: 7003, Role: "authorization (roles, permissions)", OptionalByDefault: true},
	{Name: "front50", Port: 8080, Role: "persistent metadata: applications, pipelines, projects"},
	{Name: "gate", Port: 8084, Role: "API gateway — the only externally exposed service", BasePath: "/api/v1"},
	{Name: "igor", Port: 8088, Role: "CI integration (Jenkins, Travis, Concourse, GitLab CI)"},
	{Name: "kayenta", Port: 8090, Role: "automated canary analysis", OptionalByDefault: true},
	{Name: "keel", Port: 7010, Role: "managed delivery (declarative CD)", OptionalByDefault: true},
	{Name: "orca", Port: 8083, Role: "orchestration: executes pipelines and tasks"},
	{Name: "rosco", Port: 8087, Role: "image bakery (Packer)"},
}

// Lookup returns the catalog entry for a service name.
func Lookup(name string) (Service, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, s := range Catalog {
		if s.Name == n {
			return s, nil
		}
	}
	names := make([]string, 0, len(Catalog))
	for _, s := range Catalog {
		names = append(names, s.Name)
	}
	return Service{}, fmt.Errorf("unknown service %q; known services: %s", name, strings.Join(names, ", "))
}

// Names returns every catalog service name.
func Names() []string {
	out := make([]string, 0, len(Catalog))
	for _, s := range Catalog {
		out = append(out, s.Name)
	}
	return out
}

// JVMNames returns the services that are JVM apps with actuator endpoints
// (everything except deck, which serves static assets from nginx).
func JVMNames() []string {
	out := []string{}
	for _, s := range Catalog {
		if s.Name == "deck" {
			continue
		}
		out = append(out, s.Name)
	}
	return out
}

// Config configures a Client.
type Config struct {
	// Namespace holds the Spinnaker Deployments and Services.
	Namespace string
	// Kubeconfig / KubeContext select cluster credentials. Empty uses the usual
	// resolution order (see k8s.LoadConfig).
	Kubeconfig  string
	KubeContext string
	// ServiceURLs overrides the transport per service name with a direct base URL,
	// bypassing Kubernetes entirely.
	ServiceURLs map[string]string
	// Timeout bounds a single service request.
	Timeout time.Duration

	// Gate credentials. Gate is the one service that authenticates its own
	// endpoints, and the API server's Service proxy cannot carry those
	// credentials, so gate is reached through its externally configured URL
	// instead. GateURL already includes Gate's servlet context path, so BasePath
	// is not prepended when this route is used.
	GateURL      string
	GateUser     string
	GatePassword string
	GateToken    string
	GateInsecure bool
}

// Client reaches individual Spinnaker services.
type Client struct {
	cfg  Config
	k8s  *k8s.Client
	http *http.Client
	// k8sErr records why cluster access is unavailable, so the error surfaces at
	// the point of use rather than at construction — direct --service-url callers
	// never need a cluster.
	k8sErr error
}

// New builds a Client. It does not fail when no Kubernetes config is available:
// direct service URLs may be enough, and the cluster error is reported only if a
// call actually needs it.
func New(cfg Config) *Client {
	if cfg.Namespace == "" {
		cfg.Namespace = "spinnaker"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	transport := &http.Transport{}
	if cfg.GateInsecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in via --insecure
	}
	c := &Client{cfg: cfg, http: &http.Client{Timeout: cfg.Timeout, Transport: transport}}
	restCfg, err := k8s.LoadConfig(cfg.Kubeconfig, cfg.KubeContext)
	if err != nil {
		c.k8sErr = err
		return c
	}
	kc, err := k8s.NewClient(restCfg)
	if err != nil {
		c.k8sErr = err
		return c
	}
	c.k8s = kc
	return c
}

// Namespace returns the configured namespace.
func (c *Client) Namespace() string { return c.cfg.Namespace }

// Kube returns the underlying Kubernetes client, or an error explaining why
// cluster access is unavailable.
func (c *Client) Kube() (*k8s.Client, error) {
	if c.k8s == nil {
		return nil, fmt.Errorf("this command needs Kubernetes access: %w", c.k8sErr)
	}
	return c.k8s, nil
}

// HasKube reports whether cluster access is available.
func (c *Client) HasKube() bool { return c.k8s != nil }

// KubeSource describes where cluster credentials came from (empty when absent).
func (c *Client) KubeSource() string {
	if c.k8s == nil {
		return ""
	}
	return c.k8s.Source()
}

// Request is a call to one service.
type Request struct {
	Service string
	Method  string
	// Path is relative to the service root; the service's BasePath is prepended.
	Path  string
	Query url.Values
	Body  []byte
}

// Do performs req against a service and returns the raw response body.
func (c *Client) Do(ctx context.Context, req Request) ([]byte, error) {
	s, err := Lookup(req.Service)
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	// An explicit --service-url is a complete base URL, so it already carries
	// whatever context path the service is mounted under.
	if base, ok := c.cfg.ServiceURLs[s.Name]; ok && base != "" {
		return c.doDirect(ctx, base, method, req.Path, req.Query, req.Body, s.Name == "gate")
	}

	// Gate authenticates its own actuator endpoints (everything but /health
	// redirects to its login page), and the API server's Service proxy has no way
	// to present Gate credentials — it replaces the Authorization header with its
	// own. Gate is, however, the one service that is externally reachable by
	// definition, so its own configured endpoint is used instead.
	if s.Name == "gate" && c.cfg.GateURL != "" {
		return c.doDirect(ctx, c.cfg.GateURL, method, req.Path, req.Query, req.Body, true)
	}

	// Kubernetes Service proxy transport.
	kc, err := c.Kube()
	if err != nil {
		return nil, err
	}
	raw, err := kc.ProxyRequest(ctx, c.cfg.Namespace, s.Name, s.Port, method, joinPath(s.BasePath, req.Path), req.Query, req.Body)
	if err != nil {
		return nil, annotateServiceError(s, err)
	}
	return raw, nil
}

func (c *Client) doDirect(ctx context.Context, base, method, path string, q url.Values, body []byte, withGateAuth bool) ([]byte, error) {
	u := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = strings.NewReader(string(body))
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	if len(body) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if withGateAuth {
		switch {
		case strings.HasPrefix(c.cfg.GateToken, "spk_"):
			httpReq.Header.Set("X-Spinnaker-Token", c.cfg.GateToken)
		case c.cfg.GateToken != "":
			httpReq.Header.Set("Authorization", "Bearer "+c.cfg.GateToken)
		case c.cfg.GateUser != "":
			httpReq.SetBasicAuth(c.cfg.GateUser, c.cfg.GatePassword)
		}
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return raw, fmt.Errorf("%s %s: service returned %d: %s", method, path, resp.StatusCode, truncate(string(raw), 400))
	}
	return raw, nil
}

// GetJSON performs a GET against a service and decodes the response.
func (c *Client) GetJSON(ctx context.Context, service, path string, q url.Values, out any) error {
	raw, err := c.Do(ctx, Request{Service: service, Method: http.MethodGet, Path: path, Query: q})
	if err != nil {
		return err
	}
	if out == nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", service, path, err)
	}
	return nil
}

// annotateServiceError turns the two failures operators actually hit into
// actionable messages: an endpoint that exists but is not exposed, and a service
// that upstream ships disabled.
func annotateServiceError(s Service, err error) error {
	if k8s.IsNotFound(err) {
		msg := fmt.Sprintf("%s: %v", s.Name, err)
		if s.OptionalByDefault {
			return fmt.Errorf("%s\n\n%s is disabled by default upstream (services.%s.enabled: false). "+
				"Enable it in your Spinnaker config before using this command", msg, s.Name, s.Name)
		}
		return fmt.Errorf("%s\n\nIf the service is running, this is most likely an actuator endpoint that is not exposed. "+
			"kork mounts actuator at the ROOT path (not /actuator), and Spring Boot exposes only /health by default. "+
			"Add the endpoint to management.endpoints.web.exposure.include — see 'sc svc actuator-config'", msg)
	}
	return fmt.Errorf("%s: %w", s.Name, err)
}

func joinPath(basePath, path string) string {
	p := "/" + strings.TrimLeft(path, "/")
	if basePath == "" {
		return p
	}
	return strings.TrimRight(basePath, "/") + p
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// Service inventory
// ---------------------------------------------------------------------------

// Status is one service's deployment and health state.
type Status struct {
	Name     string `json:"name" yaml:"name"`
	Role     string `json:"role" yaml:"role"`
	Port     int    `json:"port" yaml:"port"`
	Replicas string `json:"replicas" yaml:"replicas"`
	Image    string `json:"image,omitempty" yaml:"image,omitempty"`
	Version  string `json:"version,omitempty" yaml:"version,omitempty"`
	// Health is the actuator status (UP/DOWN), or a short reason it is unknown.
	Health string `json:"health" yaml:"health"`
	// Deployed reports whether a Deployment for the service exists.
	Deployed bool   `json:"deployed" yaml:"deployed"`
	Note     string `json:"note,omitempty" yaml:"note,omitempty"`
}

// List reports every catalog service's deployment and health state.
//
// Health is probed concurrently; a service with no Deployment is reported as
// not deployed rather than as an error, because an installation legitimately
// runs a subset.
func (c *Client) List(ctx context.Context) ([]Status, error) {
	kc, err := c.Kube()
	if err != nil {
		return nil, err
	}
	deployments, err := kc.ListDeployments(ctx, c.cfg.Namespace, "")
	if err != nil {
		return nil, err
	}
	byName := map[string]k8s.Deployment{}
	for _, d := range deployments {
		byName[d.Name] = d
	}

	out := make([]Status, len(Catalog))
	type result struct {
		idx    int
		health string
	}
	results := make(chan result, len(Catalog))
	probes := 0

	for i, s := range Catalog {
		st := Status{Name: s.Name, Role: s.Role, Port: s.Port, Health: "-"}
		d, ok := byName[s.Name]
		if !ok {
			st.Replicas = "-"
			st.Note = "no Deployment in namespace " + c.cfg.Namespace
			if s.OptionalByDefault {
				st.Note = "not deployed (disabled by default upstream)"
			}
			out[i] = st
			continue
		}
		st.Deployed = true
		st.Replicas = fmt.Sprintf("%d/%d", d.ReadyReplicas, d.Replicas)
		if len(d.Images) > 0 {
			st.Image = d.Images[0]
			if idx := strings.LastIndex(st.Image, ":"); idx >= 0 {
				st.Version = st.Image[idx+1:]
			}
		}
		if d.Replicas == 0 {
			st.Health = "SCALED_TO_ZERO"
			if s.OptionalByDefault {
				st.Note = "disabled by default upstream"
			}
			out[i] = st
			continue
		}
		if s.Name == "deck" {
			st.Health = "N/A"
			st.Note = "static assets; no actuator"
			out[i] = st
			continue
		}
		out[i] = st
		probes++
		go func(idx int, name string) {
			h, err := c.Health(ctx, name)
			if err != nil {
				results <- result{idx, "UNREACHABLE"}
				return
			}
			status, _ := h["status"].(string)
			if status == "" {
				status = "UNKNOWN"
			}
			results <- result{idx, status}
		}(i, s.Name)
	}

	for i := 0; i < probes; i++ {
		r := <-results
		out[r.idx].Health = r.health
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Live configuration
// ---------------------------------------------------------------------------

// LiveConfig returns the configuration files currently mounted into a service,
// read from its ConfigMaps.
//
// This is the desired config as the cluster holds it. It is not the same as the
// config the JVM resolved — for that, use Env, which reports Spring's merged
// property sources including defaults, environment variables and profiles.
func (c *Client) LiveConfig(ctx context.Context, service string) (map[string]string, error) {
	s, err := Lookup(service)
	if err != nil {
		return nil, err
	}
	kc, err := c.Kube()
	if err != nil {
		return nil, err
	}
	// Kustomize appends a content hash to generated ConfigMap names, so the
	// service's own ConfigMap must be found by prefix rather than exact name.
	names, err := kc.ListConfigMaps(ctx, c.cfg.Namespace, "")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	wanted := []string{s.Name, "spinnaker"}
	sort.Strings(names)
	for _, n := range names {
		for _, w := range wanted {
			if n == w || strings.HasPrefix(n, w+"-") {
				data, err := kc.GetConfigMap(ctx, c.cfg.Namespace, n)
				if err != nil {
					continue
				}
				for k, v := range data {
					out[n+"/"+k] = v
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no ConfigMaps found for service " + s.Name + " in namespace " + c.cfg.Namespace)
	}
	return out, nil
}
