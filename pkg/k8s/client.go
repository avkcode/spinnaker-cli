package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls a Kubernetes API server.
type Client struct {
	cfg  *RestConfig
	http *http.Client
	base *url.URL
}

// NewClient builds a client from a resolved RestConfig.
func NewClient(cfg *RestConfig) (*Client, error) {
	if cfg == nil || cfg.Server == "" {
		return nil, errors.New("kubernetes config has no server")
	}
	base, err := url.Parse(strings.TrimRight(cfg.Server, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid kubernetes server %q: %w", cfg.Server, err)
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure} //nolint:gosec // mirrors kubeconfig's insecure-skip-tls-verify
	if len(cfg.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CAData) {
			return nil, errors.New("kubernetes CA certificate could not be parsed")
		}
		tlsCfg.RootCAs = pool
	}
	if len(cfg.CertData) > 0 && len(cfg.KeyData) > 0 {
		pair, err := tls.X509KeyPair(cfg.CertData, cfg.KeyData)
		if err != nil {
			return nil, fmt.Errorf("kubernetes client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	return &Client{
		cfg:  cfg,
		base: base,
		http: &http.Client{
			Timeout:   0, // per-request, via context: log following has no deadline
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// Namespace returns the config's default namespace ("default" when unset).
func (c *Client) Namespace() string {
	if c.cfg.Namespace == "" {
		return "default"
	}
	return c.cfg.Namespace
}

// Server returns the API server URL.
func (c *Client) Server() string { return c.cfg.Server }

// Source describes where the credentials came from.
func (c *Client) Source() string { return c.cfg.Source }

// Error is a non-2xx response from the API server.
type Error struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *Error) Error() string {
	body := strings.TrimSpace(e.Body)
	// Kubernetes errors are a Status object; its message is the useful part.
	var status struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if json.Unmarshal([]byte(body), &status) == nil && status.Message != "" {
		body = status.Message
	}
	if len(body) > 500 {
		body = body[:500] + "…"
	}
	return fmt.Sprintf("%s %s: kubernetes returned %d: %s", e.Method, e.Path, e.Status, body)
}

// NotFound reports whether the error is a 404.
func (e *Error) NotFound() bool { return e.Status == http.StatusNotFound }

// IsNotFound reports whether err is a 404 from the API server.
func IsNotFound(err error) bool {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.NotFound()
	}
	return false
}

// Request is a single API server call.
type Request struct {
	Method      string
	Path        string
	Query       url.Values
	Body        []byte
	ContentType string
	Accept      string
}

// Stream performs req and returns the response body for the caller to read and
// close. Used for log following, where the body is open-ended.
func (c *Client) Stream(ctx context.Context, req Request) (io.ReadCloser, error) {
	u := *c.base
	// req.Path arrives percent-encoded (service-proxy paths carry escaped names),
	// so it must go into RawPath rather than the decoded Path field — otherwise
	// URL.String() would escape the escapes.
	setEscapedPath(&u, "/"+strings.TrimLeft(req.Path, "/"))
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	if req.Accept != "" {
		httpReq.Header.Set("Accept", req.Accept)
	}
	if c.cfg.BearerToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	} else if c.cfg.Username != "" {
		httpReq.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, &Error{Method: method, Path: req.Path, Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.Body, nil
}

// Do performs req and returns the full response body.
func (c *Client) Do(ctx context.Context, req Request) ([]byte, error) {
	body, err := c.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(body)
}

// GetRaw GETs an API path and returns the raw body.
func (c *Client) GetRaw(ctx context.Context, path string, q url.Values) ([]byte, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, Path: path, Query: q, Accept: "application/json"})
}

// GetJSON GETs an API path and decodes it into out.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	raw, err := c.GetRaw(ctx, path, q)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---------------------------------------------------------------------------
// Service proxy
// ---------------------------------------------------------------------------

// ServiceProxyPath builds the API path that proxies a request to a Service port.
//
// Going through the API server's proxy subresource means the operator plane needs
// no port-forward subprocess, no open ports and no in-cluster placement — just
// the same credentials kubectl uses. Requires get/create on
// services/proxy in the namespace.
func ServiceProxyPath(namespace, service string, port int, path string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy/%s",
		url.PathEscape(namespace), url.PathEscape(service), port, strings.TrimLeft(path, "/"))
}

// ProxyGet GETs a path on a Service through the API server proxy.
func (c *Client) ProxyGet(ctx context.Context, namespace, service string, port int, path string, q url.Values) ([]byte, error) {
	return c.Do(ctx, Request{
		Method: http.MethodGet,
		Path:   ServiceProxyPath(namespace, service, port, path),
		Query:  q,
		Accept: "application/json",
	})
}

// ProxyRequest performs an arbitrary method against a Service through the proxy.
func (c *Client) ProxyRequest(ctx context.Context, namespace, service string, port int, method, path string, q url.Values, body []byte) ([]byte, error) {
	return c.Do(ctx, Request{
		Method:      method,
		Path:        ServiceProxyPath(namespace, service, port, path),
		Query:       q,
		Body:        body,
		ContentType: "application/json",
		Accept:      "application/json",
	})
}

// ---------------------------------------------------------------------------
// Workloads
// ---------------------------------------------------------------------------

// Deployment is the subset of a Deployment this client reports on.
type Deployment struct {
	Name               string
	Namespace          string
	Replicas           int
	ReadyReplicas      int
	AvailableReplicas  int
	UpdatedReplicas    int
	Images             []string
	Containers         []string
	CreatedAt          time.Time
	Generation         int64
	ObservedGeneration int64
}

// ListDeployments returns the Deployments in a namespace, optionally filtered by
// a label selector.
func (c *Client) ListDeployments(ctx context.Context, namespace, labelSelector string) ([]Deployment, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string    `json:"name"`
				Namespace         string    `json:"namespace"`
				CreationTimestamp time.Time `json:"creationTimestamp"`
				Generation        int64     `json:"generation"`
			} `json:"metadata"`
			Spec struct {
				Replicas *int `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas      int   `json:"readyReplicas"`
				AvailableReplicas  int   `json:"availableReplicas"`
				UpdatedReplicas    int   `json:"updatedReplicas"`
				ObservedGeneration int64 `json:"observedGeneration"`
			} `json:"status"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", url.PathEscape(namespace))
	if err := c.GetJSON(ctx, path, q, &list); err != nil {
		return nil, err
	}
	out := make([]Deployment, 0, len(list.Items))
	for _, it := range list.Items {
		d := Deployment{
			Name:               it.Metadata.Name,
			Namespace:          it.Metadata.Namespace,
			ReadyReplicas:      it.Status.ReadyReplicas,
			AvailableReplicas:  it.Status.AvailableReplicas,
			UpdatedReplicas:    it.Status.UpdatedReplicas,
			CreatedAt:          it.Metadata.CreationTimestamp,
			Generation:         it.Metadata.Generation,
			ObservedGeneration: it.Status.ObservedGeneration,
		}
		if it.Spec.Replicas != nil {
			d.Replicas = *it.Spec.Replicas
		}
		for _, ct := range it.Spec.Template.Spec.Containers {
			d.Containers = append(d.Containers, ct.Name)
			d.Images = append(d.Images, ct.Image)
		}
		out = append(out, d)
	}
	return out, nil
}

// ScaleDeployment sets a Deployment's replica count via the scale subresource.
func (c *Client) ScaleDeployment(ctx context.Context, namespace, name string, replicas int) error {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", url.PathEscape(namespace), url.PathEscape(name))
	body := []byte(`{"spec":{"replicas":` + strconv.Itoa(replicas) + `}}`)
	_, err := c.Do(ctx, Request{
		Method:      http.MethodPatch,
		Path:        path,
		Body:        body,
		ContentType: "application/merge-patch+json",
		Accept:      "application/json",
	})
	return err
}

// RestartDeployment triggers a rolling restart by stamping the pod template with
// a restart annotation — the same mechanism as `kubectl rollout restart`.
func (c *Client) RestartDeployment(ctx context.Context, namespace, name string) error {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", url.PathEscape(namespace), url.PathEscape(name))
	stamp := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"spinnaker-cli.avkcode.io/restartedAt":%q}}}}}`, stamp)
	_, err := c.Do(ctx, Request{
		Method:      http.MethodPatch,
		Path:        path,
		Body:        []byte(patch),
		ContentType: "application/merge-patch+json",
		Accept:      "application/json",
	})
	return err
}

// SetDeploymentImage sets one container's image on a Deployment.
func (c *Client) SetDeploymentImage(ctx context.Context, namespace, name, container, image string) error {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", url.PathEscape(namespace), url.PathEscape(name))
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []map[string]any{{"name": container, "image": image}},
				},
			},
		},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.Do(ctx, Request{
		Method:      http.MethodPatch,
		Path:        path,
		Body:        body,
		ContentType: "application/strategic-merge-patch+json",
		Accept:      "application/json",
	})
	return err
}

// Pod is the subset of a Pod this client reports on.
type Pod struct {
	Name       string
	Namespace  string
	Phase      string
	Ready      bool
	Restarts   int
	NodeName   string
	PodIP      string
	Containers []string
	StartedAt  time.Time
}

// ListPods returns the Pods in a namespace, optionally filtered by a selector.
func (c *Client) ListPods(ctx context.Context, namespace, labelSelector string) ([]Pod, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
			Spec struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Name string `json:"name"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase      string    `json:"phase"`
				PodIP      string    `json:"podIP"`
				StartTime  time.Time `json:"startTime"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					RestartCount int  `json:"restartCount"`
					Ready        bool `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(namespace))
	if err := c.GetJSON(ctx, path, q, &list); err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(list.Items))
	for _, it := range list.Items {
		p := Pod{
			Name:      it.Metadata.Name,
			Namespace: it.Metadata.Namespace,
			Phase:     it.Status.Phase,
			NodeName:  it.Spec.NodeName,
			PodIP:     it.Status.PodIP,
			StartedAt: it.Status.StartTime,
			Ready:     true,
		}
		for _, ct := range it.Spec.Containers {
			p.Containers = append(p.Containers, ct.Name)
		}
		if len(it.Status.ContainerStatuses) == 0 {
			p.Ready = false
		}
		for _, cs := range it.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
			if !cs.Ready {
				p.Ready = false
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// LogOptions parameterises a pod log read.
type LogOptions struct {
	Container    string
	Follow       bool
	TailLines    int
	Previous     bool
	Timestamps   bool
	SinceSeconds int
}

// PodLogs streams a Pod's container log. The caller closes the reader.
func (c *Client) PodLogs(ctx context.Context, namespace, pod string, opts LogOptions) (io.ReadCloser, error) {
	q := url.Values{}
	if opts.Container != "" {
		q.Set("container", opts.Container)
	}
	if opts.Follow {
		q.Set("follow", "true")
	}
	if opts.TailLines > 0 {
		q.Set("tailLines", strconv.Itoa(opts.TailLines))
	}
	if opts.Previous {
		q.Set("previous", "true")
	}
	if opts.Timestamps {
		q.Set("timestamps", "true")
	}
	if opts.SinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.Itoa(opts.SinceSeconds))
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", url.PathEscape(namespace), url.PathEscape(pod))
	// No Accept header: the log subresource streams plain text but the API server
	// content-negotiates against its own serializers and answers 406 to
	// "text/plain". kubectl likewise sends none.
	return c.Stream(ctx, Request{Method: http.MethodGet, Path: path, Query: q})
}

// GetConfigMap returns a ConfigMap's data.
func (c *Client) GetConfigMap(ctx context.Context, namespace, name string) (map[string]string, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps/%s", url.PathEscape(namespace), url.PathEscape(name))
	if err := c.GetJSON(ctx, path, nil, &cm); err != nil {
		return nil, err
	}
	return cm.Data, nil
}

// ListConfigMaps returns the ConfigMap names in a namespace.
func (c *Client) ListConfigMaps(ctx context.Context, namespace, labelSelector string) ([]string, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/configmaps", url.PathEscape(namespace))
	if err := c.GetJSON(ctx, path, q, &list); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Metadata.Name)
	}
	return out, nil
}

// ListEvents returns a namespace's events, newest last.
func (c *Client) ListEvents(ctx context.Context, namespace, fieldSelector string) ([]map[string]any, error) {
	q := url.Values{}
	if fieldSelector != "" {
		q.Set("fieldSelector", fieldSelector)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(namespace))
	if err := c.GetJSON(ctx, path, q, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// setEscapedPath assigns an already percent-encoded path to u.
//
// url.URL.Path holds the *decoded* path, so assigning a pre-encoded string there
// makes URL.String() escape it a second time ("%20" becoming "%2520"). Setting
// RawPath alongside the decoded Path makes EscapedPath() preserve the encoding.
func setEscapedPath(u *url.URL, escaped string) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		u.Path, u.RawPath = escaped, ""
		return
	}
	u.Path, u.RawPath = decoded, escaped
}
