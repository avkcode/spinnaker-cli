package gate

import (
	"context"
	"net/url"
	"strings"
)

// Version returns the installation's Spinnaker version.
func (c *Client) Version(ctx context.Context) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/version", nil, &out)
}

// Health returns Gate's own actuator health document. Gate aggregates nothing
// here — it reports only its own status, so a healthy Gate does not imply a
// healthy installation. Use the svc package for per-service health.
func (c *Client) Health(ctx context.Context) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/health", nil, &out)
}

// AuthUser returns the authenticated user, including the Fiat roles and allowed
// accounts that every authorization decision is made against. This is the
// fastest way to tell whether credentials work and what they can reach.
func (c *Client) AuthUser(ctx context.Context) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/auth/user", nil, &out)
}

// LoggedOut reports whether the session is no longer authenticated.
func (c *Client) RawUser(ctx context.Context) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/auth/rawUser", nil, &out)
}

// ListServiceAccounts returns the Fiat service accounts the caller may use.
func (c *Client) ListServiceAccounts(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/auth/user/serviceAccounts", nil, &out)
}

// ListRoles returns the roles known for a provider.
func (c *Client) ListRoles(ctx context.Context, provider string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/roles/"+escape(provider), nil, &out)
}

// SyncRoles forces Fiat to re-resolve permissions. Required after an external
// group membership change, which Fiat otherwise picks up only on its own cycle.
func (c *Client) SyncRoles(ctx context.Context, roles []string) error {
	var body any
	if len(roles) > 0 {
		body = roles
	}
	return c.post(ctx, "/roles/sync", nil, body, nil)
}

// Capabilities returns the installation's advertised capabilities: which
// deployment monitors exist, and which SpEL expression helpers are available.
func (c *Client) Capabilities(ctx context.Context) (JSONMap, error) {
	out := JSONMap{}
	var monitors []any
	if err := c.get(ctx, "/capabilities/deploymentMonitors", nil, &monitors); err == nil {
		out["deploymentMonitors"] = monitors
	}
	var expressions any
	if err := c.get(ctx, "/capabilities/expressions", nil, &expressions); err == nil {
		out["expressions"] = expressions
	}
	var quietPeriod any
	if err := c.get(ctx, "/capabilities/quietPeriod", nil, &quietPeriod); err == nil {
		out["quietPeriod"] = quietPeriod
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Notifications, triggers, webhooks
// ---------------------------------------------------------------------------

// GetNotifications returns the notification config for an entity, e.g.
// scope "application", name "myapp".
func (c *Client) GetNotifications(ctx context.Context, scope, name string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/notifications/"+escape(scope)+"/"+escape(name), nil, &out)
}

// ListCronTriggers returns echo's scheduled (cron) pipeline triggers.
func (c *Client) ListCronTriggers(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/cron", nil, &out)
}

// ListWebhookTypes returns the preconfigured webhook stage types.
func (c *Client) ListWebhookTypes(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/webhooks/preconfigured", nil, &out)
}

// SendWebhook posts a webhook event, which echo matches against pipeline
// triggers of type "webhook".
func (c *Client) SendWebhook(ctx context.Context, webhookType, source string, payload JSONMap) (JSONMap, error) {
	var out JSONMap
	return out, c.post(ctx, "/webhooks/"+escape(webhookType)+"/"+escape(source), nil, payload, &out)
}

// ListPubsubSubscriptions returns the configured pubsub subscriptions.
func (c *Client) ListPubsubSubscriptions(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/pubsub/subscriptions", nil, &out)
}

// ---------------------------------------------------------------------------
// CI (igor)
// ---------------------------------------------------------------------------

// ListBuildMasters returns the configured CI masters (Jenkins, Travis,
// Concourse, GitLab CI, …).
func (c *Client) ListBuildMasters(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/v3/builds", nil, &out)
}

// ListBuildJobs returns the jobs on a CI master.
func (c *Client) ListBuildJobs(ctx context.Context, buildMaster string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/v2/builds/"+escape(buildMaster)+"/jobs", nil, &out)
}

// ListBuilds returns the builds of a job on a CI master.
func (c *Client) ListBuilds(ctx context.Context, buildMaster, job string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/v2/builds/"+escape(buildMaster)+"/builds/"+strings.TrimLeft(job, "/"), nil, &out)
}

// GetBuild returns one build of a job on a CI master.
func (c *Client) GetBuild(ctx context.Context, buildMaster, job, number string) (JSONMap, error) {
	var out JSONMap
	path := "/v2/builds/" + escape(buildMaster) + "/build/" + escape(number) + "/" + strings.TrimLeft(job, "/")
	return out, c.get(ctx, path, nil, &out)
}

// ListCIBuilds returns igor's normalized CI build view for a project/repo.
func (c *Client) ListCIBuilds(ctx context.Context, projectKey, repoSlug string, opts url.Values) ([]any, error) {
	q := url.Values{}
	for k, v := range opts {
		q[k] = v
	}
	if projectKey != "" {
		q.Set("projectKey", projectKey)
	}
	if repoSlug != "" {
		q.Set("repoSlug", repoSlug)
	}
	var out []any
	return out, c.get(ctx, "/ci/builds", q, &out)
}

// ---------------------------------------------------------------------------
// Bakery (rosco)
// ---------------------------------------------------------------------------

// ListBakeOptions returns the base images rosco can bake, per cloud provider.
func (c *Client) ListBakeOptions(ctx context.Context, cloudProvider string) (any, error) {
	path := "/bakery/options"
	if cloudProvider != "" {
		path += "/" + escape(cloudProvider)
	}
	var out any
	return out, c.get(ctx, path, nil, &out)
}

// GetBakeLogs returns the Packer log of a bake.
func (c *Client) GetBakeLogs(ctx context.Context, region, statusID string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/bakery/logs/"+escape(region)+"/"+escape(statusID), nil, &out)
}

// ---------------------------------------------------------------------------
// Canary (kayenta)
// ---------------------------------------------------------------------------

// ListCanaryConfigs returns the v2 canary configs, optionally for one application.
func (c *Client) ListCanaryConfigs(ctx context.Context, app string) ([]any, error) {
	q := url.Values{}
	if app != "" {
		q.Set("application", app)
	}
	var out []any
	return out, c.get(ctx, "/v2/canaryConfig", q, &out)
}

// GetCanaryConfig returns one canary config.
func (c *Client) GetCanaryConfig(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/v2/canaryConfig/"+escape(id), nil, &out)
}

// ListCanaryJudges returns the configured canary judges.
func (c *Client) ListCanaryJudges(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/v2/canaries/judges", nil, &out)
}

// GetCanaryResult returns a canary judgment result.
func (c *Client) GetCanaryResult(ctx context.Context, canaryExecutionID string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/v2/canaries/canary/"+escape(canaryExecutionID), nil, &out)
}

// ---------------------------------------------------------------------------
// Managed delivery (keel)
// ---------------------------------------------------------------------------

// GetManagedApplication returns an application's managed-delivery view:
// environments, resources and their status.
func (c *Client) GetManagedApplication(ctx context.Context, app string, includeDetails bool) (JSONMap, error) {
	q := url.Values{}
	if includeDetails {
		q.Set("entities", "resources,artifacts,environments")
	}
	var out JSONMap
	return out, c.get(ctx, "/managed/application/"+escape(app), q, &out)
}

// GetDeliveryConfig returns a delivery config manifest by name.
func (c *Client) GetDeliveryConfig(ctx context.Context, name string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/managed/delivery-configs/"+escape(name), nil, &out)
}

// GetManagedResource returns one managed-delivery resource.
func (c *Client) GetManagedResource(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/managed/resources/"+escape(id), nil, &out)
}
