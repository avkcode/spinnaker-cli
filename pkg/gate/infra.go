package gate

import (
	"context"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Accounts (credentials)
// ---------------------------------------------------------------------------

// ListAccounts returns the configured cloud accounts. expand includes each
// account's full detail (namespaces, regions, permissions), which is a much
// larger response.
func (c *Client) ListAccounts(ctx context.Context, expand bool) (JSONList, error) {
	q := url.Values{"expand": {strconv.FormatBool(expand)}}
	var out JSONList
	return out, c.get(ctx, "/credentials", q, &out)
}

// GetAccount returns one account's detail.
func (c *Client) GetAccount(ctx context.Context, name string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/credentials/"+escape(name), nil, &out)
}

// GetAccountsByType returns accounts of a given provider type (kubernetes, aws…).
func (c *Client) GetAccountsByType(ctx context.Context, accountType string) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/credentials/type/"+escape(accountType), nil, &out)
}

// ---------------------------------------------------------------------------
// Clusters & server groups
// ---------------------------------------------------------------------------

// ListClusters returns an application's clusters grouped by account.
func (c *Client) ListClusters(ctx context.Context, app string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/applications/"+escape(app)+"/clusters", nil, &out)
}

// ListClustersForAccount returns an application's cluster names in one account.
func (c *Client) ListClustersForAccount(ctx context.Context, app, account string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/applications/"+escape(app)+"/clusters/"+escape(account), nil, &out)
}

// GetCluster returns one cluster's detail.
func (c *Client) GetCluster(ctx context.Context, app, account, cluster string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/applications/"+escape(app)+"/clusters/"+escape(account)+"/"+escape(cluster), nil, &out)
}

// GetClusterServerGroups returns a cluster's server groups.
func (c *Client) GetClusterServerGroups(ctx context.Context, app, account, cluster string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/applications/"+escape(app)+"/clusters/"+escape(account)+"/"+escape(cluster)+"/serverGroups", nil, &out)
}

// GetTargetServerGroup resolves a dynamic target such as "current_asg_dynamic",
// "ancestor_asg_dynamic" or "oldest_asg_dynamic" to a concrete server group.
// This is what pipeline expressions resolve when they reference a target.
func (c *Client) GetTargetServerGroup(ctx context.Context, app, account, cluster, cloudProvider, scope, target string) (JSONMap, error) {
	path := "/applications/" + escape(app) + "/clusters/" + escape(account) + "/" + escape(cluster) +
		"/" + escape(cloudProvider) + "/" + escape(scope) + "/serverGroups/target/" + escape(target)
	var out JSONMap
	return out, c.get(ctx, path, nil, &out)
}

// ListServerGroups returns an application's server groups, optionally filtered
// by expand/cloudProvider.
func (c *Client) ListServerGroups(ctx context.Context, app string, expand bool, clusters []string) ([]any, error) {
	q := url.Values{}
	if expand {
		q.Set("expand", "true")
	}
	if len(clusters) > 0 {
		q.Set("clusters", strings.Join(clusters, ","))
	}
	var out []any
	return out, c.get(ctx, "/applications/"+escape(app)+"/serverGroups", q, &out)
}

// GetServerGroup returns one server group's detail.
func (c *Client) GetServerGroup(ctx context.Context, app, account, region, name string) (JSONMap, error) {
	path := "/applications/" + escape(app) + "/serverGroups/" + escape(account) + "/" + escape(region) + "/" + escape(name)
	var out JSONMap
	return out, c.get(ctx, path, nil, &out)
}

// ---------------------------------------------------------------------------
// Load balancers, security groups, instances
// ---------------------------------------------------------------------------

// ListLoadBalancers returns load balancers for a cloud provider.
func (c *Client) ListLoadBalancers(ctx context.Context, cloudProvider string) ([]any, error) {
	q := url.Values{}
	if cloudProvider != "" {
		q.Set("provider", cloudProvider)
	}
	var out []any
	return out, c.get(ctx, "/loadBalancers", q, &out)
}

// ListSecurityGroups returns the cached firewalls/security groups.
func (c *Client) ListSecurityGroups(ctx context.Context) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/securityGroups", nil, &out)
}

// GetInstance returns one instance's detail. On Kubernetes an "instance" is a Pod.
func (c *Client) GetInstance(ctx context.Context, account, region, instanceID string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/instances/"+escape(account)+"/"+escape(region)+"/"+escape(instanceID), nil, &out)
}

// GetInstanceConsole returns an instance's console output. On Kubernetes this is
// the Pod's container log, which makes it the closest Gate equivalent to
// `kubectl logs` for a deployed workload.
func (c *Client) GetInstanceConsole(ctx context.Context, account, region, instanceID, provider string) (JSONMap, error) {
	q := url.Values{}
	if provider != "" {
		q.Set("provider", provider)
	}
	var out JSONMap
	return out, c.get(ctx, "/instances/"+escape(account)+"/"+escape(region)+"/"+escape(instanceID)+"/console", q, &out)
}

// ---------------------------------------------------------------------------
// Manifests
// ---------------------------------------------------------------------------

// GetManifest returns a Kubernetes manifest as clouddriver has it cached,
// including its computed Spinnaker status and events. location is the namespace;
// name is "kind name", e.g. "deployment nginx".
func (c *Client) GetManifest(ctx context.Context, account, location, name string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/manifests/"+escape(account)+"/"+escape(location)+"/"+escape(name), nil, &out)
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// SearchOptions parameterises a clouddriver catalogue search.
type SearchOptions struct {
	Query string
	// Type is a resource type: applications, clusters, serverGroups, instances,
	// loadBalancers, securityGroups, projects.
	Type     string
	Platform string
	PageSize int
	Page     int
	// Allowed filters results to accounts the caller may read.
	Allowed bool
}

// Search queries clouddriver's cached infrastructure index.
//
// Gate's REST endpoint takes a single type per call (a known limitation that
// gate-mcp works around by fanning out), so `sc search` issues one call per
// requested type and merges.
func (c *Client) Search(ctx context.Context, opts SearchOptions) ([]any, error) {
	q := url.Values{}
	if opts.Query != "" {
		q.Set("q", opts.Query)
	}
	if opts.Type != "" {
		q.Set("type", opts.Type)
	}
	if opts.Platform != "" {
		q.Set("platform", opts.Platform)
	}
	if opts.PageSize > 0 {
		q.Set("pageSize", strconv.Itoa(opts.PageSize))
	}
	if opts.Page > 0 {
		q.Set("page", strconv.Itoa(opts.Page))
	}
	var out []any
	return out, c.get(ctx, "/search", q, &out)
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

// ListProjects returns the configured projects.
func (c *Client) ListProjects(ctx context.Context) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/projects", nil, &out)
}

// GetProject returns one project.
func (c *Client) GetProject(ctx context.Context, id string) (JSONMap, error) {
	var out JSONMap
	return out, c.get(ctx, "/projects/"+escape(id), nil, &out)
}

// GetProjectClusters returns a project's cluster rollup.
func (c *Client) GetProjectClusters(ctx context.Context, id string) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/projects/"+escape(id)+"/clusters", nil, &out)
}

// GetProjectPipelines returns recent executions across a project's pipelines.
func (c *Client) GetProjectPipelines(ctx context.Context, id string, limit int) ([]any, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []any
	return out, c.get(ctx, "/projects/"+escape(id)+"/pipelines", q, &out)
}

// ---------------------------------------------------------------------------
// Artifacts
// ---------------------------------------------------------------------------

// ListArtifactAccounts returns the configured artifact accounts.
func (c *Client) ListArtifactAccounts(ctx context.Context) ([]any, error) {
	var out []any
	return out, c.get(ctx, "/artifacts/credentials", nil, &out)
}

// ListArtifactNames returns the artifact names in an artifact account.
func (c *Client) ListArtifactNames(ctx context.Context, accountName, artifactType string) ([]any, error) {
	q := url.Values{}
	if artifactType != "" {
		q.Set("type", artifactType)
	}
	var out []any
	return out, c.get(ctx, "/artifacts/account/"+escape(accountName)+"/names", q, &out)
}

// ListArtifactVersions returns the versions of one artifact.
func (c *Client) ListArtifactVersions(ctx context.Context, accountName, artifactType, artifactName string) ([]any, error) {
	q := url.Values{}
	if artifactType != "" {
		q.Set("type", artifactType)
	}
	if artifactName != "" {
		q.Set("artifactName", artifactName)
	}
	var out []any
	return out, c.get(ctx, "/artifacts/account/"+escape(accountName)+"/versions", q, &out)
}

// FetchArtifact resolves an artifact reference to its contents.
func (c *Client) FetchArtifact(ctx context.Context, artifact JSONMap) ([]byte, error) {
	_, raw, err := c.Raw(ctx, Request{Method: "PUT", Path: "/artifacts/fetch", Body: artifact, Accept: "*/*"})
	return raw, err
}
