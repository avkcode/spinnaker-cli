package gate

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Terminal execution and stage statuses. orca uses the same vocabulary for both.
const (
	StatusNotStarted = "NOT_STARTED"
	StatusRunning    = "RUNNING"
	StatusPaused     = "PAUSED"
	StatusSuspended  = "SUSPENDED"
	StatusSucceeded  = "SUCCEEDED"
	StatusFailedCont = "FAILED_CONTINUE"
	StatusTerminal   = "TERMINAL"
	StatusCanceled   = "CANCELED"
	StatusRedirect   = "REDIRECT"
	StatusStopped    = "STOPPED"
	StatusBuffered   = "BUFFERED"
	StatusSkipped    = "SKIPPED"
)

// IsTerminalStatus reports whether a status means orca is done with the
// execution and will not advance it further without an explicit restart.
func IsTerminalStatus(status string) bool {
	switch strings.ToUpper(status) {
	case StatusSucceeded, StatusTerminal, StatusCanceled, StatusStopped, StatusSkipped, StatusFailedCont:
		return true
	default:
		return false
	}
}

// IsFailureStatus reports whether a status represents a failure.
func IsFailureStatus(status string) bool {
	switch strings.ToUpper(status) {
	case StatusTerminal, StatusCanceled, StatusStopped, StatusFailedCont:
		return true
	default:
		return false
	}
}

// ListExecutionsOptions filters an application's execution history.
type ListExecutionsOptions struct {
	// Limit is the number of executions to return per pipeline (Gate defaults to
	// 10 when unset, which is a common source of "missing" executions).
	Limit int
	// Statuses filters by execution status (e.g. RUNNING, TERMINAL).
	Statuses []string
	// Expand includes full stage detail. Unexpanded responses are far smaller
	// and enough for a listing.
	Expand bool
	// PipelineNameFilter restricts results to pipelines whose name contains this.
	PipelineNameFilter string
	// PipelineLimit caps the number of distinct pipelines considered.
	PipelineLimit int
	// IncludeDeleted includes executions of deleted pipeline definitions.
	IncludeDeleted bool
}

// ListExecutions returns an application's pipeline executions, newest first.
func (c *Client) ListExecutions(ctx context.Context, app string, opts ListExecutionsOptions) (JSONList, error) {
	// This endpoint rejects a wildcard application with an opaque 400; only the
	// execution-search endpoint accepts one.
	if app == "*" {
		return nil, fmt.Errorf("listing executions does not accept a wildcard application; use SearchExecutions (sc exec search --application '*')")
	}
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if len(opts.Statuses) > 0 {
		q.Set("statuses", strings.Join(upperAll(opts.Statuses), ","))
	}
	q.Set("expand", strconv.FormatBool(opts.Expand))
	if opts.PipelineNameFilter != "" {
		q.Set("pipelineNameFilter", opts.PipelineNameFilter)
	}
	if opts.PipelineLimit > 0 {
		q.Set("pipelineLimit", strconv.Itoa(opts.PipelineLimit))
	}
	if opts.IncludeDeleted {
		q.Set("includeDeletedPipelines", "true")
	}
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(app)+"/pipelines", q, &out)
}

// GetExecution returns one execution, including all stage contexts.
func (c *Client) GetExecution(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/pipelines/"+escape(id), nil, &out)
}

// LatestExecutionsOptions filters GET /executions.
type LatestExecutionsOptions struct {
	// PipelineConfigIDs returns recent executions of these pipeline definitions.
	PipelineConfigIDs []string
	// ExecutionIDs returns these specific executions. Mutually exclusive with
	// PipelineConfigIDs — Gate throws if both are supplied.
	ExecutionIDs []string
	// Limit is executions returned per pipeline config (Gate defaults to 1).
	Limit int
	// Statuses filters by status; ignored when ExecutionIDs is set.
	Statuses []string
	// Expand includes full stage detail.
	Expand bool
}

// LatestExecutions fetches executions by pipeline config id or by execution id.
func (c *Client) LatestExecutions(ctx context.Context, opts LatestExecutionsOptions) (JSONList, error) {
	if len(opts.PipelineConfigIDs) > 0 && len(opts.ExecutionIDs) > 0 {
		return nil, fmt.Errorf("pipelineConfigIds and executionIds are mutually exclusive")
	}
	if len(opts.PipelineConfigIDs) == 0 && len(opts.ExecutionIDs) == 0 {
		return nil, fmt.Errorf("one of pipelineConfigIds or executionIds is required")
	}
	q := url.Values{}
	if len(opts.PipelineConfigIDs) > 0 {
		q.Set("pipelineConfigIds", strings.Join(opts.PipelineConfigIDs, ","))
	}
	if len(opts.ExecutionIDs) > 0 {
		q.Set("executionIds", strings.Join(opts.ExecutionIDs, ","))
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if len(opts.Statuses) > 0 {
		q.Set("statuses", strings.Join(upperAll(opts.Statuses), ","))
	}
	q.Set("expand", strconv.FormatBool(opts.Expand))
	var out JSONList
	return out, c.get(ctx, "/executions", q, &out)
}

// SearchExecutionsOptions filters an execution search by trigger criteria.
type SearchExecutionsOptions struct {
	// Application to search; "*" searches every application.
	Application  string
	TriggerTypes []string
	PipelineName string
	EventID      string
	// Trigger is a base64-encoded JSON trigger subset to match against.
	Trigger       string
	StartBoundary time.Time
	EndBoundary   time.Time
	Statuses      []string
	StartIndex    int
	Size          int
	Reverse       bool
	Expand        bool
}

// SearchExecutions searches executions by trigger criteria, newest first.
//
// Application may be "*" to search every application — the one Gate endpoint
// that accepts a wildcard.
func (c *Client) SearchExecutions(ctx context.Context, opts SearchExecutionsOptions) (JSONList, error) {
	app := opts.Application
	if app == "" {
		app = "*"
	}
	q := url.Values{}
	if len(opts.TriggerTypes) > 0 {
		q.Set("triggerTypes", strings.Join(opts.TriggerTypes, ","))
	}
	if opts.PipelineName != "" {
		q.Set("pipelineName", opts.PipelineName)
	}
	if opts.EventID != "" {
		q.Set("eventId", opts.EventID)
	}
	if opts.Trigger != "" {
		q.Set("trigger", opts.Trigger)
	}
	if !opts.StartBoundary.IsZero() {
		q.Set("triggerTimeStartBoundary", strconv.FormatInt(opts.StartBoundary.UnixMilli(), 10))
	}
	if !opts.EndBoundary.IsZero() {
		q.Set("triggerTimeEndBoundary", strconv.FormatInt(opts.EndBoundary.UnixMilli(), 10))
	}
	if len(opts.Statuses) > 0 {
		q.Set("statuses", strings.Join(upperAll(opts.Statuses), ","))
	}
	if opts.StartIndex > 0 {
		q.Set("startIndex", strconv.Itoa(opts.StartIndex))
	}
	if opts.Size > 0 {
		q.Set("size", strconv.Itoa(opts.Size))
	}
	if opts.Reverse {
		q.Set("reverse", "true")
	}
	if opts.Expand {
		q.Set("expand", "true")
	}
	var out JSONList
	return out, c.get(ctx, "/applications/"+escapeApplication(app)+"/executions/search", q, &out)
}

// GetFailedStages returns an execution's failed stages, descending into nested
// pipeline executions. This is the fast path for triage: it avoids pulling a
// whole expanded execution just to find what broke.
func (c *Client) GetFailedStages(ctx context.Context, executionID string, limit int) ([]any, error) {
	q := url.Values{"executionId": {executionID}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []any
	return out, c.get(ctx, "/executions/failedStages", q, &out)
}

// CancelExecution cancels a running execution.
func (c *Client) CancelExecution(ctx context.Context, id, reason string, force bool) error {
	q := url.Values{}
	if reason != "" {
		q.Set("reason", reason)
	}
	if force {
		q.Set("force", "true")
	}
	return c.put(ctx, "/pipelines/"+escape(id)+"/cancel", q, nil, nil)
}

// PauseExecution pauses a running execution between stages.
func (c *Client) PauseExecution(ctx context.Context, id string) error {
	return c.put(ctx, "/pipelines/"+escape(id)+"/pause", nil, nil, nil)
}

// ResumeExecution resumes a paused execution.
func (c *Client) ResumeExecution(ctx context.Context, id string) error {
	return c.put(ctx, "/pipelines/"+escape(id)+"/resume", nil, nil, nil)
}

// DeleteExecution permanently removes an execution record.
func (c *Client) DeleteExecution(ctx context.Context, id string) error {
	return c.delete(ctx, "/pipelines/"+escape(id), nil, nil)
}

// ForceCancelExecution force-cancels an execution that will not respond to a
// normal cancel — a "zombie" left running by an orca restart. Requires admin.
func (c *Client) ForceCancelExecution(ctx context.Context, id, executionType string) error {
	if executionType == "" {
		executionType = "PIPELINE"
	}
	q := url.Values{"executionId": {id}, "executionType": {executionType}}
	return c.put(ctx, "/admin/executions/forceCancel", q, nil, nil)
}

// RestartStage restarts a single stage of an execution, re-running it and every
// downstream stage.
//
// body is NOT a general stage-context override, despite looking like one: orca
// passes it only to updatePreconditionStageExpression, which rewrites
// "preconditions" on checkPreconditions stages and ignores everything else. To
// change a value before the retry, use RestartStageWithOverrides.
func (c *Client) RestartStage(ctx context.Context, executionID, stageID string, body JSONMap) (JSONMap, error) {
	if body == nil {
		body = JSONMap{}
	}
	var out JSONMap
	return out, c.put(ctx, "/pipelines/"+escape(executionID)+"/stages/"+escape(stageID)+"/restart", nil, body, &out)
}

// RestartStageWithOverrides patches a stage's context and then restarts it, so
// the retry runs with corrected values.
//
// Two calls are needed because neither endpoint does both: the restart body only
// reaches checkPreconditions stages, while PATCH stage writes the context but
// does not re-run anything. orca's restart path resets a stage's status, times
// and tasks but preserves its stored context, so a patch applied first survives
// into the new attempt — and the old failure is kept under
// context.restartDetails.previousException.
func (c *Client) RestartStageWithOverrides(ctx context.Context, executionID, stageID string, overrides JSONMap) (JSONMap, error) {
	if len(overrides) > 0 {
		if _, err := c.UpdateStage(ctx, executionID, stageID, overrides); err != nil {
			return nil, fmt.Errorf("applying stage context overrides before restart: %w", err)
		}
	}
	return c.RestartStage(ctx, executionID, stageID, nil)
}

// FailedStage is one entry from the failedStages endpoint. The field names are
// stage-prefixed and differ from those on a stage inside an execution document.
type FailedStage struct {
	StageID        string `json:"stageId" yaml:"stageId"`
	StageName      string `json:"stageName" yaml:"stageName"`
	StageType      string `json:"stageType" yaml:"stageType"`
	StageStatus    string `json:"stageStatus" yaml:"stageStatus"`
	StageException string `json:"stageException,omitempty" yaml:"stageException,omitempty"`

	PipelineApplication   string `json:"pipelineApplication,omitempty" yaml:"pipelineApplication,omitempty"`
	PipelineExecutionID   string `json:"pipelineExecutionId,omitempty" yaml:"pipelineExecutionId,omitempty"`
	PipelineExecutionName string `json:"pipelineExecutionName,omitempty" yaml:"pipelineExecutionName,omitempty"`
	PipelineExecutionURL  string `json:"pipelineExecutionUrl,omitempty" yaml:"pipelineExecutionUrl,omitempty"`

	// Child/parent fields are populated when the failure is inside a nested
	// pipeline execution.
	ChildPipelineExecutionID   string `json:"childPipelineExecutionId,omitempty" yaml:"childPipelineExecutionId,omitempty"`
	ChildPipelineExecutionName string `json:"childPipelineExecutionName,omitempty" yaml:"childPipelineExecutionName,omitempty"`
	ParentPipelineExecutionID  string `json:"parentPipelineExecutionId,omitempty" yaml:"parentPipelineExecutionId,omitempty"`
}

// FailedStages returns an execution's failed stages as typed entries.
func (c *Client) FailedStages(ctx context.Context, executionID string, limit int) ([]FailedStage, error) {
	q := url.Values{"executionId": {executionID}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []FailedStage
	return out, c.get(ctx, "/executions/failedStages", q, &out)
}

// UpdateStage patches a stage's context. This is the endpoint behind a manual
// judgment decision; see JudgeStage for the typed form.
func (c *Client) UpdateStage(ctx context.Context, executionID, stageID string, context JSONMap) (JSONMap, error) {
	var out JSONMap
	return out, c.patch(ctx, "/pipelines/"+escape(executionID)+"/stages/"+escape(stageID), nil, context, &out)
}

// Manual judgment decisions.
const (
	JudgmentContinue = "continue"
	JudgmentStop     = "stop"
)

// JudgeStage answers a manual judgment stage. judgment must be "continue" or
// "stop"; input selects one of the stage's configured judgmentInputs.
func (c *Client) JudgeStage(ctx context.Context, executionID, stageID, judgment, input string) (JSONMap, error) {
	j := strings.ToLower(judgment)
	if j != JudgmentContinue && j != JudgmentStop {
		return nil, fmt.Errorf("judgment must be %q or %q, got %q", JudgmentContinue, JudgmentStop, judgment)
	}
	// orca expects a capitalized judgmentStatus ("Continue"/"Stop").
	status := strings.ToUpper(j[:1]) + j[1:]
	patch := JSONMap{"judgmentStatus": status}
	if input != "" {
		patch["judgmentInput"] = input
	}
	return c.UpdateStage(ctx, executionID, stageID, patch)
}

// PendingJudgment is an execution stage waiting on a human decision.
type PendingJudgment struct {
	ExecutionID  string   `json:"executionId"`
	Application  string   `json:"application"`
	Pipeline     string   `json:"pipeline"`
	StageID      string   `json:"stageId"`
	StageName    string   `json:"stageName"`
	Instructions string   `json:"instructions,omitempty"`
	Inputs       []string `json:"inputs,omitempty"`
	StartTime    int64    `json:"startTime,omitempty"`
}

// ListPendingJudgments finds every execution that is paused on a manual judgment
// stage, for one application or (with "*" or an empty string) for all of them.
//
// There is no Gate endpoint for this: it is derived by fetching running
// executions expanded and filtering their stages for a RUNNING manualJudgment.
//
// The two cases use different endpoints because only one of them accepts a
// wildcard application. /applications/{app}/pipelines rejects "*" with a 400,
// while /applications/{app}/executions/search accepts it and documents it.
func (c *Client) ListPendingJudgments(ctx context.Context, app string, limit int) ([]PendingJudgment, error) {
	if limit <= 0 {
		limit = 50
	}
	var execs JSONList
	var err error
	if app == "" || app == "*" {
		execs, err = c.SearchExecutions(ctx, SearchExecutionsOptions{
			Application: "*",
			Statuses:    []string{StatusRunning, StatusPaused},
			Size:        limit,
			Expand:      true,
		})
	} else {
		execs, err = c.ListExecutions(ctx, app, ListExecutionsOptions{
			Limit:    limit,
			Statuses: []string{StatusRunning, StatusPaused},
			Expand:   true,
		})
	}
	if err != nil {
		return nil, err
	}
	pending := []PendingJudgment{}
	for _, e := range execs {
		execID, _ := e["id"].(string)
		application, _ := e["application"].(string)
		pipeline, _ := e["name"].(string)
		stages, _ := e["stages"].([]any)
		for _, s := range stages {
			stage, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if stage["type"] != "manualJudgment" {
				continue
			}
			if status, _ := stage["status"].(string); strings.ToUpper(status) != StatusRunning {
				continue
			}
			pj := PendingJudgment{
				ExecutionID: execID,
				Application: application,
				Pipeline:    pipeline,
			}
			pj.StageID, _ = stage["id"].(string)
			pj.StageName, _ = stage["name"].(string)
			if st, ok := stage["startTime"].(float64); ok {
				pj.StartTime = int64(st)
			}
			if sctx, ok := stage["context"].(map[string]any); ok {
				pj.Instructions, _ = sctx["instructions"].(string)
				if inputs, ok := sctx["judgmentInputs"].([]any); ok {
					for _, in := range inputs {
						switch v := in.(type) {
						case string:
							pj.Inputs = append(pj.Inputs, v)
						case map[string]any:
							if value, ok := v["value"].(string); ok {
								pj.Inputs = append(pj.Inputs, value)
							}
						}
					}
				}
			}
			pending = append(pending, pj)
		}
	}
	return pending, nil
}

// EvaluateExpression evaluates a SpEL expression against an execution's context.
// This is the closest thing Spinnaker has to a read-only script console: it
// resolves against the live execution, so it answers "what did this stage
// actually see?" rather than what the definition said it would.
//
// When stageID is non-empty the expression is evaluated with that stage as its
// context, which is required for ${#stage(...)} and relative references.
func (c *Client) EvaluateExpression(ctx context.Context, executionID, stageID, expression string) (JSONMap, error) {
	q := url.Values{"expression": {expression}}
	path := "/pipelines/" + escape(executionID) + "/evaluateExpression"
	if stageID != "" {
		path = "/pipelines/" + escape(executionID) + "/" + escape(stageID) + "/evaluateExpression"
	}
	var out JSONMap
	return out, c.get(ctx, path, q, &out)
}

// EvaluateVariables evaluates a list of {key, value} SpEL expressions against an
// execution, optionally as of a given stage. Later entries can reference earlier
// ones, which is how Deck's pipeline-expression evaluator works.
func (c *Client) EvaluateVariables(ctx context.Context, executionID, stageID string, vars []JSONMap) (JSONMap, error) {
	q := url.Values{}
	if stageID != "" {
		q.Set("stageId", stageID)
	}
	var out JSONMap
	return out, c.post(ctx, "/pipelines/"+escape(executionID)+"/evaluateVariables", q, vars, &out)
}

// WaitForExecution polls an execution until it reaches a terminal status, the
// context is canceled, or timeout elapses. onUpdate, when non-nil, is called
// with every fetched execution so a caller can render progress.
func (c *Client) WaitForExecution(ctx context.Context, id string, interval, timeout time.Duration, onUpdate func(JSONMap)) (JSONMap, error) {
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		exec, err := c.GetExecution(ctx, id)
		if err != nil {
			return nil, err
		}
		if onUpdate != nil {
			onUpdate(exec)
		}
		status, _ := exec["status"].(string)
		if IsTerminalStatus(status) {
			return exec, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return exec, fmt.Errorf("timed out after %s waiting for execution %s (last status %s)", timeout, id, status)
		}
		select {
		case <-ctx.Done():
			return exec, ctx.Err()
		case <-ticker.C:
		}
	}
}

// refID strips the "/pipelines/" or "/tasks/" prefix off a Gate task/execution
// reference, which is returned as a path rather than a bare id.
func refID(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func upperAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(strings.TrimSpace(s))
	}
	return out
}
