package gate

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// ListApplicationsOptions filters GET /applications.
type ListApplicationsOptions struct {
	// Account restricts results to applications deployed in this account.
	Account string
	// Owner restricts results to applications with this owner email.
	Owner string
}

// ListApplications returns every application the caller may read.
//
// Note that the list mixes two sources: applications with front50 metadata
// (created in Spinnaker) and applications inferred by clouddriver purely from
// cached infrastructure. The latter carry an "accounts" field but no "email" or
// "createTs", which is how `sc app list` tells them apart.
func (c *Client) ListApplications(ctx context.Context, opts ListApplicationsOptions) (JSONList, error) {
	q := url.Values{}
	if opts.Account != "" {
		q.Set("account", opts.Account)
	}
	if opts.Owner != "" {
		q.Set("owner", opts.Owner)
	}
	var out JSONList
	return out, c.get(ctx, "/applications", q, &out)
}

// GetApplication returns one application's details. expand includes the
// clusters/instance counts clouddriver knows about, which is slower.
func (c *Client) GetApplication(ctx context.Context, name string, expand bool) (JSONMap, error) {
	q := url.Values{"expand": {strconv.FormatBool(expand)}}
	var out JSONMap
	return out, c.get(ctx, "/applications/"+escape(name), q, &out)
}

// GetApplicationHistory returns front50's revision history for an application.
func (c *Client) GetApplicationHistory(ctx context.Context, name string, limit int) (JSONList, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(name)+"/history", q, &out)
}

// Application is the subset of front50's application model that Spinnaker
// requires when creating one.
type Application struct {
	Name           string `json:"name"`
	Email          string `json:"email"`
	Description    string `json:"description,omitempty"`
	CloudProviders string `json:"cloudProviders,omitempty"`
	// PlatformHealthOnly and its "show override" sibling are the two fields
	// most often needed on a Kubernetes-only install.
	PlatformHealthOnly             bool     `json:"platformHealthOnly,omitempty"`
	PlatformHealthOnlyShowOverride bool     `json:"platformHealthOnlyShowOverride,omitempty"`
	User                           string   `json:"user,omitempty"`
	Accounts                       string   `json:"accounts,omitempty"`
	TrafficGuards                  []any    `json:"trafficGuards,omitempty"`
	Permissions                    JSONMap  `json:"permissions,omitempty"`
	RepoProjectKey                 string   `json:"repoProjectKey,omitempty"`
	RepoSlug                       string   `json:"repoSlug,omitempty"`
	RepoType                       string   `json:"repoType,omitempty"`
	InstancePort                   int      `json:"instancePort,omitempty"`
	ProviderSettings               JSONMap  `json:"providerSettings,omitempty"`
	DataSources                    JSONMap  `json:"dataSources,omitempty"`
	Aliases                        string   `json:"aliases,omitempty"`
	Owners                         []string `json:"-"`
}

// SaveApplication creates or updates an application.
//
// There is no REST "create application" endpoint: application writes go through
// orca as an upsertApplication task, the same path Deck uses, so that front50
// writes, permission propagation and the event stream all behave identically.
// The returned task reference can be polled with WaitForTask.
func (c *Client) SaveApplication(ctx context.Context, app Application, user string) (string, error) {
	if app.Name == "" {
		return "", fmt.Errorf("application name is required")
	}
	if user == "" {
		user = app.User
	}
	job := JSONMap{
		"type":        "upsertApplication",
		"application": app,
		"user":        user,
	}
	return c.CreateTask(ctx, TaskRequest{
		Application: app.Name,
		Description: "Create Application: " + app.Name,
		Job:         []JSONMap{job},
	})
}

// DeleteApplication deletes an application's front50 metadata. It does not
// delete deployed infrastructure.
func (c *Client) DeleteApplication(ctx context.Context, name, user string) (string, error) {
	job := JSONMap{
		"type":        "deleteApplication",
		"application": JSONMap{"name": name},
		"user":        user,
	}
	return c.CreateTask(ctx, TaskRequest{
		Application: name,
		Description: "Deleting Application: " + name,
		Job:         []JSONMap{job},
	})
}

// GetApplicationRawResources lists the Kubernetes resources clouddriver has
// cached for an application that are not modeled as server groups or load
// balancers (ConfigMaps, Secrets, CRDs, …).
func (c *Client) GetApplicationRawResources(ctx context.Context, app string) (JSONList, error) {
	var out JSONList
	return out, c.get(ctx, "/applications/"+escape(app)+"/rawResources", nil, &out)
}
