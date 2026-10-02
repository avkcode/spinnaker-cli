package gate

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TaskRequest is an ad-hoc orca task: a list of clouddriver operations ("jobs")
// submitted together. Almost every imperative Spinnaker action that is not a
// pipeline — create an application, deploy a manifest, resize a server group,
// delete a load balancer — is one of these.
type TaskRequest struct {
	Application string    `json:"application,omitempty"`
	Description string    `json:"description,omitempty"`
	Job         []JSONMap `json:"job"`
}

// CreateTask submits an ad-hoc task and returns its id.
func (c *Client) CreateTask(ctx context.Context, req TaskRequest) (string, error) {
	if len(req.Job) == 0 {
		return "", fmt.Errorf("task requires at least one job")
	}
	var out JSONMap
	if err := c.post(ctx, "/tasks", nil, req, &out); err != nil {
		return "", err
	}
	// Gate answers with {"ref": "/tasks/<id>"}.
	if ref, ok := out["ref"].(string); ok && ref != "" {
		return refID(ref), nil
	}
	if id, ok := out["id"].(string); ok && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("task was submitted but Gate returned no reference: %v", out)
}

// GetTask returns a task by id.
func (c *Client) GetTask(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/tasks/"+escape(id), nil, &out)
}

// CancelTask cancels a running task.
func (c *Client) CancelTask(ctx context.Context, id string) error {
	return c.put(ctx, "/tasks/"+escape(id)+"/cancel", nil, nil, nil)
}

// CancelTasks cancels several tasks in one call.
func (c *Client) CancelTasks(ctx context.Context, ids []string) error {
	q := url.Values{"ids": {strings.Join(ids, ",")}}
	return c.put(ctx, "/tasks/cancel", q, nil, nil)
}

// DeleteTask removes a task record.
func (c *Client) DeleteTask(ctx context.Context, id string) error {
	return c.delete(ctx, "/tasks/"+escape(id), nil, nil)
}

// GetTaskDetails returns a task's provider-specific detail payload (e.g. the
// Kubernetes object a deploy stage produced).
func (c *Client) GetTaskDetails(ctx context.Context, id, detailsID string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/tasks/"+escape(id)+"/details/"+escape(detailsID), nil, &out)
}

// ListApplicationTasks returns an application's ad-hoc task history.
func (c *Client) ListApplicationTasks(ctx context.Context, app string, page, limit int, statuses []string) (JSONList, error) {
	q := url.Values{}
	if page > 0 {
		q.Set("page", fmt.Sprint(page))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if len(statuses) > 0 {
		q.Set("statuses", strings.Join(upperAll(statuses), ","))
	}
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(app)+"/tasks", q, &out)
}

// TaskStatus extracts a task's status, which orca reports as a top-level
// "status" field mirroring the execution status vocabulary.
func TaskStatus(task JSONMap) string {
	if s, ok := task["status"].(string); ok {
		return s
	}
	return ""
}

// TaskFailureMessage digs the human-readable failure out of a failed task. orca
// buries it in the last failed stage's context, under one of several keys
// depending on which operation failed.
func TaskFailureMessage(task JSONMap) string {
	variables, _ := task["variables"].([]any)
	for _, v := range variables {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if key, _ := m["key"].(string); key == "exception" {
			if msg := extractException(m["value"]); msg != "" {
				return msg
			}
		}
	}
	stages, _ := task["steps"].([]any)
	for i := len(stages) - 1; i >= 0; i-- {
		stage, ok := stages[i].(map[string]any)
		if !ok {
			continue
		}
		if status, _ := stage["status"].(string); !IsFailureStatus(status) {
			continue
		}
		if sctx, ok := stage["context"].(map[string]any); ok {
			if msg := extractException(sctx["exception"]); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// extractException pulls the message out of orca's nested exception shape:
// {"details": {"errors": ["…"], "error": "…"}}.
func extractException(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	details, ok := m["details"].(map[string]any)
	if !ok {
		return ""
	}
	if errs, ok := details["errors"].([]any); ok && len(errs) > 0 {
		parts := make([]string, 0, len(errs))
		for _, e := range errs {
			parts = append(parts, fmt.Sprint(e))
		}
		return strings.Join(parts, "; ")
	}
	if e, ok := details["error"].(string); ok {
		return e
	}
	return ""
}

// WaitForTask polls a task until it reaches a terminal status. It returns the
// final task, and an error when the task failed.
func (c *Client) WaitForTask(ctx context.Context, id string, interval, timeout time.Duration) (JSONMap, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		task, err := c.GetTask(ctx, id)
		if err != nil {
			return nil, err
		}
		status := TaskStatus(task)
		if IsTerminalStatus(status) {
			if IsFailureStatus(status) {
				if msg := TaskFailureMessage(task); msg != "" {
					return task, fmt.Errorf("task %s %s: %s", id, status, msg)
				}
				return task, fmt.Errorf("task %s %s", id, status)
			}
			return task, nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return task, fmt.Errorf("timed out after %s waiting for task %s (last status %s)", timeout, id, status)
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}
