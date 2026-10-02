// Package gate is a client for Spinnaker's Gate API — the single
// authenticated entrypoint in front of orca, clouddriver, front50, igor, echo,
// fiat, rosco, kayenta and keel.
//
// Gate is a Spring Boot service whose endpoints are frequently mounted under a
// servlet context path (the upstream kustomize install uses /api/v1 so a single
// ingress can route Deck and the API on one host without CORS). Endpoint is
// therefore the full API base including that prefix, e.g.
// http://spinnaker.example.com/api/v1 — not just the host.
package gate

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a single Gate request.
const DefaultTimeout = 60 * time.Second

// APITokenPrefix is the value prefix Gate uses for its long-lived API tokens.
// Tokens carrying it are sent in X-Spinnaker-Token (which survives IAP and other
// proxies that consume the Authorization header) rather than as a bearer token.
const APITokenPrefix = "spk_"

// Config describes how to reach and authenticate against Gate.
type Config struct {
	// Endpoint is the API base URL, including any servlet context path.
	Endpoint string

	// Basic-auth credentials (security.basicform.enabled installs).
	User     string
	Password string

	// Token is either a Gate API token (spk_…, sent as X-Spinnaker-Token) or an
	// OAuth2/OIDC bearer token (sent as Authorization: Bearer).
	Token string

	// Client-certificate auth (x509).
	CertFile string
	KeyFile  string

	Insecure bool
	Timeout  time.Duration

	// UserAgent identifies this client to Gate's access logs.
	UserAgent string
}

// Client talks to a Gate instance.
type Client struct {
	cfg  Config
	base *url.URL
	http *http.Client
}

// New validates cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("gate endpoint is required")
	}
	raw := strings.TrimRight(cfg.Endpoint, "/")
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid gate endpoint %q: %w", cfg.Endpoint, err)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("invalid gate endpoint %q: missing host", cfg.Endpoint)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "spinnaker-cli"
	}

	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure} //nolint:gosec // opt-in via --insecure
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			return nil, errors.New("both --cert and --key are required for x509 auth")
		}
		pair, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{pair}
	}

	return &Client{
		cfg:  cfg,
		base: base,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// Endpoint returns the configured API base URL.
func (c *Client) Endpoint() string { return c.base.String() }

// APIError is a non-2xx response from Gate.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 600 {
		body = body[:600] + "…"
	}
	msg := fmt.Sprintf("%s %s: gate returned %d %s", e.Method, e.Path, e.Status, http.StatusText(e.Status))
	// Gate answers an unauthenticated API call with the login page rather than a
	// 401, so an HTML body is the real signal that auth is missing or wrong.
	if looksLikeHTML(body) {
		return msg + ": received an HTML page instead of JSON, which means the request was not authenticated " +
			"(Gate redirected to its login page). Check the endpoint, user and token — 'sc login' / 'sc context use'"
	}
	if body != "" {
		msg += ": " + body
	}
	return msg
}

// NotFound reports whether the error is a 404.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

func looksLikeHTML(body string) bool {
	b := strings.ToLower(strings.TrimSpace(body))
	return strings.HasPrefix(b, "<!doctype html") || strings.HasPrefix(b, "<html")
}

// IsNotFound reports whether err is a 404 from Gate.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.NotFound()
	}
	return false
}

// Request is a single Gate call.
type Request struct {
	Method string
	Path   string // relative to the endpoint, with or without a leading slash
	Query  url.Values
	Body   any // marshalled as JSON when non-nil; []byte and string are sent verbatim
	// Accept overrides the Accept header (defaults to application/json).
	Accept string
	// Headers are extra request headers, applied after the defaults so they can
	// override Content-Type or Accept.
	Headers map[string]string
}

// Raw performs req and returns the status code and undecoded response body.
func (c *Client) Raw(ctx context.Context, req Request) (int, []byte, error) {
	u := *c.base
	setEscapedPath(&u, strings.TrimRight(u.EscapedPath(), "/")+"/"+strings.TrimLeft(req.Path, "/"))
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}

	var body io.Reader
	contentType := ""
	switch b := req.Body.(type) {
	case nil:
	case []byte:
		body, contentType = bytes.NewReader(b), "application/json"
	case string:
		body, contentType = strings.NewReader(b), "application/json"
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
		body, contentType = bytes.NewReader(buf), "application/json"
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
	if err != nil {
		return 0, nil, err
	}
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", c.cfg.UserAgent)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	c.authenticate(httpReq)
	// Caller-supplied headers win, so an explicit Content-Type or Accept can
	// override the defaults chosen above.
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, raw, &APIError{Method: req.Method, Path: req.Path, Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.StatusCode, raw, nil
}

// authenticate applies whichever credential the config carries. Gate accepts an
// API token, an OAuth2 bearer token, basic auth or a client certificate; the
// first configured one wins.
func (c *Client) authenticate(req *http.Request) {
	switch {
	case strings.HasPrefix(c.cfg.Token, APITokenPrefix):
		req.Header.Set("X-Spinnaker-Token", c.cfg.Token)
	case c.cfg.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	case c.cfg.User != "":
		req.SetBasicAuth(c.cfg.User, c.cfg.Password)
	}
}

// do performs req and decodes a JSON response into out (ignored when nil).
func (c *Client) do(ctx context.Context, req Request, out any) error {
	_, raw, err := c.Raw(ctx, req)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decoding response: %w", req.Method, req.Path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, Request{Method: http.MethodGet, Path: path, Query: q}, out)
}

func (c *Client) post(ctx context.Context, path string, q url.Values, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPost, Path: path, Query: q, Body: body}, out)
}

func (c *Client) put(ctx context.Context, path string, q url.Values, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPut, Path: path, Query: q, Body: body}, out)
}

func (c *Client) patch(ctx context.Context, path string, q url.Values, body, out any) error {
	return c.do(ctx, Request{Method: http.MethodPatch, Path: path, Query: q, Body: body}, out)
}

func (c *Client) delete(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, Request{Method: http.MethodDelete, Path: path, Query: q}, out)
}

// escape path-escapes a single URL path segment. Application, pipeline, account
// and manifest names routinely contain characters that must not be read as path
// separators.
func escape(segment string) string { return url.PathEscape(segment) }

// setEscapedPath assigns an already percent-encoded path to u.
//
// Assigning a pre-encoded string to url.URL.Path is wrong: that field holds the
// *decoded* path, so URL.String() escapes it again and a manifest name like
// "deployment nginx" — correctly sent as "deployment%20nginx" — would go out as
// "deployment%2520nginx". Setting RawPath alongside the decoded Path makes
// EscapedPath() return the encoding we chose.
func setEscapedPath(u *url.URL, escaped string) {
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		// Not valid percent-encoding; treat it as a literal path and let net/url
		// encode it, which is the best available interpretation.
		u.Path, u.RawPath = escaped, ""
		return
	}
	u.Path, u.RawPath = decoded, escaped
}

// escapeApplication escapes an application path segment, leaving the "*"
// wildcard literal.
//
// The execution-search endpoint documents "*" as "every application", but Spring
// matches the path variable against the raw segment: percent-encoded as %2A it no
// longer reads as the wildcard and Gate answers 400.
func escapeApplication(app string) string {
	if app == "*" {
		return "*"
	}
	return url.PathEscape(app)
}

// JSONMap is a decoded JSON object. Most Gate responses are loosely typed
// pass-throughs of a downstream service's model, so the client keeps them as
// maps rather than inventing structs that would drift from the server.
type JSONMap = map[string]any

// JSONList is a decoded JSON array of objects.
type JSONList = []map[string]any
