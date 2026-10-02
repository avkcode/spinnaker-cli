package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/avkcode/spinnaker-cli/pkg/mcp"
	scsvc "github.com/avkcode/spinnaker-cli/pkg/svc"
	"github.com/spf13/cobra"
)

// mcpToolTimeout bounds every tool call so a slow installation cannot hang the
// server indefinitely.
const mcpToolTimeout = 5 * time.Minute

var (
	mcpReadOnly    bool
	mcpAllowScript bool
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run an MCP server exposing sc operations as agent tools (stdio)",
	Long: `Starts a Model Context Protocol server over stdio so an agent can drive Spinnaker
through sc.

Relationship to Gate's own MCP server. Gate 2026.3.0 embeds one (gate-mcp), which
covers the user plane well: applications, pipeline configs and executions, manual
judgments, tasks, Kayenta and Keel. It is off by default (mcp.server.enabled) and,
being inside Gate, it can only see what Gate can see.

This server is the complement, not a duplicate. It works whether or not gate-mcp is
enabled, and its distinctive tools are the operator-plane ones — per-service health
and resolved configuration, live log-level changes, thread-dump triage, Kubernetes
state — which no Gate endpoint can answer.

Safety. Mutating tools can be disabled with --read-only. Tools that reach past the
Gate API into service internals or mutate the cluster are disabled unless
--allow-script is passed.`,
	GroupID: GroupOperator,
	Example: `  sc mcp
  sc mcp --read-only
  sc mcp --allow-script`,
	RunE: func(cmd *cobra.Command, args []string) error {
		s := mcp.NewServer("sc", Version, os.Stdin, os.Stdout)
		s.SetAllowWrite(!mcpReadOnly)
		s.SetAllowScript(mcpAllowScript)
		registerMCPTools(s)
		registerMCPResources(s)
		registerMCPPrompts(s)
		return s.Serve(cmd.Context())
	},
}

// ---------------------------------------------------------------------------
// schema helpers
// ---------------------------------------------------------------------------

func objectSchema(required []string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}
func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func numberProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func objProp(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc}
}
func arrayProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// schemaWithContext adds the common optional "context" argument, so one server
// can address several installations.
func schemaWithContext(required []string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	props["context"] = stringProp("sc context (installation) to target; defaults to the current one")
	return objectSchema(required, props)
}

// mutatingSchema adds "context" and "dryRun".
func mutatingSchema(required []string, props map[string]any) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	props["dryRun"] = boolProp("report what would happen without doing it")
	return schemaWithContext(required, props)
}

// mcpGate connects to Gate for a tool call, honouring a per-call context override.
func mcpGate(parent context.Context, args map[string]any) (*gate.Client, context.Context, context.CancelFunc, error) {
	ctxName := mcp.StringArg(args, "context")
	if ctxName == "" {
		ctxName = contextOverride
	}
	ctx, cancel := context.WithTimeout(parent, mcpToolTimeout)
	client, err := getGateWithContext(ctxName)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return client, ctx, cancel, nil
}

// mcpSvc connects to the operator plane for a tool call.
func mcpSvc(parent context.Context, args map[string]any) (*scsvc.Client, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(parent, mcpToolTimeout)
	client, err := getSvc()
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return client, ctx, cancel, nil
}

func intArg(args map[string]any, name string) (int, bool) {
	switch v := args[name].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n, true
		}
	}
	return 0, false
}

func stringsArg(args map[string]any, name string) []string {
	switch v := args[name].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	}
	return nil
}

func mapArg(args map[string]any, name string) map[string]any {
	if m, ok := args[name].(map[string]any); ok {
		return m
	}
	return nil
}

func dryRunResult(action string) (any, error) {
	return map[string]any{"dryRun": true, "wouldDo": action}, nil
}

func requireString(args map[string]any, name string) (string, error) {
	v := mcp.StringArg(args, name)
	if v == "" {
		return "", mcp.Errorf("invalid_args", "%s is required", name)
	}
	return v, nil
}

func registerMCPTools(s *mcp.Server) {
	registerCoreTools(s)
	registerExecutionTools(s)
	registerOperatorTools(s)
}

// ---------------------------------------------------------------------------
// Core (Gate) tools
// ---------------------------------------------------------------------------

func registerCoreTools(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:        "list_contexts",
		Description: "List the Spinnaker installations sc is configured for, and which is current.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: objectSchema(nil, nil),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			contexts := viperStringMap("contexts")
			out := []map[string]any{}
			for name, raw := range contexts {
				m, _ := raw.(map[string]any)
				out = append(out, map[string]any{
					"name":    name,
					"gate":    stringField(m, "gate"),
					"current": name == viperString("current-context"),
				})
			}
			return out, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "spinnaker_version",
		Description: "Report the installation's Spinnaker version and, where the cluster is reachable, each service's image version.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, nil),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.Version(ctx)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_applications",
		Description: "List applications. Entries with an email/createTs are registered in front50; the rest are inferred by clouddriver from cached infrastructure and cannot own pipelines.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, map[string]any{
			"account": stringProp("only applications deployed in this account"),
			"owner":   stringProp("only applications with this owner email"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ListApplications(ctx, gate.ListApplicationsOptions{
				Account: mcp.StringArg(args, "account"),
				Owner:   mcp.StringArg(args, "owner"),
			})
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_application",
		Description: "Get one application's details, optionally including the clusters clouddriver has cached.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"application"}, map[string]any{
			"application": stringProp("application name"),
			"expand":      boolProp("include cached clusters (slower)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.GetApplication(ctx, app, mcp.BoolArg(args, "expand"))
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "save_application",
		Description: "Create or update an application. Submitted to orca as an upsertApplication task; returns the task id.",
		InputSchema: mutatingSchema([]string{"application", "email"}, map[string]any{
			"application":    stringProp("application name"),
			"email":          stringProp("owner email (front50 requires one)"),
			"description":    stringProp("description"),
			"cloudProviders": stringProp("comma-separated cloud providers, e.g. kubernetes"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			email, err := requireString(args, "email")
			if err != nil {
				return nil, err
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("create/update application " + name)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			taskID, err := client.SaveApplication(ctx, gate.Application{
				Name:           name,
				Email:          email,
				Description:    mcp.StringArg(args, "description"),
				CloudProviders: mcp.StringArg(args, "cloudProviders"),
			}, mcp.StringArg(args, "email"))
			if err != nil {
				return nil, err
			}
			audit("mcp.save_application", name)
			return map[string]any{"application": name, "task": taskID}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "delete_application",
		Description: "Delete an application's front50 metadata and pipeline definitions. Deployed infrastructure is left running.",
		Destructive: true,
		InputSchema: mutatingSchema([]string{"application"}, map[string]any{
			"application": stringProp("application name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("delete application " + name)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			taskID, err := client.DeleteApplication(ctx, name, "")
			if err != nil {
				return nil, err
			}
			audit("mcp.delete_application", name)
			return map[string]any{"deleted": name, "task": taskID}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_pipeline_configs",
		Description: "List an application's pipeline definitions.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"application"}, map[string]any{
			"application": stringProp("application name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ListPipelineConfigs(ctx, app)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_pipeline_config",
		Description: "Get one pipeline definition as JSON.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"application", "pipeline"}, map[string]any{
			"application": stringProp("application name"),
			"pipeline":    stringProp("pipeline name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			name, err := requireString(args, "pipeline")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.GetPipelineConfig(ctx, app, name)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "save_pipeline_config",
		Description: "Create or update a pipeline definition from a full JSON document. An 'id' makes it an update.",
		InputSchema: mutatingSchema([]string{"pipeline"}, map[string]any{
			"pipeline": objProp("the complete pipeline definition, including application and name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			pipeline := mapArg(args, "pipeline")
			if len(pipeline) == 0 {
				return nil, mcp.Errorf("invalid_args", "pipeline is required")
			}
			app, _ := pipeline["application"].(string)
			name, _ := pipeline["name"].(string)
			if app == "" || name == "" {
				return nil, mcp.Errorf("invalid_args", "pipeline must have 'application' and 'name'")
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("save pipeline " + app + "/" + name)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			if err := client.SavePipelineConfig(ctx, pipeline); err != nil {
				return nil, err
			}
			audit("mcp.save_pipeline_config", app+"/"+name)
			return map[string]any{"saved": app + "/" + name}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "delete_pipeline_config",
		Description: "Delete a pipeline definition by name.",
		Destructive: true,
		InputSchema: mutatingSchema([]string{"application", "pipeline"}, map[string]any{
			"application": stringProp("application name"),
			"pipeline":    stringProp("pipeline name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			name, err := requireString(args, "pipeline")
			if err != nil {
				return nil, err
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("delete pipeline " + app + "/" + name)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			if err := client.DeletePipelineConfig(ctx, app, name); err != nil {
				return nil, err
			}
			audit("mcp.delete_pipeline_config", app+"/"+name)
			return map[string]any{"deleted": app + "/" + name}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_accounts",
		Description: "List the configured cloud accounts and whether the caller is authorized for each.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, map[string]any{
			"expand": boolProp("include each account's full detail"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ListAccounts(ctx, mcp.BoolArg(args, "expand"))
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_manifest",
		Description: "Get a Kubernetes manifest from clouddriver's cache with the status Spinnaker computed for it — exactly what a 'wait for manifest to stabilize' stage is waiting on. Name it as Spinnaker does: 'kind name', e.g. 'deployment nginx'.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"account", "location", "name"}, map[string]any{
			"account":  stringProp("Kubernetes account"),
			"location": stringProp("namespace"),
			"name":     stringProp("'kind name', e.g. 'deployment nginx'"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			account, err := requireString(args, "account")
			if err != nil {
				return nil, err
			}
			location, err := requireString(args, "location")
			if err != nil {
				return nil, err
			}
			name, err := requireString(args, "name")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.GetManifest(ctx, account, location, name)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "search_infrastructure",
		Description: "Search clouddriver's cached infrastructure index. Gate accepts one type per call, so this fans out over the requested types and merges the results.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"query"}, map[string]any{
			"query":    stringProp("search query"),
			"types":    arrayProp("resource types: applications, clusters, serverGroups, instances, loadBalancers, securityGroups, projects"),
			"pageSize": numberProp("results per type (default 25)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			query, err := requireString(args, "query")
			if err != nil {
				return nil, err
			}
			types := stringsArg(args, "types")
			if len(types) == 0 {
				types = []string{"applications", "serverGroups", "clusters", "loadBalancers", "instances"}
			}
			pageSize, ok := intArg(args, "pageSize")
			if !ok {
				pageSize = 25
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			out := map[string]any{}
			for _, ty := range types {
				results, err := client.Search(ctx, gate.SearchOptions{Query: query, Type: ty, PageSize: pageSize})
				if err != nil {
					// A provider that is not configured 404s its types; that is not
					// a reason to fail the whole search.
					continue
				}
				out[ty] = results
			}
			return out, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "call_gate_api",
		Description: "Call any Gate endpoint. Use this for anything the typed tools do not cover; 'sc svc mappings gate' (or the inspect_service_routes tool) lists what the installation actually serves. Paths are relative to the configured endpoint and must not repeat Gate's context path.",
		InputSchema: mutatingSchema([]string{"path"}, map[string]any{
			"method": stringProp("HTTP method (default GET)"),
			"path":   stringProp("path relative to the Gate endpoint, e.g. /applications"),
			"query":  objProp("query parameters"),
			"body":   objProp("JSON request body"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			path, err := requireString(args, "path")
			if err != nil {
				return nil, err
			}
			method := strings.ToUpper(mcp.StringArg(args, "method"))
			if method == "" {
				method = "GET"
			}
			if method != "GET" && mcp.BoolArg(args, "dryRun") {
				return dryRunResult(method + " " + path)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			q := toQuery(mapArg(args, "query"))
			var body any
			if b := mapArg(args, "body"); len(b) > 0 {
				body = b
			}
			status, raw, err := client.Raw(ctx, gate.Request{Method: method, Path: path, Query: q, Body: body})
			if err != nil {
				return nil, err
			}
			if method != "GET" {
				audit("mcp.call_gate_api", method+" "+path)
			}
			return map[string]any{"status": status, "body": decodeLoosely(raw)}, nil
		},
	})
}

func init() {
	mcpCmd.Flags().BoolVar(&mcpReadOnly, "read-only", false, "disable every mutating tool")
	mcpCmd.Flags().BoolVar(&mcpAllowScript, "allow-script", false, "enable tools that bypass Gate to reach service internals or mutate the cluster")
	rootCmd.AddCommand(mcpCmd)
}
