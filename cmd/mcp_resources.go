package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/avkcode/spinnaker-cli/pkg/k8s"
	"github.com/avkcode/spinnaker-cli/pkg/mcp"
	scsvc "github.com/avkcode/spinnaker-cli/pkg/svc"
)

// resourceScheme is the URI scheme for sc's MCP resources.
const resourceScheme = "spinnaker://"

// k8sLogOpts builds pod log options for the MCP tools.
func k8sLogOpts(tail int, previous bool) k8s.LogOptions {
	return k8s.LogOptions{TailLines: tail, Previous: previous}
}

// readLines reads a log stream into lines, optionally keeping only those
// containing needle. Output is capped so one tool call cannot return a megabyte
// of logs.
func readLines(r io.Reader, needle string) ([]string, error) {
	const maxLines = 2000
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lines := []string{}
	for scanner.Scan() {
		line := scanner.Text()
		if needle != "" && !strings.Contains(line, needle) {
			continue
		}
		lines = append(lines, line)
		if len(lines) >= maxLines {
			lines = append(lines, fmt.Sprintf("… truncated at %d lines", maxLines))
			break
		}
	}
	return lines, scanner.Err()
}

func registerMCPResources(s *mcp.Server) {
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://applications/{application}",
		Name:        "application", MimeType: "application/json",
		Description: "An application's details",
	})
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://applications/{application}/pipelines",
		Name:        "application pipeline definitions", MimeType: "application/json",
		Description: "An application's pipeline definitions",
	})
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://applications/{application}/executions",
		Name:        "application executions", MimeType: "application/json",
		Description: "An application's recent pipeline executions",
	})
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://executions/{executionId}",
		Name:        "execution", MimeType: "application/json",
		Description: "One execution in full",
	})
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://services/{service}/health",
		Name:        "service health", MimeType: "application/json",
		Description: "One service's actuator health, with per-component detail",
	})
	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: "spinnaker://services/{service}/config",
		Name:        "service config", MimeType: "text/plain",
		Description: "The configuration files mounted into one service",
	})

	s.SetResourceLister(func(ctx context.Context) ([]mcp.Resource, error) {
		res := []mcp.Resource{
			{URI: resourceScheme + "services", Name: "services",
				MimeType: "application/json", Description: "Every service's replicas, version and health"},
			{URI: resourceScheme + "diagnosis", Name: "diagnosis",
				MimeType: "application/json", Description: "Full installation diagnosis (the doctor report)"},
		}
		// Listing applications is best-effort: an unconfigured or unreachable
		// installation should still advertise the static resources.
		client, err := getGate()
		if err != nil {
			return res, nil
		}
		apps, err := client.ListApplications(ctx, gate.ListApplicationsOptions{})
		if err != nil {
			return res, nil
		}
		for _, a := range apps {
			// Only registered applications can own pipelines, so inferred ones
			// would be dead resource links.
			if str(a, "email") == "" && str(a, "createTs") == "" {
				continue
			}
			name := str(a, "name")
			res = append(res, mcp.Resource{
				URI:         resourceScheme + "applications/" + name,
				Name:        name,
				MimeType:    "application/json",
				Description: "Application " + name,
			})
		}
		return res, nil
	})

	s.SetResourceReader(func(ctx context.Context, uri string) (mcp.ResourceContent, error) {
		return readSpinnakerResource(ctx, uri)
	})
}

func readSpinnakerResource(ctx context.Context, uri string) (mcp.ResourceContent, error) {
	if !strings.HasPrefix(uri, resourceScheme) {
		return mcp.ResourceContent{}, fmt.Errorf("unsupported resource uri: %s", uri)
	}
	path := strings.TrimPrefix(uri, resourceScheme)
	jsonContent := func(v any) (mcp.ResourceContent, error) {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		return mcp.ResourceContent{URI: uri, MimeType: "application/json", Text: string(raw)}, nil
	}

	switch {
	case path == "services":
		client, err := getSvc()
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		statuses, err := client.List(ctx)
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		return jsonContent(statuses)

	case path == "diagnosis":
		return jsonContent(diagnoseInstallation(ctx))

	case strings.HasPrefix(path, "services/") && strings.HasSuffix(path, "/health"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "services/"), "/health")
		client, err := getSvc()
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		health, err := client.Health(ctx, name)
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		return jsonContent(health)

	case strings.HasPrefix(path, "services/") && strings.HasSuffix(path, "/config"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "services/"), "/config")
		client, err := getSvc()
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		files, err := client.LiveConfig(ctx, name)
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		var sb strings.Builder
		for _, key := range sortedKeys(files) {
			fmt.Fprintf(&sb, "# ---- %s ----\n%s\n", key, strings.TrimRight(files[key], "\n"))
		}
		return mcp.ResourceContent{URI: uri, MimeType: "text/plain", Text: sb.String()}, nil

	case strings.HasPrefix(path, "executions/"):
		id := strings.TrimPrefix(path, "executions/")
		client, err := getGate()
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		exec, err := client.GetExecution(ctx, id)
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		return jsonContent(exec)

	case strings.HasPrefix(path, "applications/"):
		rest := strings.TrimPrefix(path, "applications/")
		client, err := getGate()
		if err != nil {
			return mcp.ResourceContent{}, err
		}
		switch {
		case strings.HasSuffix(rest, "/pipelines"):
			app := strings.TrimSuffix(rest, "/pipelines")
			pipelines, err := client.ListPipelineConfigs(ctx, app)
			if err != nil {
				return mcp.ResourceContent{}, err
			}
			return jsonContent(pipelines)
		case strings.HasSuffix(rest, "/executions"):
			app := strings.TrimSuffix(rest, "/executions")
			execs, err := client.ListExecutions(ctx, app, gate.ListExecutionsOptions{Limit: 20})
			if err != nil {
				return mcp.ResourceContent{}, err
			}
			return jsonContent(execs)
		default:
			app, err := client.GetApplication(ctx, rest, false)
			if err != nil {
				return mcp.ResourceContent{}, err
			}
			return jsonContent(app)
		}
	}
	return mcp.ResourceContent{}, fmt.Errorf("unknown resource: %s", uri)
}

func registerMCPPrompts(s *mcp.Server) {
	s.AddPrompt(mcp.Prompt{
		Name:        "triage-failed-execution",
		Description: "Gather a failed execution's stages, failure messages and the health of the services involved, then ask for a root cause and a concrete next action.",
		Arguments: []mcp.PromptArg{
			{Name: "executionId", Description: "execution id", Required: true},
		},
		Handler: func(ctx context.Context, args map[string]string) (mcp.PromptResult, error) {
			id := args["executionId"]
			if id == "" {
				return mcp.PromptResult{}, fmt.Errorf("executionId is required")
			}
			guidance := "Determine the root cause and recommend one concrete next action. " +
				"Useful follow-ups: get_execution_stages and get_failed_stages for detail; evaluate_expression to see what a " +
				"stage actually resolved; service_health and service_logs for the service that failed; restart_stage once the " +
				"cause is fixed (it reruns only that stage and what follows, keeping the execution's history)."

			var sb strings.Builder
			findings, err := diagnoseExecution(ctx, id)
			if err != nil {
				sb.WriteString(fmt.Sprintf("Execution %s could not be fetched: %v\n\n", id, err))
			} else {
				sb.WriteString(fmt.Sprintf("Diagnosis of execution %s:\n\n", id))
				for _, f := range findings {
					sb.WriteString(fmt.Sprintf("- [%s] %s: %s\n", f.Level, f.Area, f.Message))
					if f.Hint != "" {
						sb.WriteString(fmt.Sprintf("    suggested: %s\n", f.Hint))
					}
				}
				sb.WriteString("\n")
			}
			sb.WriteString(guidance)
			return mcp.PromptResult{
				Description: "Triage a failed Spinnaker execution",
				Messages:    []mcp.PromptMessage{{Role: "user", Text: sb.String()}},
			}, nil
		},
	})

	s.AddPrompt(mcp.Prompt{
		Name:        "review-manual-judgment",
		Description: "Gather the context behind a pending manual judgment — what the pipeline has done so far and what it will do next — before approving or rejecting it.",
		Arguments: []mcp.PromptArg{
			{Name: "executionId", Description: "execution id", Required: true},
		},
		Handler: func(ctx context.Context, args map[string]string) (mcp.PromptResult, error) {
			id := args["executionId"]
			if id == "" {
				return mcp.PromptResult{}, fmt.Errorf("executionId is required")
			}
			client, err := getGate()
			if err != nil {
				return mcp.PromptResult{}, err
			}
			exec, err := client.GetExecution(ctx, id)
			if err != nil {
				return mcp.PromptResult{}, err
			}

			var sb strings.Builder
			fmt.Fprintf(&sb, "Execution %s of %s/%s is %s and is waiting on a manual judgment.\n\n",
				id, str(exec, "application"), str(exec, "name"), str(exec, "status"))
			sb.WriteString("Stages so far:\n")
			for _, r := range stageRows(exec, false) {
				fmt.Fprintf(&sb, "- %-10s %-30s %s", r.Status, strings.TrimSpace(r.Name), r.Duration)
				if r.Message != "" {
					fmt.Fprintf(&sb, " — %s", r.Message)
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\nAssess whether this judgment should be approved. Weigh what has already succeeded, " +
				"anything that failed or was skipped, and what the remaining stages will do. " +
				"Recommend judge_stage with continue or stop, and say why.")
			return mcp.PromptResult{
				Description: "Review a pending manual judgment",
				Messages:    []mcp.PromptMessage{{Role: "user", Text: sb.String()}},
			}, nil
		},
	})

	s.AddPrompt(mcp.Prompt{
		Name:        "installation-report",
		Description: "Gather the installation's service inventory and diagnosis, then ask for an assessment of its health and what to fix first.",
		Handler: func(ctx context.Context, args map[string]string) (mcp.PromptResult, error) {
			var sb strings.Builder
			sb.WriteString("Spinnaker installation diagnosis:\n\n")
			for _, f := range diagnoseInstallation(ctx) {
				fmt.Fprintf(&sb, "- [%s] %s: %s\n", f.Level, f.Area, f.Message)
				if f.Hint != "" {
					fmt.Fprintf(&sb, "    suggested: %s\n", f.Hint)
				}
			}
			fmt.Fprintf(&sb, "\nServices in the catalog: %s.\n", strings.Join(scsvc.Names(), ", "))
			sb.WriteString("\nAssess this installation's health and say what to fix first and why. " +
				"service_health, service_config, service_logs and service_thread_summary can be used to dig further.")
			return mcp.PromptResult{
				Description: "Assess a Spinnaker installation",
				Messages:    []mcp.PromptMessage{{Role: "user", Text: sb.String()}},
			}, nil
		},
	})
}
