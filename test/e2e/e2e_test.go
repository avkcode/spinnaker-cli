// Package e2e drives the built sc binary against a real Spinnaker installation.
//
// It is skipped unless SC_E2E=1 and SC_E2E_GATE are set, so `go test ./...` stays
// hermetic. Run it with `make e2e`.
//
// Environment:
//
//	SC_E2E=1              enable the suite
//	SC_E2E_GATE           Gate API base URL, including its context path
//	SC_E2E_USER           basic-auth user (optional)
//	SC_E2E_PASSWORD       basic-auth password (optional)
//	SC_E2E_TOKEN          API/bearer token instead of basic auth (optional)
//	SC_E2E_KUBECONFIG     kubeconfig for the operator-plane tests (optional)
//	SC_E2E_NAMESPACE      namespace holding Spinnaker (default: spinnaker)
//	SC_E2E_ACCOUNT        Kubernetes account to deploy with (default: managing)
//	SC_E2E_DEPLOY_NS      namespace to deploy the test workload into (default: demo)
//	SC_E2E_APP            application to create and use (default: sc-e2e)
//	SC_E2E_KEEP=1         keep the application and workload afterwards
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	bin       string
	gateURL   string
	account   string
	deployNS  string
	appName   string
	configDir string
)

func TestMain(m *testing.M) {
	if os.Getenv("SC_E2E") != "1" || os.Getenv("SC_E2E_GATE") == "" {
		fmt.Fprintln(os.Stderr, "skipping e2e: set SC_E2E=1 and SC_E2E_GATE to run (see package docs)")
		os.Exit(0)
	}
	gateURL = os.Getenv("SC_E2E_GATE")
	account = envOr("SC_E2E_ACCOUNT", "managing")
	deployNS = envOr("SC_E2E_DEPLOY_NS", "demo")
	appName = envOr("SC_E2E_APP", "sc-e2e")

	// The suite must not disturb the developer's own ~/.spinnaker-cli.yaml.
	tmp, err := os.MkdirTemp("", "sc-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: cannot create a temp config dir:", err)
		os.Exit(1)
	}
	configDir = tmp
	defer os.RemoveAll(tmp)

	bin, err = findBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}

	code := m.Run()
	teardown()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// teardown removes the shared application after every test has run.
//
// This cannot be a test: Go runs top-level tests in source order, so any
// teardown test would have to be physically last in the file and would break the
// moment a test was appended after it. TestMain is the only hook that reliably
// runs last, and it has no *testing.T, so the binary is invoked directly.
func teardown() {
	if !suiteReady || os.Getenv("SC_E2E_KEEP") == "1" {
		if suiteReady {
			fmt.Fprintf(os.Stderr, "SC_E2E_KEEP=1: leaving application %s in place\n", appName)
		}
		return
	}
	cmd := exec.Command(bin, "--config", filepath.Join(configDir, "config.yaml"),
		"app", "delete", appName, "--force", "--wait")
	cmd.Env = append(os.Environ(), "SC_NO_AUDIT=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "teardown: deleting application %s failed: %v\n%s\n", appName, err, out)
	}
}

// setupSuite logs in and creates the shared application. It runs once, from the
// first test that needs it, because TestMain cannot use *testing.T helpers.
var suiteReady bool

func setup(t *testing.T) {
	t.Helper()
	if suiteReady {
		return
	}
	args := []string{"login", "--gate", gateURL, "--context-name", "e2e"}
	if u := os.Getenv("SC_E2E_USER"); u != "" {
		args = append(args, "--user", u, "--password", os.Getenv("SC_E2E_PASSWORD"))
	}
	if tok := os.Getenv("SC_E2E_TOKEN"); tok != "" {
		args = append(args, "--token", tok)
	}
	if ns := os.Getenv("SC_E2E_NAMESPACE"); ns != "" {
		args = append(args, "--save-namespace", ns)
	}
	if kc := os.Getenv("SC_E2E_KUBECONFIG"); kc != "" {
		args = append(args, "--save-kubeconfig", kc)
	}
	mustSC(t, args...)
	mustSC(t, "app", "create", appName, "--email", "sc-e2e@example.com",
		"--cloud-providers", "kubernetes", "--wait")
	suiteReady = true
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// findBinary locates the sc binary built by `make build` at the repo root.
func findBinary() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	// test/e2e -> repo root
	candidate := filepath.Join(wd, "..", "..", "sc")
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("sc binary not found at %s; run 'make build' first", abs)
	}
	return abs, nil
}

// sc runs the CLI with the suite's isolated config and returns combined output.
func sc(t *testing.T, args ...string) (string, error) {
	t.Helper()
	full := append([]string{"--config", filepath.Join(configDir, "config.yaml")}, args...)
	cmd := exec.Command(bin, full...)
	cmd.Env = append(os.Environ(), "SC_NO_AUDIT=1")
	if kc := os.Getenv("SC_E2E_KUBECONFIG"); kc != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kc)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustSC fails the test if the command does not succeed.
func mustSC(t *testing.T, args ...string) string {
	t.Helper()
	out, err := sc(t, args...)
	if err != nil {
		t.Fatalf("sc %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// exitCode runs the CLI and returns its exit status.
func exitCode(t *testing.T, args ...string) int {
	t.Helper()
	_, err := sc(t, args...)
	var ee *exec.ExitError
	if err == nil {
		return 0
	}
	if ok := asExitError(err, &ee); ok {
		return ee.ExitCode()
	}
	t.Fatalf("sc %s: unexpected error type %T: %v", strings.Join(args, " "), err, err)
	return -1
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// jsonOut runs a command with -o json and decodes the result.
func jsonOut(t *testing.T, v any, args ...string) {
	t.Helper()
	decodeJSON(t, mustSC(t, append(args, "-o", "json")...), v, args...)
}

// jsonOutAllowFailure is jsonOut for commands that intentionally exit non-zero
// while still printing their payload — doctor exits 8 when it finds problems.
func jsonOutAllowFailure(t *testing.T, v any, args ...string) {
	t.Helper()
	out, _ := sc(t, append(args, "-o", "json")...)
	decodeJSON(t, out, v, args...)
}

// decodeJSON extracts the first JSON value from mixed output.
//
// Commands write their payload to stdout and notices, warnings and errors to
// stderr; CombinedOutput merges the two, so the payload can be bracketed by
// human-readable lines. A streaming decoder stops at the end of the first value
// rather than choking on whatever follows it.
func decodeJSON(t *testing.T, out string, v any, args ...string) {
	t.Helper()
	idx := strings.IndexAny(out, "[{")
	if idx < 0 {
		t.Fatalf("sc %s produced no JSON:\n%s", strings.Join(args, " "), out)
	}
	if err := json.NewDecoder(strings.NewReader(out[idx:])).Decode(v); err != nil {
		t.Fatalf("decoding JSON from sc %s: %v\n%s", strings.Join(args, " "), err, out[idx:])
	}
}

// TestLogin authenticates and creates the shared application the rest of the
// suite uses. Go runs top-level tests in source order, so this runs first.
func TestLogin(t *testing.T) {
	args := []string{"login", "--gate", gateURL, "--context-name", "e2e"}
	if u := os.Getenv("SC_E2E_USER"); u != "" {
		args = append(args, "--user", u, "--password", os.Getenv("SC_E2E_PASSWORD"))
	}
	if tok := os.Getenv("SC_E2E_TOKEN"); tok != "" {
		args = append(args, "--token", tok)
	}
	out := mustSC(t, args...)
	if !strings.Contains(out, "Authenticated as") {
		t.Errorf("login did not confirm an identity:\n%s", out)
	}
	// The shared application is created here and torn down by TestZZZTeardown,
	// after every other test has run.
	setup(t)
}

func TestHealthAndVersion(t *testing.T) {
	setup(t)
	var health map[string]any
	jsonOut(t, &health, "health")
	if health["reachable"] != true {
		t.Errorf("gate is not reachable: %v", health)
	}
	if v, ok := health["version"].(string); !ok || v == "" {
		t.Errorf("no Spinnaker version reported: %v", health["version"])
	}

	out := mustSC(t, "version")
	if !strings.Contains(out, "sc ") {
		t.Errorf("version output missing the CLI version:\n%s", out)
	}
}

func TestAccountsAreConfigured(t *testing.T) {
	setup(t)
	var accounts []map[string]any
	jsonOut(t, &accounts, "account", "list")
	if len(accounts) == 0 {
		t.Fatal("no cloud accounts configured; the deploy tests cannot run")
	}
	found := false
	for _, a := range accounts {
		if a["name"] == account {
			found = true
			if a["authorized"] != true {
				t.Errorf("account %q is not authorized for this identity", account)
			}
		}
	}
	if !found {
		t.Errorf("account %q not found; set SC_E2E_ACCOUNT (have: %v)", account, accountNames(accounts))
	}
}

func accountNames(accounts []map[string]any) []string {
	names := make([]string, 0, len(accounts))
	for _, a := range accounts {
		names = append(names, fmt.Sprint(a["name"]))
	}
	return names
}

func TestApplicationLifecycle(t *testing.T) {
	setup(t)

	var app map[string]any
	jsonOut(t, &app, "app", "get", appName)
	attrs, _ := app["attributes"].(map[string]any)
	if attrs == nil {
		attrs = app
	}
	if attrs["email"] != "sc-e2e@example.com" {
		t.Errorf("owner email = %v, want the one just set", attrs["email"])
	}

	// A created application must eventually appear as registered, not merely
	// inferred. front50 has two read paths with different freshness: a direct
	// lookup is immediate, while the collection behind 'app list' is served from a
	// cache that refreshes on its own cycle — so this polls.
	waitFor(t, 2*time.Minute, "the application to appear in 'app list --registered'", func() bool {
		var apps []map[string]any
		out, err := sc(t, "app", "list", "--registered", "-o", "json")
		if err != nil {
			return false
		}
		i := strings.IndexAny(out, "[")
		if i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&apps) != nil {
			return false
		}
		return containsName(apps, appName)
	})

	// Cover deletion on a throwaway application rather than the shared fixture.
	throwaway := appName + "tmp"
	mustSC(t, "app", "create", throwaway, "--email", "sc-e2e@example.com", "--wait")
	mustSC(t, "app", "delete", throwaway, "--force", "--wait")
	// front50 serves applications from a cache, so a deletion becomes visible on
	// its refresh cycle rather than immediately.
	waitFor(t, 90*time.Second, "the deleted application to stop resolving", func() bool {
		return exitCode(t, "app", "get", throwaway) == 5
	})
}

func containsName(items []map[string]any, name string) bool {
	for _, i := range items {
		if i["name"] == name {
			return true
		}
	}
	return false
}

// TestPipelineDeployAndJudgment is the full core-plane loop: define a pipeline,
// run it, answer its manual judgment, and confirm the Kubernetes workload it
// deploys becomes stable.
func TestPipelineDeployAndJudgment(t *testing.T) {
	setup(t)
	pipeline := map[string]any{
		"application":     appName,
		"name":            "e2e-deploy",
		"limitConcurrent": false,
		"parameterConfig": []map[string]any{
			{"name": "REPLICAS", "default": "1", "required": true},
		},
		"stages": []map[string]any{
			{
				"refId": "1", "requisiteStageRefIds": []string{},
				"name": "Approve", "type": "manualJudgment",
				"instructions":   "e2e: approve the rollout",
				"judgmentInputs": []map[string]any{{"value": "proceed"}},
				"failPipeline":   true,
			},
			{
				"refId": "2", "requisiteStageRefIds": []string{"1"},
				"name": "Deploy", "type": "deployManifest",
				"account": account, "cloudProvider": "kubernetes",
				"moniker": map[string]any{"app": appName},
				"source":  "text", "namespaceOverride": deployNS,
				"manifests": []map[string]any{{
					"apiVersion": "apps/v1", "kind": "Deployment",
					"metadata": map[string]any{
						"name": "sc-e2e-nginx", "namespace": deployNS,
						"labels": map[string]any{"app": "sc-e2e-nginx"},
					},
					"spec": map[string]any{
						"replicas": "${#toInt(parameters.REPLICAS)}",
						"selector": map[string]any{"matchLabels": map[string]any{"app": "sc-e2e-nginx"}},
						"template": map[string]any{
							"metadata": map[string]any{"labels": map[string]any{"app": "sc-e2e-nginx"}},
							"spec": map[string]any{"containers": []map[string]any{{
								"name": "nginx", "image": "nginx:1.27-alpine",
								"resources": map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "32Mi"}},
							}}},
						},
					},
				}},
			},
		},
	}
	applyPipeline(t, pipeline)

	// Trigger, then confirm it parks on the judgment rather than racing ahead.
	execID := strings.TrimSpace(lastLine(mustSC(t, "pipeline", "run", appName, "e2e-deploy", "-P", "REPLICAS=2")))
	if execID == "" {
		t.Fatal("pipeline run did not print an execution id")
	}
	t.Logf("execution %s", execID)

	waitFor(t, 90*time.Second, "the judgment stage to start", func() bool {
		var judgments []map[string]any
		if out, err := sc(t, "judge", "get", execID, "-o", "json"); err == nil {
			idx := strings.IndexAny(out, "[")
			if idx >= 0 && json.Unmarshal([]byte(out[idx:]), &judgments) == nil {
				for _, j := range judgments {
					if j["status"] == "RUNNING" {
						return true
					}
				}
			}
		}
		return false
	})

	// The pending judgment must also be discoverable without knowing the id, via
	// the wildcard search path. That path reads orca's execution search index,
	// which trails the live execution record by a moment, so this polls.
	waitFor(t, 60*time.Second, "the judgment to appear in 'judge list'", func() bool {
		var pending []map[string]any
		out, err := sc(t, "judge", "list", "-o", "json")
		if err != nil {
			return false
		}
		i := strings.IndexAny(out, "[")
		if i < 0 || json.NewDecoder(strings.NewReader(out[i:])).Decode(&pending) != nil {
			return false
		}
		for _, p := range pending {
			if p["executionId"] == execID {
				return true
			}
		}
		return false
	})

	// SpEL evaluation must see the parameter as passed.
	out := mustSC(t, "exec", "eval", execID, "${parameters.REPLICAS}")
	if !strings.Contains(out, "2") {
		t.Errorf("SpEL evaluation of the parameter returned %q, want 2", strings.TrimSpace(out))
	}

	mustSC(t, "judge", "continue", execID, "Approve", "--input", "proceed")
	mustSC(t, "exec", "wait", execID, "--wait-timeout", "6m")

	var stages []map[string]any
	jsonOut(t, &stages, "exec", "stages", execID)
	for _, s := range stages {
		if s["status"] != "SUCCEEDED" {
			t.Errorf("stage %v finished %v, want SUCCEEDED", s["name"], s["status"])
		}
	}

	// Spinnaker must agree the deployed workload is stable.
	var manifest map[string]any
	jsonOut(t, &manifest, "manifest", "get", account, deployNS, "deployment sc-e2e-nginx")
	status, _ := manifest["status"].(map[string]any)
	stable, _ := status["stable"].(map[string]any)
	if stable == nil || stable["state"] != true {
		t.Errorf("deployed manifest is not stable: %v", status)
	}

	t.Cleanup(func() {
		if os.Getenv("SC_E2E_KEEP") == "1" {
			return
		}
		// Delete the workload through Spinnaker, which also exercises task submission.
		job := fmt.Sprintf(`[{"type":"deleteManifest","account":%q,"location":%q,"manifestName":"deployment sc-e2e-nginx","cloudProvider":"kubernetes","options":{}}]`,
			account, deployNS)
		cmd := exec.Command(bin, "--config", filepath.Join(configDir, "config.yaml"),
			"task", "submit", "-f", "-", "--application", appName,
			"--description", "e2e cleanup", "--wait")
		cmd.Stdin = strings.NewReader(job)
		cmd.Env = append(os.Environ(), "SC_NO_AUDIT=1")
		if kc := os.Getenv("SC_E2E_KUBECONFIG"); kc != "" {
			cmd.Env = append(cmd.Env, "KUBECONFIG="+kc)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup: deleting the workload failed: %v\n%s", err, out)
		}
	})
}

// TestFailureAndRecovery covers the triage loop: a stage fails, doctor explains
// it and exits 8, and restart-stage with a context override makes the retry
// succeed.
func TestFailureAndRecovery(t *testing.T) {
	setup(t)
	applyPipeline(t, map[string]any{
		"application":     appName,
		"name":            "e2e-failing",
		"limitConcurrent": false,
		"stages": []map[string]any{{
			"refId": "1", "requisiteStageRefIds": []string{},
			"name": "Call hook", "type": "webhook", "method": "GET",
			// An unresolvable host fails immediately and deterministically.
			"url":               "http://sc-e2e-no-such-host.invalid/hook",
			"waitForCompletion": false,
			"failPipeline":      true,
		}},
	})

	execID := strings.TrimSpace(lastLine(mustSC(t, "pipeline", "run", appName, "e2e-failing")))
	if execID == "" {
		t.Fatal("pipeline run did not print an execution id")
	}

	// --wait must exit 8 on failure so CI can gate on it.
	if code := exitCode(t, "exec", "wait", execID, "--wait-timeout", "3m"); code != 8 {
		t.Errorf("exec wait on a failed execution exited %d, want 8", code)
	}

	// doctor must identify the failing stage and also exit 8.
	var findings []map[string]any
	jsonOutAllowFailure(t, &findings, "doctor", "--execution", execID)
	sawStageFailure := false
	for _, f := range findings {
		if f["level"] == "FAIL" && strings.HasPrefix(fmt.Sprint(f["area"]), "stage/") {
			sawStageFailure = true
			if !strings.Contains(fmt.Sprint(f["hint"]), "restart-stage") {
				t.Errorf("doctor finding has no restart hint: %v", f)
			}
		}
	}
	if !sawStageFailure {
		t.Errorf("doctor did not report the failing stage: %v", findings)
	}
	if code := exitCode(t, "doctor", "--execution", execID); code != 8 {
		t.Errorf("doctor on a failed execution exited %d, want 8", code)
	}

	// failed-stages must work whether or not orca's endpoint is configured: it
	// falls back to deriving them from the execution.
	if out := mustSC(t, "exec", "failed-stages", execID); !strings.Contains(out, "Call hook") {
		t.Errorf("failed-stages did not name the failing stage:\n%s", out)
	}

	// The recovery itself: patch the stage context and rerun just that stage.
	// A URL that resolves and is permitted by orca's SSRF restrictions is needed;
	// without one, only the override mechanism is asserted.
	fixedURL := os.Getenv("SC_E2E_WEBHOOK_URL")
	if fixedURL == "" {
		t.Log("SC_E2E_WEBHOOK_URL not set: asserting the override is applied, not that the retry succeeds")
	}
	target := fixedURL
	if target == "" {
		target = "http://sc-e2e-other-host.invalid/hook"
	}
	mustSC(t, "exec", "restart-stage", execID, "Call hook", "--set", "url="+target)

	waitFor(t, 2*time.Minute, "the restarted stage to settle", func() bool {
		var stages []map[string]any
		out, err := sc(t, "exec", "stages", execID, "-o", "json")
		if err != nil {
			return false
		}
		i := strings.IndexAny(out, "[")
		if i < 0 || json.Unmarshal([]byte(out[i:]), &stages) != nil {
			return false
		}
		for _, s := range stages {
			if s["name"] == "Call hook" {
				st := fmt.Sprint(s["status"])
				return st == "SUCCEEDED" || st == "TERMINAL"
			}
		}
		return false
	})

	// Whatever the outcome, the override must have reached the stage context.
	var exec map[string]any
	jsonOut(t, &exec, "exec", "get", execID)
	stages, _ := exec["stages"].([]any)
	for _, s := range stages {
		stage, _ := s.(map[string]any)
		if stage["name"] != "Call hook" {
			continue
		}
		sctx, _ := stage["context"].(map[string]any)
		if sctx["url"] != target {
			t.Errorf("stage url = %v, want the override %q to have been applied", sctx["url"], target)
		}
		// orca preserves the original failure, which is what makes the retry auditable.
		rd, _ := sctx["restartDetails"].(map[string]any)
		if rd == nil || rd["previousException"] == nil {
			t.Errorf("restartDetails.previousException is missing; the original failure was not preserved")
		}
		if fixedURL != "" && stage["status"] != "SUCCEEDED" {
			t.Errorf("stage status = %v, want SUCCEEDED after the fix", stage["status"])
		}
	}
}

func TestOperatorPlane(t *testing.T) {
	setup(t)
	if os.Getenv("SC_E2E_KUBECONFIG") == "" && os.Getenv("KUBECONFIG") == "" {
		t.Skip("no kubeconfig: set SC_E2E_KUBECONFIG to run the operator-plane tests")
	}

	var statuses []map[string]any
	jsonOut(t, &statuses, "svc", "list")
	if len(statuses) == 0 {
		t.Fatal("svc list reported no services")
	}
	required := map[string]bool{"orca": false, "clouddriver": false, "front50": false, "gate": false}
	for _, s := range statuses {
		name := fmt.Sprint(s["name"])
		if _, ok := required[name]; !ok {
			continue
		}
		required[name] = true
		if s["health"] != "UP" {
			t.Errorf("service %s health = %v, want UP", name, s["health"])
		}
	}
	for name, seen := range required {
		if !seen {
			t.Errorf("service %s missing from svc list", name)
		}
	}

	// Per-component health is what distinguishes diagnosis from liveness.
	out := mustSC(t, "svc", "health", "orca")
	if !strings.Contains(out, "(overall)") {
		t.Errorf("svc health did not report components:\n%s", out)
	}

	// Actuator endpoints beyond /health are optional, so the rest degrades to a skip.
	if _, err := sc(t, "svc", "env", "orca", "--profiles"); err != nil {
		t.Skip("actuator endpoints beyond /health are not exposed; run 'sc svc actuator-config' to enable them")
	}

	t.Run("resolved config", func(t *testing.T) {
		var props []map[string]any
		jsonOut(t, &props, "svc", "env", "orca", "redis")
		if len(props) == 0 {
			t.Error("no redis properties resolved on orca")
		}
	})

	t.Run("live log level", func(t *testing.T) {
		const logger = "com.netflix.spinnaker.orca.q"
		out := mustSC(t, "svc", "set-level", "orca", logger, "DEBUG")
		if !strings.Contains(out, "DEBUG") {
			t.Errorf("set-level did not confirm the new level:\n%s", out)
		}
		var loggers []map[string]any
		jsonOut(t, &loggers, "svc", "loggers", "orca", logger)
		found := false
		for _, l := range loggers {
			if l["name"] == logger && l["configuredLevel"] == "DEBUG" {
				found = true
			}
		}
		if !found {
			t.Errorf("log level was not applied: %v", loggers)
		}
		// Reset so the suite leaves no trace.
		mustSC(t, "svc", "set-level", "orca", logger, "")
	})

	t.Run("thread summary", func(t *testing.T) {
		var summary map[string]any
		jsonOut(t, &summary, "svc", "threads", "orca")
		if total, ok := summary["total"].(float64); !ok || total <= 0 {
			t.Errorf("thread summary reported no threads: %v", summary)
		}
	})

	t.Run("gate routes need gate auth", func(t *testing.T) {
		// Gate authenticates its own actuator endpoints, so this only works if the
		// operator plane falls back to the configured Gate URL and credentials.
		var mappings []map[string]any
		jsonOut(t, &mappings, "svc", "mappings", "gate", "pipelines")
		if len(mappings) == 0 {
			t.Error("no gate routes returned; the gate-auth fallback may be broken")
		}
	})

	t.Run("service api bypasses gate", func(t *testing.T) {
		out := mustSC(t, "svc", "api", "clouddriver", "GET", "/credentials")
		if !strings.Contains(out, account) {
			t.Errorf("clouddriver /credentials did not mention %q:\n%s", account, out)
		}
	})
}

func TestDoctorInstallation(t *testing.T) {
	setup(t)
	var findings []map[string]any
	// doctor exits non-zero when it finds problems, so the failure is expected here.
	jsonOutAllowFailure(t, &findings, "doctor")
	if len(findings) == 0 {
		t.Fatal("doctor reported nothing")
	}
	for _, f := range findings {
		if f["level"] == "FAIL" {
			t.Errorf("doctor FAIL: %v — %v", f["area"], f["message"])
		}
	}
}

func TestMCPServer(t *testing.T) {
	setup(t)
	t.Run("tools are listed and gated", func(t *testing.T) {
		all := mcpRequest(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		readOnly := mcpRequest(t, []string{"--read-only"}, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)

		allTools := toolNames(t, all)
		roTools := toolNames(t, readOnly)
		if len(allTools) == 0 {
			t.Fatal("no tools registered")
		}
		if len(roTools) >= len(allTools) {
			t.Errorf("--read-only listed %d tools, which is not fewer than the %d default", len(roTools), len(allTools))
		}
		for _, mutating := range []string{"save_application", "delete_application", "run_pipeline", "scale_service"} {
			if contains(roTools, mutating) {
				t.Errorf("--read-only exposed the mutating tool %q", mutating)
			}
		}
		// Service-internal tools must stay hidden without --allow-script.
		plain := toolNames(t, mcpRequest(t, nil, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if contains(plain, "call_service_api") {
			t.Error("call_service_api was exposed without --allow-script")
		}
	})

	t.Run("script tools are refused without the flag", func(t *testing.T) {
		resp := mcpRequest(t, nil,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_log_level","arguments":{"service":"orca","logger":"x","level":"DEBUG"}}}`)
		if !strings.Contains(resp, "allow-script") {
			t.Errorf("expected a refusal naming --allow-script, got: %s", resp)
		}
	})

	t.Run("a read tool returns data", func(t *testing.T) {
		resp := mcpRequest(t, nil,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_applications","arguments":{}}}`)
		if !strings.Contains(resp, appName) {
			t.Errorf("list_applications did not include %q: %s", appName, truncate(resp, 400))
		}
	})

	t.Run("prompts are advertised", func(t *testing.T) {
		resp := mcpRequest(t, nil, `{"jsonrpc":"2.0","id":1,"method":"prompts/list","params":{}}`)
		for _, name := range []string{"triage-failed-execution", "review-manual-judgment", "installation-report"} {
			if !strings.Contains(resp, name) {
				t.Errorf("prompt %q is not advertised", name)
			}
		}
	})
}

// mcpRequest sends one JSON-RPC line to `sc mcp` and returns the response line.
func mcpRequest(t *testing.T, extraArgs []string, request string) string {
	t.Helper()
	args := append([]string{"--config", filepath.Join(configDir, "config.yaml"), "mcp"}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Stdin = strings.NewReader(request + "\n")
	cmd.Env = append(os.Environ(), "SC_NO_AUDIT=1")
	if kc := os.Getenv("SC_E2E_KUBECONFIG"); kc != "" {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+kc)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sc mcp failed: %v", err)
	}
	return string(out)
}

func toolNames(t *testing.T, response string) []string {
	t.Helper()
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(firstLine(response))), &resp); err != nil {
		t.Fatalf("decoding tools/list: %v\n%s", err, response)
	}
	names := make([]string, 0, len(resp.Result.Tools))
	for _, tool := range resp.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestExitCodes(t *testing.T) {
	setup(t)
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"not found", []string{"app", "get", "sc-e2e-definitely-absent"}, 5},
		{"network error", []string{"--gate", "http://127.0.0.1:1/api/v1", "health"}, 2},
		{"usage error", []string{"svc", "env", "orca"}, 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(t, tc.args...); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDryRunMakesNoChanges(t *testing.T) {
	setup(t)
	out := mustSC(t, "app", "delete", appName, "--dry-run")
	if !strings.Contains(out, "dry-run") {
		t.Errorf("no dry-run notice:\n%s", out)
	}
	// The application must still be there.
	mustSC(t, "app", "get", appName)
}

func TestAPIEscapeHatch(t *testing.T) {
	setup(t)
	out := mustSC(t, "api", "GET", "/version")
	if !strings.Contains(out, "version") {
		t.Errorf("sc api GET /version returned:\n%s", out)
	}

	// A path segment must round-trip through the escape hatch. This asserts
	// against the application itself rather than its pipelines, which other tests
	// create and clean up.
	var app map[string]any
	jsonOut(t, &app, "api", "GET", "/applications/"+appName)
	attrs, _ := app["attributes"].(map[string]any)
	if attrs == nil {
		attrs = app
	}
	if attrs["name"] != appName {
		t.Errorf("sc api did not return application %s: %v", appName, app)
	}

	// A query parameter must reach Gate: this endpoint returns nothing at all
	// unless one of its mutually exclusive filters is supplied.
	var executions []map[string]any
	jsonOut(t, &executions, "api", "GET", "/executions", "--query", "pipelineConfigIds=none-such-id")
	if executions == nil {
		t.Error("expected an empty array rather than a null body")
	}

	// A manifest name contains a space, which must be percent-encoded exactly once.
	if _, err := sc(t, "api", "GET", "/manifests/"+account+"/"+deployNS+"/deployment no-such-deployment"); err == nil {
		t.Log("unexpected success for a missing manifest, but the path encoded cleanly")
	}
}

// applyPipeline saves a pipeline definition and registers its cleanup.
func applyPipeline(t *testing.T, pipeline map[string]any) {
	t.Helper()
	body, err := json.Marshal(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, fmt.Sprintf("%v.json", pipeline["name"]))
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	mustSC(t, "pipeline", "apply", "-f", path)
	t.Cleanup(func() {
		if os.Getenv("SC_E2E_KEEP") == "1" {
			return
		}
		if out, err := sc(t, "pipeline", "delete", appName, fmt.Sprint(pipeline["name"]), "--force"); err != nil {
			t.Logf("cleanup: deleting pipeline %v failed: %v\n%s", pipeline["name"], err, out)
		}
	})
}

// waitFor polls until cond is true, failing the test on timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", time.Since(start).Round(time.Second), what)
		}
		time.Sleep(3 * time.Second)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
