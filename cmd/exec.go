package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

var execCmd = &cobra.Command{
	Use:     "exec",
	Aliases: []string{"execution", "executions", "ex"},
	Short:   "Inspect and control pipeline executions",
	Long: `Inspect and control pipeline executions — the running instances of a pipeline
definition.

Executions are where Spinnaker keeps the state that matters during an incident:
each stage's resolved context, the manifests it produced, the exception that
stopped it. 'sc exec stages' and 'sc exec eval' read that state; 'sc exec
restart-stage', 'pause', 'resume' and 'judge' act on it.`,
	GroupID: GroupCore,
}

var (
	execListLimit    int
	execListStatuses []string
	execListPipeline string
	execListAll      bool
)

var execListCmd = &cobra.Command{
	Use:     "list [application]",
	Aliases: []string{"ls"},
	Short:   "List an application's executions, newest first",
	Long: `Lists an application's pipeline executions, newest first.

Gate defaults to 10 executions per pipeline when no limit is given, which is a
common reason an execution appears to be missing; --limit raises it.`,
	Args: cobra.ExactArgs(1),
	Example: `  sc exec list demo
  sc exec list demo --status RUNNING
  sc exec list demo --status TERMINAL --limit 50
  sc exec list demo --pipeline deploy -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		execs, err := client.ListExecutions(ctx, args[0], gate.ListExecutionsOptions{
			Limit:              execListLimit,
			Statuses:           execListStatuses,
			PipelineNameFilter: execListPipeline,
			IncludeDeleted:     execListAll,
			// Stage detail is not needed for a listing and makes the response
			// an order of magnitude larger.
			Expand: false,
		})
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(execs)
		}

		t := newTable("ID", "PIPELINE", "STATUS", "STARTED", "DURATION", "TRIGGER", "BY")
		for _, e := range execs {
			trigger := mapField(e, "trigger")
			t.add(
				str(e, "id"),
				ellipsis(str(e, "name"), 28),
				str(e, "status"),
				epochTime(e, "startTime"),
				execDuration(e),
				dash(str(trigger, "type")),
				dash(ellipsis(str(trigger, "user"), 20)),
			)
		}
		t.print("No executions found. Pass --limit to look further back, or check --status.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var execGetCmd = &cobra.Command{
	Use:   "get [execution-id]",
	Short: "Print a full execution as JSON",
	Long:  "Prints the whole execution document, including every stage's resolved context. This is the raw state orca holds.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		exec, err := client.GetExecution(ctx, args[0])
		if err != nil {
			return err
		}
		return render(exec)
	},
}

var execStagesAll bool

var execStagesCmd = &cobra.Command{
	Use:   "stages [execution-id]",
	Short: "Show an execution's stages and where it stands",
	Long: `Shows an execution's stages in dependency order with status, duration and any
failure message.

Synthetic stages — the bookkeeping stages orca injects around real ones — are
hidden unless --all is passed, because they triple the output without adding
information.`,
	Args: cobra.ExactArgs(1),
	Example: `  sc exec stages 01M3Y567A2CTDWCP8T7QAR7GH8
  sc exec stages 01M3Y... --all`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		exec, err := client.GetExecution(ctx, args[0])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(stageRows(exec, execStagesAll))
		}
		printExecutionSummary(exec)
		fmt.Println()
		printStages(exec, execStagesAll)
		return nil
	},
}

// stageRow is one stage, flattened for display.
type stageRow struct {
	Name     string `json:"name" yaml:"name"`
	Type     string `json:"type" yaml:"type"`
	Status   string `json:"status" yaml:"status"`
	StageID  string `json:"stageId" yaml:"stageId"`
	Started  string `json:"started,omitempty" yaml:"started,omitempty"`
	Duration string `json:"duration,omitempty" yaml:"duration,omitempty"`
	Message  string `json:"message,omitempty" yaml:"message,omitempty"`
}

// stageRows flattens an execution's stages in orca's requisite order.
func stageRows(exec map[string]any, includeSynthetic bool) []stageRow {
	stages := mapList(listField(exec, "stages"))
	// orca stores stages unordered; refIds and requisiteStageRefIds carry the
	// graph, and synthetic stages reference a parent. Sorting by start time with
	// refId as a tiebreaker reproduces the order a human expects without having
	// to topologically sort a graph that may not yet have run.
	sort.SliceStable(stages, func(i, j int) bool {
		si, oki := num(stages[i], "startTime")
		sj, okj := num(stages[j], "startTime")
		switch {
		case oki && okj && si != sj:
			return si < sj
		case oki != okj:
			return oki
		default:
			return str(stages[i], "refId") < str(stages[j], "refId")
		}
	})

	rows := []stageRow{}
	for _, s := range stages {
		synthetic := str(s, "syntheticStageOwner") != "" || str(s, "parentStageId") != ""
		if synthetic && !includeSynthetic {
			continue
		}
		name := str(s, "name")
		if synthetic {
			name = "  ↳ " + name
		}
		row := stageRow{
			Name:     name,
			Type:     str(s, "type"),
			Status:   str(s, "status"),
			StageID:  str(s, "id"),
			Duration: execDuration(s),
		}
		if ms, ok := num(s, "startTime"); ok && ms > 0 {
			row.Started = time.UnixMilli(ms).Local().Format("15:04:05")
		}
		row.Message = stageMessage(s)
		rows = append(rows, row)
	}
	return rows
}

// stageMessage extracts the most useful one-line explanation of a stage's state:
// its failure message when it failed, or what it is waiting on when it is not.
func stageMessage(stage map[string]any) string {
	sctx := mapField(stage, "context")

	// A failed stage carries its reason in one of several places depending on
	// which subsystem failed.
	if ex := mapField(sctx, "exception"); ex != nil {
		if details := mapField(ex, "details"); details != nil {
			if errs := listField(details, "errors"); len(errs) > 0 {
				parts := make([]string, 0, len(errs))
				for _, e := range errs {
					parts = append(parts, fmt.Sprint(e))
				}
				return strings.Join(parts, "; ")
			}
			if e := str(details, "error"); e != "" {
				return e
			}
		}
		if e := str(ex, "message"); e != "" {
			return e
		}
	}
	if msg := strOr(sctx, "failureMessage", "exceptionMessage", "errorMessage"); msg != "" {
		return msg
	}
	// Kubernetes stages report stability/health problems here.
	if events := listField(sctx, "events"); len(events) > 0 {
		if last := mapList(events); len(last) > 0 {
			if m := str(last[len(last)-1], "message"); m != "" {
				return m
			}
		}
	}
	// A manual judgment stage's instructions are what the operator needs to see.
	if str(stage, "type") == "manualJudgment" {
		if instructions := str(sctx, "instructions"); instructions != "" {
			return instructions
		}
	}
	if msg := str(sctx, "warnings"); msg != "" {
		return msg
	}
	return ""
}

func printExecutionSummary(exec map[string]any) {
	w := getOutput()
	tw := w.Table()
	fmt.Fprintf(tw, "Execution:\t%s\n", str(exec, "id"))
	fmt.Fprintf(tw, "Application:\t%s\n", str(exec, "application"))
	fmt.Fprintf(tw, "Pipeline:\t%s\n", str(exec, "name"))
	fmt.Fprintf(tw, "Status:\t%s\n", str(exec, "status"))
	fmt.Fprintf(tw, "Started:\t%s\n", epochTime(exec, "startTime"))
	fmt.Fprintf(tw, "Duration:\t%s\n", execDuration(exec))
	trigger := mapField(exec, "trigger")
	fmt.Fprintf(tw, "Trigger:\t%s", dash(str(trigger, "type")))
	if u := str(trigger, "user"); u != "" {
		fmt.Fprintf(tw, " by %s", u)
	}
	fmt.Fprintln(tw)
	if params := mapField(trigger, "parameters"); len(params) > 0 {
		for _, k := range sortedKeys(params) {
			fmt.Fprintf(tw, "Parameter %s:\t%v\n", k, params[k])
		}
	}
	if cancelledBy := str(exec, "canceledBy"); cancelledBy != "" {
		fmt.Fprintf(tw, "Canceled by:\t%s\n", cancelledBy)
		if reason := str(exec, "cancellationReason"); reason != "" {
			fmt.Fprintf(tw, "Reason:\t%s\n", reason)
		}
	}
	w.FlushTable(tw)
}

func printStages(exec map[string]any, includeSynthetic bool) {
	rows := stageRows(exec, includeSynthetic)
	t := newTable("STAGE", "TYPE", "STATUS", "STARTED", "DURATION", "DETAIL")
	for _, r := range rows {
		t.add(r.Name, r.Type, r.Status, dash(r.Started), r.Duration, ellipsis(r.Message, 60))
	}
	t.print("This execution has no stages.")
}

var (
	execCancelReason string
	execCancelForce  bool
)

var execCancelCmd = &cobra.Command{
	Use:   "cancel [execution-id]",
	Short: "Cancel a running execution",
	Long: `Cancels a running execution.

--force uses the admin force-cancel endpoint instead, for an execution that orca
still believes is running but will not respond to a normal cancel — typically one
orphaned by an orca restart. That endpoint requires admin permissions.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would cancel execution %s", args[0])
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if execCancelForce {
			if err := client.ForceCancelExecution(ctx, args[0], "PIPELINE"); err != nil {
				return err
			}
			audit("exec.force-cancel", args[0])
			fmt.Fprintf(os.Stderr, "Force-cancelled execution %s\n", args[0])
			return nil
		}
		if err := client.CancelExecution(ctx, args[0], execCancelReason, false); err != nil {
			return err
		}
		audit("exec.cancel", args[0])
		fmt.Fprintf(os.Stderr, "Cancelled execution %s\n", args[0])
		return nil
	},
}

var execPauseCmd = &cobra.Command{
	Use:   "pause [execution-id]",
	Short: "Pause a running execution",
	Long:  "Pauses a running execution. orca stops it at the next stage boundary; in-flight stages are not interrupted.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would pause execution %s", args[0])
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.PauseExecution(ctx, args[0]); err != nil {
			return err
		}
		audit("exec.pause", args[0])
		fmt.Fprintf(os.Stderr, "Paused execution %s\n", args[0])
		return nil
	},
}

var execResumeCmd = &cobra.Command{
	Use:   "resume [execution-id]",
	Short: "Resume a paused execution",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would resume execution %s", args[0])
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.ResumeExecution(ctx, args[0]); err != nil {
			return err
		}
		audit("exec.resume", args[0])
		fmt.Fprintf(os.Stderr, "Resumed execution %s\n", args[0])
		return nil
	},
}

var execDeleteForce bool

var execDeleteCmd = &cobra.Command{
	Use:   "delete [execution-id]",
	Short: "Delete an execution record",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would delete execution %s", args[0])
			return nil
		}
		if !execDeleteForce {
			if err := confirm(fmt.Sprintf("Permanently delete execution %s?", args[0])); err != nil {
				return err
			}
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.DeleteExecution(ctx, args[0]); err != nil {
			return err
		}
		audit("exec.delete", args[0])
		fmt.Fprintf(os.Stderr, "Deleted execution %s\n", args[0])
		return nil
	},
}

var (
	execRestartParams []string
	execRestartFollow bool
)

var execRestartStageCmd = &cobra.Command{
	Use:   "restart-stage [execution-id] [stage-name-or-id]",
	Short: "Restart one stage of an execution",
	Long: `Restarts a single stage, re-running it and everything downstream of it.

This is Spinnaker's native retry: the execution keeps its id and history rather
than starting over, so a long pipeline that failed at stage nine does not have to
repeat stages one to eight.

--set corrects a value before the retry. It is applied as a patch to the stage's
stored context, which orca preserves across a restart, so the new attempt runs
with the corrected value and the original failure is kept on the stage under
restartDetails.previousException. Note that this patches the execution, not the
pipeline definition: the next fresh run uses the definition's value again.`,
	Args: cobra.ExactArgs(2),
	Example: `  sc exec restart-stage 01M3Y... deploy-prod
  sc exec restart-stage 01M3Y... deploy-prod --set manifestArtifactAccount=embedded-artifact
  sc exec restart-stage 01M3Y... deploy-prod --follow`,
	RunE: func(cmd *cobra.Command, args []string) error {
		overrides, err := parseKeyValues(execRestartParams)
		if err != nil {
			return err
		}
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx := cmd.Context()
		lookupCtx, cancel := cmdContext(ctx)
		exec, err := client.GetExecution(lookupCtx, args[0])
		if err != nil {
			cancel()
			return err
		}
		stageID, stageName, err := resolveStage(exec, args[1])
		if err != nil {
			cancel()
			return err
		}
		if isDryRun() {
			cancel()
			dryRunMsg("would restart stage %s (%s) of execution %s with overrides %v", stageName, stageID, args[0], overrides)
			return nil
		}
		if _, err := client.RestartStageWithOverrides(lookupCtx, args[0], stageID, overrides); err != nil {
			cancel()
			return err
		}
		cancel()
		audit("exec.restart-stage", fmt.Sprintf("%s/%s", args[0], stageName))
		fmt.Fprintf(os.Stderr, "Restarted stage %q of execution %s\n", stageName, args[0])
		if execRestartFollow {
			// orca queues the restart rather than applying it inline, so for a
			// moment the execution still reports the terminal status it had before.
			// Following immediately would observe that and report the old failure as
			// if the retry had failed.
			if err := waitForRestart(ctx, client, args[0], 60*time.Second); err != nil {
				return err
			}
			return followExecution(ctx, client, args[0], true)
		}
		return nil
	},
}

// waitForRestart polls until a restarted execution has left its terminal status,
// so a follower does not report the pre-restart outcome.
func waitForRestart(ctx context.Context, client *gate.Client, execID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		exec, err := client.GetExecution(ctx, execID)
		if err == nil && !gate.IsTerminalStatus(str(exec, "status")) {
			return nil
		}
		if time.Now().After(deadline) {
			// Not fatal: the restart may simply have completed faster than the poll
			// interval, in which case following still reports the right outcome.
			debugf("execution %s was still terminal %s after the restart", execID, timeout)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// resolveStage finds a stage by id, exact name, or unique case-insensitive name
// match, so the CLI accepts the stage name a human knows rather than a UUID.
func resolveStage(exec map[string]any, nameOrID string) (id, name string, err error) {
	stages := mapList(listField(exec, "stages"))
	var matches []map[string]any
	for _, s := range stages {
		if str(s, "id") == nameOrID {
			return str(s, "id"), str(s, "name"), nil
		}
		if str(s, "name") == nameOrID {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		for _, s := range stages {
			if strings.EqualFold(str(s, "name"), nameOrID) {
				matches = append(matches, s)
			}
		}
	}
	switch len(matches) {
	case 1:
		return str(matches[0], "id"), str(matches[0], "name"), nil
	case 0:
		available := []string{}
		for _, s := range stages {
			if n := str(s, "name"); n != "" && str(s, "syntheticStageOwner") == "" {
				available = append(available, n)
			}
		}
		return "", "", fmt.Errorf("no stage named or identified by %q in this execution (stages: %s)",
			nameOrID, strings.Join(available, ", "))
	default:
		ids := []string{}
		for _, s := range matches {
			ids = append(ids, str(s, "id"))
		}
		return "", "", fmt.Errorf("%d stages are named %q; pass a stage id instead (%s)",
			len(matches), nameOrID, strings.Join(ids, ", "))
	}
}

var execEvalStage string

var execEvalCmd = &cobra.Command{
	Use:   "eval [execution-id] [expression]",
	Short: "Evaluate a SpEL expression against an execution",
	Long: `Evaluates a SpEL (Spinnaker Expression Language) expression against a live
execution's context.

This is the closest thing Spinnaker has to a read-only script console. It answers
what a stage actually resolved rather than what the definition said it would,
which is usually the difference between guessing at a templating bug and seeing
it. --stage evaluates with a particular stage as context, which ${#stage(...)}
and relative references require.`,
	Args: cobra.ExactArgs(2),
	Example: `  sc exec eval 01M3Y... '${trigger.parameters}'
  sc exec eval 01M3Y... '${#stage("deploy").context.manifests}' --stage deploy
  sc exec eval 01M3Y... '${execution.stages.?[status == "TERMINAL"].![name]}'`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		stageID := ""
		if execEvalStage != "" {
			exec, err := client.GetExecution(ctx, args[0])
			if err != nil {
				return err
			}
			stageID, _, err = resolveStage(exec, execEvalStage)
			if err != nil {
				return err
			}
		}
		result, err := client.EvaluateExpression(ctx, args[0], stageID, args[1])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(result)
		}
		// orca answers with {result: …, detail: {…}}; the detail holds the
		// evaluation errors, which are the point when an expression misbehaves.
		if detail := mapField(result, "detail"); len(detail) > 0 {
			fmt.Fprintln(os.Stderr, "Evaluation reported problems:")
			for _, k := range sortedKeys(detail) {
				fmt.Fprintf(os.Stderr, "  %s: %v\n", k, detail[k])
			}
		}
		if v, ok := result["result"]; ok {
			switch typed := v.(type) {
			case string:
				fmt.Println(typed)
			default:
				return getOutput().PrintJSON(v)
			}
			return nil
		}
		return getOutput().PrintJSON(result)
	},
}

var execFailedStagesLimit int

var execFailedStagesCmd = &cobra.Command{
	Use:   "failed-stages [execution-id]",
	Short: "Show only the failed stages, descending into nested pipelines",
	Long: `Shows just an execution's failed stages, following nested pipeline stages into
their child executions.

This is the fast path for triage: it avoids pulling a whole expanded execution to
find what broke, and it crosses the pipeline boundary that 'sc exec stages' stops
at.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		failed, err := client.FailedStages(ctx, args[0], execFailedStagesLimit)
		if err != nil {
			// orca's /pipelines/failedStages dereferences the optional
			// tasks.controller.failedStages config block without a null check, so it
			// returns a 500 on any installation that has not set it. Rather than
			// surface that, the failed stages are computed from the execution — which
			// loses only the descent into nested pipeline executions.
			if isFailedStagesConfigBug(err) {
				fmt.Fprintln(os.Stderr, "Note: orca's failedStages endpoint is unconfigured on this installation, "+
					"so failed stages were computed locally (nested pipeline executions are not followed).\n"+
					"      To enable the endpoint, set tasks.controller.failedStages.onlyIncludeStagesThatFailPipelines "+
					"in orca-local.yml and restart orca.")
				return printLocalFailedStages(ctx, client, args[0])
			}
			return err
		}
		if outputIsStructured() {
			return render(failed)
		}
		t := newTable("STAGE", "TYPE", "STATUS", "PIPELINE", "EXECUTION", "DETAIL")
		for _, f := range failed {
			// A failure inside a nested pipeline reports the child execution, which is
			// where the stage actually ran.
			execID := f.PipelineExecutionID
			if f.ChildPipelineExecutionID != "" {
				execID = f.ChildPipelineExecutionID
			}
			pipeline := f.PipelineExecutionName
			if f.ChildPipelineExecutionName != "" {
				pipeline = f.ChildPipelineExecutionName
			}
			t.add(
				dash(f.StageName),
				dash(f.StageType),
				dash(f.StageStatus),
				dash(ellipsis(pipeline, 22)),
				dash(execID),
				// NOT_FOUND_CHECK_UI is orca's placeholder for "the exception is not on
				// this record"; the real message is in the stage context.
				ellipsis(dash(failureDetail(f.StageException)), 48),
			)
		}
		t.print("No failed stages in this execution.")
		return nil
	},
}

// failureDetail renders a failedStages exception, translating orca's
// NOT_FOUND_CHECK_UI placeholder into something actionable.
func failureDetail(exception string) string {
	if exception == "NOT_FOUND_CHECK_UI" {
		return "(not recorded here — see 'sc exec stages')"
	}
	return exception
}

// isFailedStagesConfigBug recognises the orca NPE caused by an unset
// tasks.controller.failedStages block.
func isFailedStagesConfigBug(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "onlyIncludeStagesThatFailPipelines") ||
		(strings.Contains(msg, "failedStages") && strings.Contains(msg, "null object"))
}

// printLocalFailedStages derives an execution's failed stages client-side.
func printLocalFailedStages(ctx context.Context, client *gate.Client, execID string) error {
	exec, err := client.GetExecution(ctx, execID)
	if err != nil {
		return err
	}
	rows := []stageRow{}
	for _, r := range stageRows(exec, true) {
		if gate.IsFailureStatus(r.Status) {
			rows = append(rows, r)
		}
	}
	if outputIsStructured() {
		return render(rows)
	}
	t := newTable("STAGE", "TYPE", "STATUS", "DURATION", "DETAIL")
	for _, r := range rows {
		t.add(strings.TrimSpace(r.Name), r.Type, r.Status, r.Duration, ellipsis(r.Message, 60))
	}
	t.print("No failed stages in this execution.")
	return nil
}

var (
	execSearchApp      string
	execSearchPipeline string
	execSearchStatuses []string
	execSearchTriggers []string
	execSearchSince    time.Duration
	execSearchSize     int
)

var execSearchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search executions across applications by trigger criteria",
	Long: `Searches executions by trigger criteria across one or all applications.

Unlike 'sc exec list', which walks one application's history, this searches by
how executions were triggered — useful for "what did this commit deploy?" or
"which pipelines did that webhook start?".`,
	Example: `  sc exec search --application '*' --status RUNNING
  sc exec search --application demo --trigger-type webhook --since 24h
  sc exec search --pipeline deploy --status TERMINAL`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		opts := gate.SearchExecutionsOptions{
			Application:  execSearchApp,
			PipelineName: execSearchPipeline,
			Statuses:     execSearchStatuses,
			TriggerTypes: execSearchTriggers,
			Size:         execSearchSize,
		}
		if execSearchSince > 0 {
			opts.StartBoundary = time.Now().Add(-execSearchSince)
		}
		execs, err := client.SearchExecutions(ctx, opts)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(execs)
		}
		t := newTable("ID", "APPLICATION", "PIPELINE", "STATUS", "STARTED", "DURATION", "TRIGGER")
		for _, e := range execs {
			t.add(
				str(e, "id"),
				str(e, "application"),
				ellipsis(str(e, "name"), 24),
				str(e, "status"),
				epochTime(e, "startTime"),
				execDuration(e),
				dash(str(mapField(e, "trigger"), "type")),
			)
		}
		t.print("No executions matched.")
		return nil
	},
}

var (
	execWaitTimeout  time.Duration
	execWaitInterval time.Duration
	execWaitQuiet    bool
)

var execWaitCmd = &cobra.Command{
	Use:     "wait [execution-id]",
	Aliases: []string{"follow"},
	Short:   "Wait for an execution to finish, rendering stage progress",
	Long: `Waits for an execution to reach a terminal status.

Exits 0 when it succeeded and 8 when it failed, so this is the command CI should
gate on. Stage transitions are printed as they happen unless --quiet is set.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		return followExecution(cmd.Context(), client, args[0], !execWaitQuiet)
	},
}

// followExecution polls an execution to completion, optionally printing each
// stage status transition, then reports the outcome.
//
// It deliberately does not use the global --timeout: a deployment legitimately
// runs longer than an API request should, so the bound here is --wait-timeout.
func followExecution(ctx context.Context, client *gate.Client, id string, verbose bool) error {
	seen := map[string]string{}
	onUpdate := func(exec map[string]any) {
		if !verbose {
			return
		}
		for _, s := range mapList(listField(exec, "stages")) {
			if str(s, "syntheticStageOwner") != "" || str(s, "parentStageId") != "" {
				continue
			}
			sid, status := str(s, "id"), str(s, "status")
			if status == "" || status == gate.StatusNotStarted {
				continue
			}
			if seen[sid] == status {
				continue
			}
			seen[sid] = status
			line := fmt.Sprintf("  %-10s %-28s %s", status, ellipsis(str(s, "name"), 28), execDuration(s))
			if msg := stageMessage(s); msg != "" && gate.IsFailureStatus(status) {
				line += "  " + ellipsis(msg, 80)
			}
			fmt.Fprintln(os.Stderr, line)
		}
	}

	exec, err := client.WaitForExecution(ctx, id, execWaitInterval, execWaitTimeout, onUpdate)
	if err != nil {
		return err
	}
	status := str(exec, "status")

	if outputIsStructured() {
		if err := render(exec); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nExecution %s finished: %s (%s)\n", id, status, execDuration(exec))
	}

	if gate.IsFailureStatus(status) {
		// Point straight at the failure rather than making the user run another
		// command to find it.
		for _, r := range stageRows(exec, false) {
			if gate.IsFailureStatus(r.Status) && r.Message != "" {
				return &ExecFailedError{Msg: fmt.Sprintf("execution %s %s at stage %q: %s",
					id, status, strings.TrimSpace(r.Name), r.Message)}
			}
		}
		return &ExecFailedError{Msg: fmt.Sprintf("execution %s finished %s", id, status)}
	}
	return nil
}

func init() {
	execListCmd.Flags().IntVar(&execListLimit, "limit", 20, "number of executions to return per pipeline")
	execListCmd.Flags().StringArrayVar(&execListStatuses, "status", nil, "filter by status, e.g. RUNNING, TERMINAL, SUCCEEDED (repeatable)")
	execListCmd.Flags().StringVar(&execListPipeline, "pipeline", "", "only executions of pipelines whose name contains this")
	execListCmd.Flags().BoolVar(&execListAll, "include-deleted", false, "include executions of deleted pipeline definitions")

	execStagesCmd.Flags().BoolVar(&execStagesAll, "all", false, "include orca's synthetic bookkeeping stages")

	execCancelCmd.Flags().StringVar(&execCancelReason, "reason", "", "cancellation reason recorded on the execution")
	execCancelCmd.Flags().BoolVar(&execCancelForce, "force", false, "use the admin force-cancel endpoint for a stuck execution (requires admin)")

	execDeleteCmd.Flags().BoolVar(&execDeleteForce, "force", false, "skip the confirmation prompt")

	execRestartStageCmd.Flags().StringArrayVar(&execRestartParams, "set", nil, "stage context override KEY=VALUE applied before the retry (repeatable)")
	execRestartStageCmd.Flags().BoolVar(&execRestartFollow, "follow", false, "follow the execution after restarting")

	execEvalCmd.Flags().StringVar(&execEvalStage, "stage", "", "evaluate with this stage as context (name or id)")

	execFailedStagesCmd.Flags().IntVar(&execFailedStagesLimit, "limit", 5, "maximum nested pipeline executions to descend into")

	execSearchCmd.Flags().StringVar(&execSearchApp, "application", "*", "application to search ('*' for all)")
	execSearchCmd.Flags().StringVar(&execSearchPipeline, "pipeline", "", "only executions of this pipeline name")
	execSearchCmd.Flags().StringArrayVar(&execSearchStatuses, "status", nil, "filter by status (repeatable)")
	execSearchCmd.Flags().StringArrayVar(&execSearchTriggers, "trigger-type", nil, "filter by trigger type, e.g. manual, webhook, jenkins (repeatable)")
	execSearchCmd.Flags().DurationVar(&execSearchSince, "since", 0, "only executions triggered within this period, e.g. 24h")
	execSearchCmd.Flags().IntVar(&execSearchSize, "size", 50, "maximum results")

	execWaitCmd.Flags().DurationVar(&execWaitTimeout, "wait-timeout", 0, "give up after this long (0 = no limit)")
	execWaitCmd.Flags().DurationVar(&execWaitInterval, "interval", 3*time.Second, "poll interval")
	execWaitCmd.Flags().BoolVar(&execWaitQuiet, "quiet", false, "do not print stage transitions")

	// --follow on 'pipeline run' and 'exec restart-stage' shares these knobs.
	pipelineRunCmd.Flags().DurationVar(&execWaitTimeout, "wait-timeout", 0, "with --wait/--follow, give up after this long (0 = no limit)")
	pipelineRunCmd.Flags().DurationVar(&execWaitInterval, "interval", 3*time.Second, "with --wait/--follow, poll interval")

	execCmd.AddCommand(
		execListCmd, execGetCmd, execStagesCmd, execCancelCmd, execPauseCmd, execResumeCmd,
		execDeleteCmd, execRestartStageCmd, execEvalCmd, execFailedStagesCmd, execSearchCmd, execWaitCmd,
	)
	rootCmd.AddCommand(execCmd)
}
