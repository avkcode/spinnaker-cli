package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestNewValidatesEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
		wantBase string
	}{
		{name: "empty", endpoint: "", wantErr: true},
		{name: "with scheme", endpoint: "http://host/api/v1", wantBase: "http://host/api/v1"},
		{name: "trailing slash trimmed", endpoint: "http://host/api/v1/", wantBase: "http://host/api/v1"},
		{name: "scheme defaulted", endpoint: "host:8084", wantBase: "http://host:8084"},
		{name: "https preserved", endpoint: "https://host/api/v1", wantBase: "https://host/api/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(Config{Endpoint: tc.endpoint})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := c.Endpoint(); got != tc.wantBase {
				t.Errorf("endpoint = %q, want %q", got, tc.wantBase)
			}
		})
	}
}

func TestNewRequiresBothCertAndKey(t *testing.T) {
	if _, err := New(Config{Endpoint: "http://host", CertFile: "cert.pem"}); err == nil {
		t.Fatal("expected an error when only --cert is supplied")
	}
	if _, err := New(Config{Endpoint: "http://host", KeyFile: "key.pem"}); err == nil {
		t.Fatal("expected an error when only --key is supplied")
	}
}

// TestRawPreservesPreEscapedPath guards the bug where a pre-escaped path segment
// was assigned to url.URL.Path (the decoded field) and escaped a second time, so
// a manifest named "deployment nginx" went out as "deployment%2520nginx".
func TestRawPreservesPreEscapedPath(t *testing.T) {
	var gotRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, err := New(Config{Endpoint: srv.URL + "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetManifest(context.Background(), "managing", "demo", "deployment nginx"); err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	want := "/api/v1/manifests/managing/demo/deployment%20nginx"
	if gotRawPath != want {
		t.Errorf("escaped path = %q, want %q", gotRawPath, want)
	}
}

// TestSearchExecutionsKeepsWildcardLiteral guards the wildcard application:
// Gate matches the path variable against the raw segment, so "*" must not be
// percent-encoded or the endpoint answers 400.
func TestSearchExecutionsKeepsWildcardLiteral(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c, _ := New(Config{Endpoint: srv.URL})
	if _, err := c.SearchExecutions(context.Background(), SearchExecutionsOptions{Application: "*"}); err != nil {
		t.Fatalf("SearchExecutions: %v", err)
	}
	if !strings.Contains(gotPath, "/applications/*/executions/search") {
		t.Errorf("path = %q, want a literal * in the application segment", gotPath)
	}
}

func TestListExecutionsRejectsWildcard(t *testing.T) {
	c, _ := New(Config{Endpoint: "http://host"})
	_, err := c.ListExecutions(context.Background(), "*", ListExecutionsOptions{})
	if err == nil {
		t.Fatal("expected an error: this endpoint does not accept a wildcard application")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error = %q, want it to mention the wildcard", err)
	}
}

func TestAuthenticateSelectsCredential(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		wantHeader string
		wantValue  string
	}{
		{
			name:       "api token uses X-Spinnaker-Token",
			cfg:        Config{Endpoint: "http://host", Token: "spk_abc"},
			wantHeader: "X-Spinnaker-Token", wantValue: "spk_abc",
		},
		{
			name:       "bearer token",
			cfg:        Config{Endpoint: "http://host", Token: "oauth-xyz"},
			wantHeader: "Authorization", wantValue: "Bearer oauth-xyz",
		},
		{
			name:       "basic auth",
			cfg:        Config{Endpoint: "http://host", User: "admin", Password: "pw"},
			wantHeader: "Authorization", wantValue: "Basic YWRtaW46cHc=",
		},
		{
			// A token must win over basic auth so that an explicit --token is not
			// silently ignored when a context also carries a user.
			name:       "token wins over basic auth",
			cfg:        Config{Endpoint: "http://host", User: "admin", Password: "pw", Token: "spk_abc"},
			wantHeader: "X-Spinnaker-Token", wantValue: "spk_abc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodGet, "http://host/x", nil)
			c.authenticate(req)
			if got := req.Header.Get(tc.wantHeader); got != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
			}
		})
	}
}

func TestAPIErrorExplainsHTMLResponse(t *testing.T) {
	err := &APIError{Method: "GET", Path: "/applications", Status: 200, Body: "<!DOCTYPE html><html><body>login</body></html>"}
	msg := err.Error()
	if !strings.Contains(msg, "not authenticated") {
		t.Errorf("an HTML body should be explained as an auth failure, got: %s", msg)
	}
}

func TestIsNotFound(t *testing.T) {
	if !IsNotFound(&APIError{Status: 404}) {
		t.Error("404 should be reported as not found")
	}
	if IsNotFound(&APIError{Status: 500}) {
		t.Error("500 should not be reported as not found")
	}
}

func TestStatusHelpers(t *testing.T) {
	terminal := []string{StatusSucceeded, StatusTerminal, StatusCanceled, StatusStopped, StatusSkipped, StatusFailedCont}
	for _, s := range terminal {
		if !IsTerminalStatus(s) {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []string{StatusRunning, StatusPaused, StatusNotStarted, StatusBuffered} {
		if IsTerminalStatus(s) {
			t.Errorf("%s should not be terminal", s)
		}
	}
	for _, s := range []string{StatusTerminal, StatusCanceled, StatusStopped, StatusFailedCont} {
		if !IsFailureStatus(s) {
			t.Errorf("%s should be a failure", s)
		}
	}
	if IsFailureStatus(StatusSucceeded) {
		t.Error("SUCCEEDED is not a failure")
	}
	// Status comparison must be case-insensitive: these values are read back out
	// of loosely-typed JSON.
	if !IsTerminalStatus("succeeded") {
		t.Error("status comparison should be case-insensitive")
	}
}

func TestLatestExecutionsRejectsConflictingFilters(t *testing.T) {
	c, _ := New(Config{Endpoint: "http://host"})
	ctx := context.Background()
	if _, err := c.LatestExecutions(ctx, LatestExecutionsOptions{}); err == nil {
		t.Error("expected an error when neither filter is supplied")
	}
	_, err := c.LatestExecutions(ctx, LatestExecutionsOptions{
		PipelineConfigIDs: []string{"a"},
		ExecutionIDs:      []string{"b"},
	})
	if err == nil {
		t.Error("expected an error when both filters are supplied: Gate throws on that combination")
	}
}

func TestRunPipelineReturnsExecutionIDFromRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var trigger map[string]any
		_ = json.NewDecoder(r.Body).Decode(&trigger)
		if trigger["type"] != "manual" {
			t.Errorf("trigger type = %v, want manual", trigger["type"])
		}
		_, _ = w.Write([]byte(`{"ref":"/pipelines/01ABC"}`))
	}))
	defer srv.Close()

	c, _ := New(Config{Endpoint: srv.URL})
	id, err := c.RunPipeline(context.Background(), "demo", "deploy", RunPipelineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if id != "01ABC" {
		t.Errorf("execution id = %q, want 01ABC (the ref must be reduced to the bare id)", id)
	}
}

func TestJudgeStageValidatesAndCapitalises(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL})
	ctx := context.Background()

	if _, err := c.JudgeStage(ctx, "e", "s", "maybe", ""); err == nil {
		t.Error("expected an error for an invalid judgment")
	}
	if _, err := c.JudgeStage(ctx, "e", "s", "continue", "proceed"); err != nil {
		t.Fatal(err)
	}
	// orca expects a capitalised judgmentStatus.
	if body["judgmentStatus"] != "Continue" {
		t.Errorf("judgmentStatus = %v, want Continue", body["judgmentStatus"])
	}
	if body["judgmentInput"] != "proceed" {
		t.Errorf("judgmentInput = %v, want proceed", body["judgmentInput"])
	}
}

func TestTaskFailureMessage(t *testing.T) {
	task := map[string]any{
		"variables": []any{
			map[string]any{"key": "exception", "value": map[string]any{
				"details": map[string]any{"errors": []any{"first problem", "second problem"}},
			}},
		},
	}
	if got := TaskFailureMessage(task); got != "first problem; second problem" {
		t.Errorf("message = %q, want both errors joined", got)
	}

	fromStage := map[string]any{
		"steps": []any{
			map[string]any{"status": "SUCCEEDED", "context": map[string]any{}},
			map[string]any{"status": "TERMINAL", "context": map[string]any{
				"exception": map[string]any{"details": map[string]any{"error": "stage blew up"}},
			}},
		},
	}
	if got := TaskFailureMessage(fromStage); got != "stage blew up" {
		t.Errorf("message = %q, want the failed step's error", got)
	}

	if got := TaskFailureMessage(map[string]any{"status": "SUCCEEDED"}); got != "" {
		t.Errorf("message = %q, want empty for a successful task", got)
	}
}

func TestCreateTaskRequiresJob(t *testing.T) {
	c, _ := New(Config{Endpoint: "http://host"})
	if _, err := c.CreateTask(context.Background(), TaskRequest{Application: "demo"}); err == nil {
		t.Error("expected an error when no job operations are supplied")
	}
}

func TestListPendingJudgmentsFindsRunningManualJudgment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The wildcard path must reach the search endpoint, not the per-application
		// one, since only search accepts "*".
		if !strings.Contains(r.URL.EscapedPath(), "/executions/search") {
			t.Errorf("wildcard lookup used %q, want the search endpoint", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`[{
			"id":"01EXEC","application":"demo","name":"deploy","status":"RUNNING",
			"stages":[
				{"id":"s1","name":"Build","type":"script","status":"SUCCEEDED"},
				{"id":"s2","name":"Approve","type":"manualJudgment","status":"RUNNING","startTime":1000,
				 "context":{"instructions":"ok?","judgmentInputs":[{"value":"yes"},{"value":"no"}]}},
				{"id":"s3","name":"Old approve","type":"manualJudgment","status":"SUCCEEDED"}
			]}]`))
	}))
	defer srv.Close()

	c, _ := New(Config{Endpoint: srv.URL})
	pending, err := c.ListPendingJudgments(context.Background(), "*", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("found %d pending judgments, want 1 (only the RUNNING manualJudgment)", len(pending))
	}
	p := pending[0]
	if p.StageID != "s2" || p.ExecutionID != "01EXEC" || p.Pipeline != "deploy" {
		t.Errorf("unexpected judgment: %+v", p)
	}
	if p.Instructions != "ok?" {
		t.Errorf("instructions = %q", p.Instructions)
	}
	if len(p.Inputs) != 2 || p.Inputs[0] != "yes" {
		t.Errorf("inputs = %v, want the judgmentInputs values", p.Inputs)
	}
}

func TestRestartStageWithOverridesPatchesFirst(t *testing.T) {
	var calls []string
	var patched map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		if r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&patched)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := New(Config{Endpoint: srv.URL})
	if _, err := c.RestartStageWithOverrides(context.Background(), "e1", "s1", JSONMap{"url": "http://fixed"}); err != nil {
		t.Fatal(err)
	}
	// The restart body never reaches a normal stage, so the context must be
	// patched before the restart is issued, in that order.
	if len(calls) != 2 {
		t.Fatalf("made %d calls (%v), want a PATCH then a PUT", len(calls), calls)
	}
	if !strings.HasPrefix(calls[0], "PATCH ") {
		t.Errorf("first call = %q, want the PATCH", calls[0])
	}
	if !strings.Contains(calls[1], "/restart") {
		t.Errorf("second call = %q, want the restart", calls[1])
	}
	if patched["url"] != "http://fixed" {
		t.Errorf("patched context = %v, want the override applied", patched)
	}
}

func TestRestartStageWithoutOverridesSkipsPatch(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, _ := New(Config{Endpoint: srv.URL})
	if _, err := c.RestartStageWithOverrides(context.Background(), "e1", "s1", nil); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != http.MethodPut {
		t.Errorf("calls = %v, want a single PUT", calls)
	}
}

func TestSetPipelineConfigDisabledRoundTrips(t *testing.T) {
	var saved map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"application":"demo","name":"deploy","id":"abc","disabled":false}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&saved)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := New(Config{Endpoint: srv.URL})
	if err := c.SetPipelineConfigDisabled(context.Background(), "demo", "deploy", true); err != nil {
		t.Fatal(err)
	}
	if saved["disabled"] != true {
		t.Errorf("saved disabled = %v, want true", saved["disabled"])
	}
	// The rest of the definition must survive: this is a read-modify-write.
	if saved["id"] != "abc" || saved["name"] != "deploy" {
		t.Errorf("saved definition lost fields: %v", saved)
	}
}

func TestSavePipelineConfigRequiresIdentity(t *testing.T) {
	c, _ := New(Config{Endpoint: "http://host"})
	err := c.SavePipelineConfig(context.Background(), JSONMap{"name": "deploy"})
	if err == nil || !strings.Contains(err.Error(), "application") {
		t.Errorf("expected a complaint about the missing application, got %v", err)
	}
}

func TestEscapeApplication(t *testing.T) {
	if got := escapeApplication("*"); got != "*" {
		t.Errorf("wildcard = %q, want it left literal", got)
	}
	if got := escapeApplication("my app"); got != url.PathEscape("my app") {
		t.Errorf("normal name = %q, want it escaped", got)
	}
}

func TestSetEscapedPathFallsBackOnBadEncoding(t *testing.T) {
	u := &url.URL{}
	setEscapedPath(u, "/a%zzb")
	// Invalid percent-encoding cannot be decoded, so it is treated as a literal
	// path rather than silently dropped.
	if u.Path != "/a%zzb" || u.RawPath != "" {
		t.Errorf("Path=%q RawPath=%q, want the literal path with no RawPath", u.Path, u.RawPath)
	}
}
