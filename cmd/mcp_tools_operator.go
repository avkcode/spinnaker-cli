package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/avkcode/spinnaker-cli/pkg/mcp"
	scsvc "github.com/avkcode/spinnaker-cli/pkg/svc"
)

// registerOperatorTools registers the operator-plane tools.
//
// These are what this server adds over Gate's own embedded MCP server: Gate
// cannot report its own services' resolved configuration, change a log level on a
// running JVM, or read a thread dump, because none of that is behind a Gate
// endpoint. Tools that reach past Gate into service internals, or that mutate the
// cluster, are marked Script so they stay off until --allow-script.
func registerOperatorTools(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:        "list_services",
		Description: "List every Spinnaker service with its replica count, image version and actuator health. Distinguishes not-deployed from deployed-but-unhealthy from deliberately-disabled — the first thing to check on a misbehaving installation.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, nil),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.List(ctx)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "service_health",
		Description: "Get one or more services' actuator health with per-component detail (redis, SQL, downstream services). This is the diagnosis behind a failing deployment that Gate's own /health cannot give.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, map[string]any{
			"services": arrayProp("service names; defaults to every JVM service"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			targets := stringsArg(args, "services")
			if len(targets) == 0 {
				targets = scsvc.JVMNames()
			}
			out := map[string]any{}
			var mu sync.Mutex
			var wg sync.WaitGroup
			for _, name := range targets {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					health, err := client.Health(ctx, name)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						out[name] = map[string]any{"error": err.Error()}
						return
					}
					out[name] = health
				}(name)
			}
			wg.Wait()
			return out, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "service_config",
		Description: "Show the configuration a service actually resolved, in precedence order with shadowed values marked. Answers not 'what did the config file say' but 'what value is this service using, and which source won' — normally the whole explanation when a config change appears to have had no effect. A filter is required unless 'all' is set. Values can include credentials.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service": stringProp("service name, e.g. orca, clouddriver"),
			"filter":  stringProp("filter property names by substring, e.g. 'redis', 'kubernetes'"),
			"all":     boolProp("return every property (large)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			filter := mcp.StringArg(args, "filter")
			if filter == "" && !mcp.BoolArg(args, "all") {
				return nil, mcp.Errorf("invalid_args", "a filter is required (a service resolves thousands of properties); pass 'filter' or set 'all'")
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			props, err := client.Env(ctx, service, filter)
			if err != nil {
				return nil, err
			}
			profiles, _ := client.ActiveProfiles(ctx, service)
			return map[string]any{"service": service, "activeProfiles": profiles, "properties": props}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_log_levels",
		Description: "List a service's log categories with their configured and effective levels. Set 'configured' to see only categories someone has explicitly changed.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service":    stringProp("service name"),
			"filter":     stringProp("filter category names by substring (defaults to com.netflix.spinnaker)"),
			"configured": boolProp("only categories with an explicitly configured level"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			filter := mcp.StringArg(args, "filter")
			configured := mcp.BoolArg(args, "configured")
			if filter == "" && !configured {
				filter = "com.netflix.spinnaker"
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.Loggers(ctx, service, filter, configured)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "set_log_level",
		Description: "Change a log category's level on a running service — no restart, no redeploy, reverting on the next restart. This turns an investigation that would need a redeploy into one call; follow it with service_logs to read the result. Pass an empty level to reset the category to inherited.",
		Script:      true,
		InputSchema: mutatingSchema([]string{"service", "logger"}, map[string]any{
			"service": stringProp("service name"),
			"logger":  stringProp("log category, e.g. com.netflix.spinnaker.clouddriver.kubernetes"),
			"level":   map[string]any{"type": "string", "enum": append([]string{""}, scsvc.LogLevels...), "description": "level to set; empty resets to inherited"},
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			logger, err := requireString(args, "logger")
			if err != nil {
				return nil, err
			}
			level := mcp.StringArg(args, "level")
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult(fmt.Sprintf("set %s logger %s to %q", service, logger, level))
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			if err := client.SetLogLevel(ctx, service, logger, level); err != nil {
				return nil, err
			}
			audit("mcp.set_log_level", fmt.Sprintf("%s %s=%s", service, logger, level))
			result, err := client.GetLogger(ctx, service, logger)
			if err != nil {
				return map[string]any{"service": service, "logger": logger, "configuredLevel": level}, nil
			}
			return result, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "service_metrics",
		Description: "List a service's metric names, or read one metric's measurements. Useful: queue.* on orca (depth and lag), cats.*/executionTime on clouddriver (cache agents), http.server.requests anywhere, jvm.* everywhere.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service": stringProp("service name"),
			"metric":  stringProp("exact metric name to read; omit to list names"),
			"filter":  stringProp("filter metric names by substring when listing"),
			"tags":    arrayProp("filter a metric by tag, as KEY:VALUE"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			if metric := mcp.StringArg(args, "metric"); metric != "" {
				return client.Metric(ctx, service, metric, stringsArg(args, "tags"))
			}
			return client.MetricNames(ctx, service, mcp.StringArg(args, "filter"))
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "service_thread_summary",
		Description: "Summarise a service's thread dump: counts by state plus every blocked thread with the lock it waits on and its top frames. This identifies a wedged service — an orca that stopped draining its queue, a clouddriver stuck in a provider call — without reading a multi-megabyte dump.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service": stringProp("service name"),
			"frames":  numberProp("stack frames per blocked thread (default 5)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			frames, ok := intArg(args, "frames")
			if !ok {
				frames = 5
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ThreadSummary(ctx, service, frames)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "inspect_service_routes",
		Description: "List the HTTP routes a running service actually serves. The authoritative endpoint inventory for this build and configuration, including plugin-contributed routes — use it to discover what call_gate_api or call_service_api can reach.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service": stringProp("service name"),
			"filter":  stringProp("filter routes by path or handler substring"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.Mappings(ctx, service, mcp.StringArg(args, "filter"))
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "call_service_api",
		Description: "Call an arbitrary endpoint on an individual service, bypassing Gate. Reaches internal endpoints that have no Gate route at all — clouddriver cache introspection and on-demand refresh, orca's raw execution documents, front50's unfiltered collections. Two caveats: these are internal and not a stable contract, and they mostly enforce no Fiat authorization, so this bypasses the permission checks Gate would apply.",
		Script:      true,
		InputSchema: mutatingSchema([]string{"service", "path"}, map[string]any{
			"service": stringProp("service name"),
			"method":  stringProp("HTTP method (default GET)"),
			"path":    stringProp("path on the service, e.g. /cache/introspection"),
			"query":   objProp("query parameters"),
			"body":    objProp("JSON request body"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			service, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			path, err := requireString(args, "path")
			if err != nil {
				return nil, err
			}
			method := strings.ToUpper(mcp.StringArg(args, "method"))
			if method == "" {
				method = "GET"
			}
			if method != "GET" && mcp.BoolArg(args, "dryRun") {
				return dryRunResult(method + " " + path + " on " + service)
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			var body []byte
			if b := mapArg(args, "body"); len(b) > 0 {
				body = []byte(jsonCompact(b))
			}
			raw, err := client.Do(ctx, scsvc.Request{
				Service: service, Method: method, Path: path,
				Query: toQuery(mapArg(args, "query")), Body: body,
			})
			if err != nil {
				return nil, err
			}
			if method != "GET" {
				audit("mcp.call_service_api", service+" "+method+" "+path)
			}
			return decodeLoosely(raw), nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "service_logs",
		Description: "Read a service's recent container logs from Kubernetes. Pair with set_log_level: turn a category up, reproduce, read it here.",
		ReadOnly:    true,
		Script:      true,
		InputSchema: schemaWithContext([]string{"service"}, map[string]any{
			"service":  stringProp("service name"),
			"tail":     numberProp("lines of history (default 200)"),
			"previous": boolProp("read the previous container instance, i.e. the one that crashed"),
			"grep":     stringProp("only lines containing this substring"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			service, err := scsvc.Lookup(name)
			if err != nil {
				return nil, mcp.Errorf("invalid_args", "%s", err.Error())
			}
			tail, ok := intArg(args, "tail")
			if !ok {
				tail = 200
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			kube, err := client.Kube()
			if err != nil {
				return nil, err
			}
			pods, err := kube.ListPods(ctx, client.Namespace(), "app.kubernetes.io/name="+service.Name)
			if err != nil {
				return nil, err
			}
			if len(pods) == 0 {
				return nil, mcp.Errorf("not_found", "no pods for service %s in namespace %s", service.Name, client.Namespace())
			}
			stream, err := kube.PodLogs(ctx, client.Namespace(), pods[0].Name, k8sLogOpts(tail, mcp.BoolArg(args, "previous")))
			if err != nil {
				return nil, err
			}
			defer stream.Close()
			lines, err := readLines(stream, mcp.StringArg(args, "grep"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"service": service.Name, "pod": pods[0].Name, "lines": lines}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "scale_service",
		Description: "Scale a service's Deployment. Scaling to 0 is the supported way to stop a service without removing it — how an optional service is disabled, and how clouddriver is quiesced before a cache rebuild.",
		Destructive: true,
		Script:      true,
		InputSchema: mutatingSchema([]string{"service", "replicas"}, map[string]any{
			"service":  stringProp("service name"),
			"replicas": numberProp("desired replica count"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			service, err := scsvc.Lookup(name)
			if err != nil {
				return nil, mcp.Errorf("invalid_args", "%s", err.Error())
			}
			replicas, ok := intArg(args, "replicas")
			if !ok || replicas < 0 {
				return nil, mcp.Errorf("invalid_args", "replicas must be a non-negative integer")
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult(fmt.Sprintf("scale %s to %d replicas", service.Name, replicas))
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			kube, err := client.Kube()
			if err != nil {
				return nil, err
			}
			if err := kube.ScaleDeployment(ctx, client.Namespace(), service.Name, replicas); err != nil {
				return nil, err
			}
			audit("mcp.scale_service", fmt.Sprintf("%s=%d", service.Name, replicas))
			return map[string]any{"service": service.Name, "replicas": replicas}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "restart_service",
		Description: "Roll a service's pods, as 'kubectl rollout restart' does. Needed after a ConfigMap change, since Spinnaker services read configuration only at startup.",
		Destructive: true,
		Script:      true,
		InputSchema: mutatingSchema([]string{"service"}, map[string]any{
			"service": stringProp("service name"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			name, err := requireString(args, "service")
			if err != nil {
				return nil, err
			}
			service, err := scsvc.Lookup(name)
			if err != nil {
				return nil, mcp.Errorf("invalid_args", "%s", err.Error())
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("roll " + service.Name + " pods")
			}
			client, ctx, cancel, err := mcpSvc(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			kube, err := client.Kube()
			if err != nil {
				return nil, err
			}
			if err := kube.RestartDeployment(ctx, client.Namespace(), service.Name); err != nil {
				return nil, err
			}
			audit("mcp.restart_service", service.Name)
			return map[string]any{"restarted": service.Name}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "diagnose",
		Description: "Run the full diagnostic battery and return findings at OK/WARN/FAIL. With no executionId it checks the installation (Gate, identity, accounts, every service's health, whether the operator plane is exposed); with one it explains why that execution failed and what to do next.",
		ReadOnly:    true,
		InputSchema: schemaWithContext(nil, map[string]any{
			"executionId": stringProp("diagnose this execution instead of the installation"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			ctx, cancel := context.WithTimeout(ctx, mcpToolTimeout)
			defer cancel()
			if id := mcp.StringArg(args, "executionId"); id != "" {
				return diagnoseExecution(ctx, id)
			}
			return diagnoseInstallation(ctx), nil
		},
	})
}
