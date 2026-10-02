package cmd

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/avkcode/spinnaker-cli/pkg/mcp"
	"github.com/spf13/viper"
)

// registerExecutionTools registers the execution/task/judgment tools — the agent
// loop for "a deployment is failing, work out why and act".
func registerExecutionTools(s *mcp.Server) {
	s.AddTool(mcp.Tool{
		Name:        "run_pipeline",
		Description: "Trigger a pipeline and return the new execution id. Does not wait; poll with get_execution or call wait_for_execution.",
		InputSchema: mutatingSchema([]string{"application", "pipeline"}, map[string]any{
			"application": stringProp("application name"),
			"pipeline":    stringProp("pipeline name or config id"),
			"parameters":  objProp("pipeline parameters, readable downstream as ${parameters.name}"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			pipeline, err := requireString(args, "pipeline")
			if err != nil {
				return nil, err
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("trigger pipeline " + app + "/" + pipeline)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			execID, err := client.RunPipeline(ctx, app, pipeline, gate.RunPipelineOptions{
				Parameters: mapArg(args, "parameters"),
			})
			if err != nil {
				return nil, err
			}
			audit("mcp.run_pipeline", app+"/"+pipeline+" -> "+execID)
			return map[string]any{"application": app, "pipeline": pipeline, "executionId": execID}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_executions",
		Description: "List an application's executions, newest first. Gate returns 10 per pipeline unless limit is raised.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"application"}, map[string]any{
			"application": stringProp("application name"),
			"limit":       numberProp("executions per pipeline (default 20)"),
			"statuses":    arrayProp("filter by status: RUNNING, PAUSED, SUCCEEDED, TERMINAL, CANCELED, BUFFERED"),
			"pipeline":    stringProp("only pipelines whose name contains this"),
			"expand":      boolProp("include full stage detail (much larger)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			limit, ok := intArg(args, "limit")
			if !ok {
				limit = 20
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ListExecutions(ctx, app, gate.ListExecutionsOptions{
				Limit:              limit,
				Statuses:           stringsArg(args, "statuses"),
				PipelineNameFilter: mcp.StringArg(args, "pipeline"),
				Expand:             mcp.BoolArg(args, "expand"),
			})
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_execution",
		Description: "Get one execution in full, including every stage's resolved context.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"executionId"}, map[string]any{
			"executionId": stringProp("execution id"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.GetExecution(ctx, id)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_execution_stages",
		Description: "Summarise an execution's stages: status, duration and failure message each, in order. Far smaller than get_execution and usually enough to locate a failure.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"executionId"}, map[string]any{
			"executionId":      stringProp("execution id"),
			"includeSynthetic": boolProp("include orca's synthetic bookkeeping stages"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			exec, err := client.GetExecution(ctx, id)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"executionId": id,
				"application": str(exec, "application"),
				"pipeline":    str(exec, "name"),
				"status":      str(exec, "status"),
				"stages":      stageRows(exec, mcp.BoolArg(args, "includeSynthetic")),
			}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_failed_stages",
		Description: "Return only an execution's failed stages, descending into nested pipeline executions. The fast path for triage.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"executionId"}, map[string]any{
			"executionId": stringProp("execution id"),
			"limit":       numberProp("maximum nested executions to descend into (default 5)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			limit, ok := intArg(args, "limit")
			if !ok {
				limit = 5
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			failed, err := client.GetFailedStages(ctx, id, limit)
			if err == nil {
				return map[string]any{"executionId": id, "failedStages": failed}, nil
			}
			// orca's failedStages endpoint NPEs when tasks.controller.failedStages is
			// unset, which is the default. Fall back to deriving them from the
			// execution; only the descent into nested pipelines is lost.
			if !isFailedStagesConfigBug(err) {
				return nil, err
			}
			exec, gerr := client.GetExecution(ctx, id)
			if gerr != nil {
				return nil, err
			}
			rows := []stageRow{}
			for _, r := range stageRows(exec, true) {
				if gate.IsFailureStatus(r.Status) {
					rows = append(rows, r)
				}
			}
			return map[string]any{
				"executionId":  id,
				"failedStages": rows,
				"note": "orca's failedStages endpoint is unconfigured on this installation " +
					"(tasks.controller.failedStages), so these were derived from the execution; " +
					"nested pipeline executions were not followed",
			}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "wait_for_execution",
		Description: "Block until an execution reaches a terminal status, then report the outcome and which stage failed. Bounded by timeoutSeconds (default 600).",
		ReadOnly:    true,
		InputSchema: schemaWithContext([]string{"executionId"}, map[string]any{
			"executionId":    stringProp("execution id"),
			"timeoutSeconds": numberProp("give up after this long (default 600, max 1800)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			timeout, ok := intArg(args, "timeoutSeconds")
			if !ok {
				timeout = 600
			}
			// The tool-call budget caps this; a longer wait should be several calls.
			if timeout > 1800 {
				timeout = 1800
			}
			client, _, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			cancel()
			waitCtx, waitCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer waitCancel()
			exec, err := client.WaitForExecution(waitCtx, id, 3*time.Second, time.Duration(timeout)*time.Second, nil)
			if err != nil {
				return nil, err
			}
			status := str(exec, "status")
			result := map[string]any{
				"executionId": id,
				"status":      status,
				"succeeded":   status == gate.StatusSucceeded,
				"duration":    execDuration(exec),
			}
			if gate.IsFailureStatus(status) {
				failed := []stageRow{}
				for _, r := range stageRows(exec, false) {
					if gate.IsFailureStatus(r.Status) {
						failed = append(failed, r)
					}
				}
				result["failedStages"] = failed
			}
			return result, nil
		},
	})

	for _, action := range []struct {
		name, verb, desc string
		destructive      bool
		fn               func(*gate.Client, context.Context, string) error
	}{
		{"cancel_execution", "cancel", "Cancel a running execution.", true,
			func(c *gate.Client, ctx context.Context, id string) error {
				return c.CancelExecution(ctx, id, "cancelled via sc mcp", false)
			}},
		{"pause_execution", "pause", "Pause a running execution at its next stage boundary.", false,
			func(c *gate.Client, ctx context.Context, id string) error { return c.PauseExecution(ctx, id) }},
		{"resume_execution", "resume", "Resume a paused execution.", false,
			func(c *gate.Client, ctx context.Context, id string) error { return c.ResumeExecution(ctx, id) }},
	} {
		action := action
		s.AddTool(mcp.Tool{
			Name:        action.name,
			Description: action.desc,
			Destructive: action.destructive,
			InputSchema: mutatingSchema([]string{"executionId"}, map[string]any{
				"executionId": stringProp("execution id"),
			}),
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				id, err := requireString(args, "executionId")
				if err != nil {
					return nil, err
				}
				if mcp.BoolArg(args, "dryRun") {
					return dryRunResult(action.verb + " execution " + id)
				}
				client, ctx, cancel, err := mcpGate(ctx, args)
				if err != nil {
					return nil, err
				}
				defer cancel()
				if err := action.fn(client, ctx, id); err != nil {
					return nil, err
				}
				audit("mcp."+action.name, id)
				return map[string]any{action.verb + "d": id}, nil
			},
		})
	}

	s.AddTool(mcp.Tool{
		Name:        "restart_stage",
		Description: "Restart one stage of an execution, re-running it and everything downstream. The execution keeps its id and history, so a long pipeline need not repeat the stages that already succeeded. 'overrides' is applied as a patch to the stage's stored context before the restart, so the retry runs with the corrected value; the original failure is preserved under restartDetails.previousException. This patches the execution, not the pipeline definition.",
		InputSchema: mutatingSchema([]string{"executionId", "stage"}, map[string]any{
			"executionId": stringProp("execution id"),
			"stage":       stringProp("stage name or stage id"),
			"overrides":   objProp("context values to set before the retry"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			stage, err := requireString(args, "stage")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			exec, err := client.GetExecution(ctx, id)
			if err != nil {
				return nil, err
			}
			stageID, stageName, err := resolveStage(exec, stage)
			if err != nil {
				return nil, mcp.Errorf("not_found", "%s", err.Error())
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("restart stage " + stageName + " of execution " + id)
			}
			if _, err := client.RestartStageWithOverrides(ctx, id, stageID, mapArg(args, "overrides")); err != nil {
				return nil, err
			}
			audit("mcp.restart_stage", id+"/"+stageName)
			return map[string]any{"restarted": stageName, "stageId": stageID, "executionId": id}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "evaluate_expression",
		Description: "Evaluate a SpEL expression against a live execution's context — the read-only script console. Answers what a stage actually resolved rather than what the definition said it would. Set 'stage' for expressions that need a stage context, such as ${#stage(...)}.",
		ReadOnly:    true,
		InputSchema: schemaWithContext([]string{"executionId", "expression"}, map[string]any{
			"executionId": stringProp("execution id"),
			"expression":  stringProp("SpEL expression, e.g. ${trigger.parameters}"),
			"stage":       stringProp("evaluate with this stage as context (name or id)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			expression, err := requireString(args, "expression")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			stageID := ""
			if stage := mcp.StringArg(args, "stage"); stage != "" {
				exec, err := client.GetExecution(ctx, id)
				if err != nil {
					return nil, err
				}
				stageID, _, err = resolveStage(exec, stage)
				if err != nil {
					return nil, mcp.Errorf("not_found", "%s", err.Error())
				}
			}
			return client.EvaluateExpression(ctx, id, stageID, expression)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "list_pending_judgments",
		Description: "Find executions paused on a manual judgment stage — the pipelines currently holding a deployment open waiting for a human. Derived by scanning running executions, since Gate has no endpoint for it.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext(nil, map[string]any{
			"application": stringProp("application to scan ('*' for all; the default)"),
			"limit":       numberProp("executions to scan per pipeline (default 50)"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app := mcp.StringArg(args, "application")
			if app == "" {
				app = "*"
			}
			limit, ok := intArg(args, "limit")
			if !ok {
				limit = 50
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			return client.ListPendingJudgments(ctx, app, limit)
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "judge_stage",
		Description: "Answer a manual judgment stage: 'continue' lets the pipeline proceed, 'stop' fails it. When the execution has exactly one pending judgment, 'stage' may be omitted.",
		InputSchema: mutatingSchema([]string{"executionId", "judgment"}, map[string]any{
			"executionId": stringProp("execution id"),
			"judgment":    map[string]any{"type": "string", "enum": []string{"continue", "stop"}, "description": "the decision"},
			"stage":       stringProp("stage name or id (optional when only one judgment is pending)"),
			"input":       stringProp("one of the stage's configured judgment inputs"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "executionId")
			if err != nil {
				return nil, err
			}
			judgment, err := requireString(args, "judgment")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			exec, err := client.GetExecution(ctx, id)
			if err != nil {
				return nil, err
			}

			var stageID, stageName string
			if stage := mcp.StringArg(args, "stage"); stage != "" {
				stageID, stageName, err = resolveStage(exec, stage)
				if err != nil {
					return nil, mcp.Errorf("not_found", "%s", err.Error())
				}
			} else {
				candidates := [][2]string{}
				for _, st := range mapList(listField(exec, "stages")) {
					if str(st, "type") == "manualJudgment" && str(st, "status") == gate.StatusRunning {
						candidates = append(candidates, [2]string{str(st, "id"), str(st, "name")})
					}
				}
				if len(candidates) != 1 {
					return nil, mcp.Errorf("ambiguous", "execution %s has %d pending judgments; pass 'stage'", id, len(candidates))
				}
				stageID, stageName = candidates[0][0], candidates[0][1]
			}

			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("answer " + judgment + " on stage " + stageName)
			}
			if _, err := client.JudgeStage(ctx, id, stageID, judgment, mcp.StringArg(args, "input")); err != nil {
				return nil, err
			}
			audit("mcp.judge_stage", id+"/"+stageName+"="+judgment)
			return map[string]any{"executionId": id, "stage": stageName, "judgment": judgment}, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "get_task",
		Description: "Get an ad-hoc orca task by id, including its failure message when it failed.",
		ReadOnly:    true,
		Idempotent:  true,
		InputSchema: schemaWithContext([]string{"taskId"}, map[string]any{
			"taskId": stringProp("task id"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, err := requireString(args, "taskId")
			if err != nil {
				return nil, err
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			task, err := client.GetTask(ctx, id)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"task": task, "status": gate.TaskStatus(task)}
			if msg := gate.TaskFailureMessage(task); msg != "" {
				out["failureMessage"] = msg
			}
			return out, nil
		},
	})

	s.AddTool(mcp.Tool{
		Name:        "submit_task",
		Description: "Submit an ad-hoc orca task: a list of clouddriver operations. This is the write-side escape hatch — every imperative Spinnaker action (deploy a manifest, resize or disable a server group, delete a load balancer) is one of these, including those with no dedicated tool.",
		InputSchema: mutatingSchema([]string{"application", "job"}, map[string]any{
			"application": stringProp("application to scope the task to"),
			"description": stringProp("human-readable description"),
			"job":         map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "the operations to run, each with a 'type'"},
			"wait":        boolProp("wait for the task to finish before returning"),
		}),
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			app, err := requireString(args, "application")
			if err != nil {
				return nil, err
			}
			rawJobs, ok := args["job"].([]any)
			if !ok || len(rawJobs) == 0 {
				return nil, mcp.Errorf("invalid_args", "job must be a non-empty array of operations")
			}
			jobs := make([]gate.JSONMap, 0, len(rawJobs))
			for _, j := range rawJobs {
				m, ok := j.(map[string]any)
				if !ok {
					return nil, mcp.Errorf("invalid_args", "each job entry must be an object")
				}
				jobs = append(jobs, m)
			}
			description := mcp.StringArg(args, "description")
			if description == "" {
				description = "sc mcp task"
			}
			if mcp.BoolArg(args, "dryRun") {
				return dryRunResult("submit task to " + app + ": " + description)
			}
			client, ctx, cancel, err := mcpGate(ctx, args)
			if err != nil {
				return nil, err
			}
			defer cancel()
			taskID, err := client.CreateTask(ctx, gate.TaskRequest{Application: app, Description: description, Job: jobs})
			if err != nil {
				return nil, err
			}
			audit("mcp.submit_task", app+": "+description)
			if !mcp.BoolArg(args, "wait") {
				return map[string]any{"task": taskID, "application": app}, nil
			}
			task, werr := client.WaitForTask(ctx, taskID, 2*time.Second, 4*time.Minute)
			out := map[string]any{"task": taskID, "status": gate.TaskStatus(task)}
			if werr != nil {
				out["error"] = werr.Error()
			}
			return out, nil
		},
	})
}

// toQuery converts a loose JSON object into URL query values.
func toQuery(m map[string]any) url.Values {
	if len(m) == 0 {
		return nil
	}
	q := url.Values{}
	for k, v := range m {
		switch typed := v.(type) {
		case []any:
			for _, item := range typed {
				q.Add(k, jsonCompactScalar(item))
			}
		default:
			q.Set(k, jsonCompactScalar(v))
		}
	}
	return q
}

// jsonCompactScalar renders a scalar for a query value, JSON-encoding anything
// that is not already a string.
func jsonCompactScalar(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeLoosely decodes a JSON body, falling back to the raw string so a tool
// never hides a non-JSON response.
func decodeLoosely(raw []byte) any {
	var v any
	if err := json.Unmarshal(raw, &v); err == nil {
		return v
	}
	return string(raw)
}

// viperString and viperStringMap keep the MCP handlers from importing viper
// directly for the two values they need.
func viperString(key string) string { return viper.GetString(key) }

func viperStringMap(key string) map[string]any { return viper.GetStringMap(key) }
