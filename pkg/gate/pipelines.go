package gate

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ListPipelineConfigs returns an application's pipeline definitions.
func (c *Client) ListPipelineConfigs(ctx context.Context, app string) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(app)+"/pipelineConfigs", nil, &out)
}

// GetPipelineConfig returns one pipeline definition by name.
func (c *Client) GetPipelineConfig(ctx context.Context, app, name string) (JSONMap, error) {
	var out JSONMap
	err := c.get(ctx, "/applications/"+escape(app)+"/pipelineConfigs/"+escape(name), nil, &out)
	return out, err
}

// ListAllPipelineConfigs returns every pipeline definition on the installation.
func (c *Client) ListAllPipelineConfigs(ctx context.Context) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/pipelineConfigs", nil, &out)
}

// GetPipelineConfigHistory returns prior revisions of a pipeline definition.
func (c *Client) GetPipelineConfigHistory(ctx context.Context, configID string, limit int) (JSONList, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out JSONList
	return out, c.get(ctx, "/pipelineConfigs/"+escape(configID)+"/history", q, &out)
}

// SavePipelineConfig creates or updates a pipeline definition. An "id" in the
// body makes this an update; without one front50 assigns a new id.
//
// Gate returns an empty body here, so success is signaled by the absence of an
// error.
func (c *Client) SavePipelineConfig(ctx context.Context, pipeline JSONMap) error {
	if pipeline["application"] == nil || pipeline["name"] == nil {
		return fmt.Errorf("pipeline must have 'application' and 'name' fields")
	}
	return c.post(ctx, "/pipelines", nil, pipeline, nil)
}

// BulkSavePipelineConfigs saves many pipeline definitions in one call, which is
// markedly faster than a loop over SavePipelineConfig for a GitOps sync.
func (c *Client) BulkSavePipelineConfigs(ctx context.Context, pipelines []JSONMap) (JSONMap, error) {
	var out JSONMap
	return out, c.post(ctx, "/pipelines/bulksave", nil, pipelines, &out)
}

// DeletePipelineConfig deletes a pipeline definition by name.
func (c *Client) DeletePipelineConfig(ctx context.Context, app, name string) error {
	return c.delete(ctx, "/pipelines/"+escape(app)+"/"+escape(name), nil, nil)
}

// RenamePipelineConfig renames a pipeline definition in place, preserving its id
// (and therefore its execution history).
func (c *Client) RenamePipelineConfig(ctx context.Context, app, from, to string) error {
	return c.post(ctx, "/pipelines/move", nil, JSONMap{
		"application": app,
		"from":        from,
		"to":          to,
	}, nil)
}

// SetPipelineConfigDisabled toggles a pipeline definition's disabled flag by
// reading the definition, flipping the field and saving it back. front50 has no
// dedicated enable/disable endpoint.
func (c *Client) SetPipelineConfigDisabled(ctx context.Context, app, name string, disabled bool) error {
	cfg, err := c.GetPipelineConfig(ctx, app, name)
	if err != nil {
		return err
	}
	if len(cfg) == 0 {
		return fmt.Errorf("pipeline %q not found in application %q", name, app)
	}
	cfg["disabled"] = disabled
	return c.SavePipelineConfig(ctx, cfg)
}

// ReorderPipelineConfigs sets the display order of an application's pipelines.
// indexes maps pipeline id to its new index.
func (c *Client) ReorderPipelineConfigs(ctx context.Context, app string, indexes map[string]int) error {
	return c.post(ctx, "/pipelines/reorder", nil, JSONMap{
		"application":    app,
		"idsToIndices":   indexes,
		"isStrategy":     false,
		"applicationKey": app,
	}, nil)
}

// RunPipelineOptions parameterises a pipeline trigger.
type RunPipelineOptions struct {
	// Parameters become the execution's trigger.parameters, readable in SpEL as
	// ${parameters.name}.
	Parameters map[string]any
	// Artifacts are the trigger's expected artifacts.
	Artifacts []JSONMap
	// Type overrides the trigger type (default "manual").
	Type string
	// User overrides the recorded triggering user.
	User string
	// ViaEcho routes the trigger through echo (POST /pipelines/v2/...), which
	// returns an eventId immediately instead of blocking until orca has created
	// the execution. Useful when orca is saturated.
	ViaEcho bool
}

// RunPipeline triggers a pipeline by name or config id and returns the new
// execution's reference.
//
// For the default (non-echo) path Gate returns {"ref": "/pipelines/<id>"}, so
// the caller gets a real execution id. With ViaEcho, Gate returns an echo
// eventId instead and the execution id is not yet known — the returned string is
// then the eventId, which SearchExecutions can resolve.
func (c *Client) RunPipeline(ctx context.Context, app, nameOrID string, opts RunPipelineOptions) (string, error) {
	trigger := JSONMap{"type": opts.Type}
	if opts.Type == "" {
		trigger["type"] = "manual"
	}
	if opts.User != "" {
		trigger["user"] = opts.User
	}
	if len(opts.Parameters) > 0 {
		trigger["parameters"] = opts.Parameters
	}
	if len(opts.Artifacts) > 0 {
		trigger["artifacts"] = opts.Artifacts
	}

	path := "/pipelines/" + escape(app) + "/" + escape(nameOrID)
	if opts.ViaEcho {
		path = "/pipelines/v2/" + escape(app) + "/" + escape(nameOrID)
	}

	var out JSONMap
	if err := c.post(ctx, path, nil, trigger, &out); err != nil {
		return "", err
	}
	if ref, ok := out["ref"].(string); ok && ref != "" {
		return refID(ref), nil
	}
	if id, ok := out["eventId"].(string); ok && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("pipeline %s/%s was triggered but Gate returned no execution reference: %v", app, nameOrID, out)
}

// StartPipeline submits an ad-hoc pipeline definition for immediate execution
// without saving it to front50 first. This is how Deck runs a pipeline that is
// being edited, and the cleanest way to execute generated pipelines.
func (c *Client) StartPipeline(ctx context.Context, pipeline JSONMap) (string, error) {
	var out JSONMap
	if err := c.post(ctx, "/pipelines/start", nil, pipeline, &out); err != nil {
		return "", err
	}
	if id, ok := out["id"].(string); ok {
		return id, nil
	}
	if ref, ok := out["ref"].(string); ok {
		return refID(ref), nil
	}
	return "", fmt.Errorf("pipeline started but Gate returned no execution id: %v", out)
}

// ConvertPipelineConfigToTemplate renders an existing pipeline definition as a
// v2 managed pipeline template.
func (c *Client) ConvertPipelineConfigToTemplate(ctx context.Context, configID string) (string, error) {
	_, raw, err := c.Raw(ctx, Request{
		Method: "GET",
		Path:   "/pipelineConfigs/" + escape(configID) + "/convertToTemplate",
		Accept: "text/plain",
	})
	return string(raw), err
}

// ListStrategyConfigs returns an application's deployment strategy definitions
// (custom strategies are pipelines flagged as strategies).
func (c *Client) ListStrategyConfigs(ctx context.Context, app string) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(app)+"/strategyConfigs", nil, &out)
}

// ListPipelineTemplates returns v2 managed pipeline templates, optionally scoped
// to a set of applications.
func (c *Client) ListPipelineTemplates(ctx context.Context, scopes []string) (JSONList, error) {
	q := url.Values{}
	for _, s := range scopes {
		q.Add("scopes", s)
	}
	var out JSONList
	return out, c.get(ctx, "/v2/pipelineTemplates", q, &out)
}

// GetPipelineTemplate returns one v2 pipeline template.
func (c *Client) GetPipelineTemplate(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/v2/pipelineTemplates/"+escape(id), nil, &out)
}
