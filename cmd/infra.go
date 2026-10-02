package cmd

import (
	"fmt"
	"strings"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------

var accountCmd = &cobra.Command{
	Use:     "account",
	Aliases: []string{"accounts", "credential", "credentials"},
	Short:   "Inspect configured cloud accounts",
	Long: `Inspect the cloud accounts clouddriver is configured with.

An account is a credential plus a scope: for Kubernetes, one cluster and the
namespaces Spinnaker may touch. Everything deployable is addressed through one, so
this is the first thing to check when a deploy stage cannot find its target.`,
	GroupID: GroupCore,
}

var accountListExpand bool

var accountListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List cloud accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		accounts, err := client.ListAccounts(ctx, accountListExpand)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(accounts)
		}
		t := newTable("NAME", "TYPE", "AUTHORIZED", "ENVIRONMENT", "NAMESPACES/REGIONS")
		for _, a := range accounts {
			scope := []string{}
			for _, key := range []string{"namespaces", "regions"} {
				for _, v := range listField(a, key) {
					switch typed := v.(type) {
					case string:
						scope = append(scope, typed)
					case map[string]any:
						scope = append(scope, str(typed, "name"))
					}
				}
			}
			t.add(
				str(a, "name"),
				str(a, "type"),
				fmt.Sprint(boolean(a, "authorized")),
				dash(str(a, "environment")),
				dash(ellipsis(strings.Join(scope, ","), 36)),
			)
		}
		t.print("No accounts configured.")
		return nil
	},
}

var accountGetCmd = &cobra.Command{
	Use:   "get [account]",
	Short: "Show one account's detail",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		account, err := client.GetAccount(ctx, args[0])
		if err != nil {
			return err
		}
		if len(account) == 0 {
			return fmt.Errorf("account %q not found", args[0])
		}
		return render(account)
	},
	ValidArgsFunction: completeAccounts,
}

// completeAccounts completes configured account names.
func completeAccounts(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	client, err := getGate()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := cmdContext(cmd.Context())
	defer cancel()
	accounts, err := client.ListAccounts(ctx, false)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := []string{}
	for _, a := range accounts {
		if n := str(a, "name"); strings.HasPrefix(n, toComplete) {
			names = append(names, n)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// ---------------------------------------------------------------------------
// clusters and server groups
// ---------------------------------------------------------------------------

var clusterCmd = &cobra.Command{
	Use:     "cluster",
	Aliases: []string{"clusters"},
	Short:   "Inspect clusters and server groups",
	Long: `Inspect an application's clusters and server groups.

Spinnaker's model is provider-neutral: a cluster is a logical service and a server
group is one versioned deployment of it. On Kubernetes a server group is a
ReplicaSet (or the workload controller behind one), so 'sc cluster get' shows the
versions of a service currently deployed and which one is taking traffic.`,
	GroupID: GroupCore,
}

var clusterListCmd = &cobra.Command{
	Use:     "list [application]",
	Aliases: []string{"ls"},
	Short:   "List an application's clusters by account",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		clusters, err := client.ListClusters(ctx, args[0])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(clusters)
		}
		t := newTable("ACCOUNT", "CLUSTER")
		for _, account := range sortedKeys(clusters) {
			for _, c := range listField(clusters, account) {
				switch typed := c.(type) {
				case string:
					t.add(account, typed)
				case map[string]any:
					t.add(account, str(typed, "name"))
				}
			}
		}
		t.print("No clusters cached for this application.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var clusterGetCmd = &cobra.Command{
	Use:   "get [application] [account] [cluster]",
	Short: "Show a cluster's server groups",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		cluster, err := client.GetCluster(ctx, args[0], args[1], args[2])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(cluster)
		}
		groups := mapList(listField(cluster, "serverGroups"))
		t := newTable("SERVER GROUP", "REGION", "INSTANCES", "UP", "DOWN", "DISABLED", "CREATED")
		for _, g := range groups {
			up, down := instanceHealth(g)
			t.add(
				str(g, "name"),
				dash(strOr(g, "region", "namespace")),
				fmt.Sprint(len(listField(g, "instances"))),
				fmt.Sprint(up),
				fmt.Sprint(down),
				fmt.Sprint(boolean(g, "disabled")),
				epochTime(g, "createdTime"),
			)
		}
		t.print("This cluster has no server groups.")
		return nil
	},
}

// instanceHealth counts a server group's healthy and unhealthy instances.
func instanceHealth(group map[string]any) (up, down int) {
	for _, inst := range mapList(listField(group, "instances")) {
		switch strings.ToUpper(str(inst, "healthState")) {
		case "UP":
			up++
		case "DOWN", "OUTOFSERVICE", "FAILED":
			down++
		}
	}
	return up, down
}

var serverGroupCmd = &cobra.Command{
	Use:     "servergroup",
	Aliases: []string{"sg", "servergroups"},
	Short:   "Inspect server groups",
	GroupID: GroupCore,
}

var serverGroupListExpand bool

var serverGroupListCmd = &cobra.Command{
	Use:     "list [application]",
	Aliases: []string{"ls"},
	Short:   "List an application's server groups",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		groups, err := client.ListServerGroups(ctx, args[0], serverGroupListExpand, nil)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(groups)
		}
		t := newTable("NAME", "ACCOUNT", "REGION", "CLUSTER", "INSTANCES", "DISABLED", "CREATED")
		for _, g := range mapList(groups) {
			t.add(
				str(g, "name"),
				dash(str(g, "account")),
				dash(strOr(g, "region", "namespace")),
				dash(str(g, "cluster")),
				fmt.Sprint(len(listField(g, "instances"))),
				fmt.Sprint(boolean(g, "disabled")),
				epochTime(g, "createdTime"),
			)
		}
		t.print("No server groups cached for this application.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var serverGroupGetCmd = &cobra.Command{
	Use:   "get [application] [account] [region] [name]",
	Short: "Show one server group's detail",
	Args:  cobra.ExactArgs(4),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		group, err := client.GetServerGroup(ctx, args[0], args[1], args[2], args[3])
		if err != nil {
			return err
		}
		return render(group)
	},
}

// ---------------------------------------------------------------------------
// manifests
// ---------------------------------------------------------------------------

var manifestCmd = &cobra.Command{
	Use:     "manifest",
	Aliases: []string{"manifests"},
	Short:   "Inspect Kubernetes manifests as Spinnaker sees them",
	GroupID: GroupCore,
}

var manifestGetCmd = &cobra.Command{
	Use:   "get [account] [namespace] [kind name]",
	Short: "Show a Kubernetes manifest from clouddriver's cache",
	Long: `Shows a Kubernetes manifest as clouddriver has it cached, together with the
status Spinnaker computed for it and the events it observed.

The computed status is the interesting part: it is exactly what a "Wait for
manifest to stabilize" stage is waiting on, so this answers why such a stage has
not completed.

The resource is addressed the way Spinnaker names it: "kind name", e.g.
"deployment nginx".`,
	Args: cobra.ExactArgs(3),
	Example: `  sc manifest get managing spinnaker "deployment orca"
  sc manifest get managing default "service nginx" -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		manifest, err := client.GetManifest(ctx, args[0], args[1], args[2])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(manifest)
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintf(tw, "Name:\t%s\n", str(manifest, "name"))
		fmt.Fprintf(tw, "Account:\t%s\n", str(manifest, "account"))
		fmt.Fprintf(tw, "Location:\t%s\n", str(manifest, "location"))
		if status := mapField(manifest, "status"); status != nil {
			for _, key := range sortedKeys(status) {
				cond := mapField(status, key)
				state := fmt.Sprint(status[key])
				if cond != nil {
					state = fmt.Sprintf("%v", boolean(cond, "state"))
					if msg := str(cond, "message"); msg != "" {
						state += " (" + msg + ")"
					}
				}
				fmt.Fprintf(tw, "Status %s:\t%s\n", key, state)
			}
		}
		w.FlushTable(tw)
		if events := mapList(listField(manifest, "events")); len(events) > 0 {
			fmt.Println()
			et := newTable("REASON", "MESSAGE", "COUNT")
			for _, e := range events {
				et.add(dash(str(e, "reason")), ellipsis(str(e, "message"), 70), dash(str(e, "count")))
			}
			et.print("")
		}
		return nil
	},
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return completeAccounts(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
}

// ---------------------------------------------------------------------------
// search
// ---------------------------------------------------------------------------

var (
	searchTypes    []string
	searchPlatform string
	searchPageSize int
)

var searchCmd = &cobra.Command{
	Use:   "search [query]",
	Short: "Search clouddriver's infrastructure cache",
	Long: `Searches clouddriver's cached infrastructure index.

Gate's search endpoint accepts a single type per call, so searching several types
means several calls; this command fans out and merges, giving each type its own
result budget — the behaviour of Deck's global search bar.`,
	Args:    cobra.ExactArgs(1),
	GroupID: GroupCore,
	Example: `  sc search nginx
  sc search demo --type applications --type serverGroups
  sc search orca --type instances --page-size 50`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		types := searchTypes
		if len(types) == 0 {
			types = []string{"applications", "serverGroups", "clusters", "loadBalancers", "instances", "securityGroups"}
		}

		type hit struct {
			Type     string `json:"type" yaml:"type"`
			Name     string `json:"name" yaml:"name"`
			Account  string `json:"account,omitempty" yaml:"account,omitempty"`
			Region   string `json:"region,omitempty" yaml:"region,omitempty"`
			Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
		}
		hits := []hit{}
		var firstErr error
		for _, ty := range types {
			results, err := client.Search(ctx, gate.SearchOptions{
				Query:    args[0],
				Type:     ty,
				Platform: searchPlatform,
				PageSize: searchPageSize,
			})
			if err != nil {
				// A provider that is not configured 404s for its types; that is not
				// a reason to fail the whole search.
				if firstErr == nil {
					firstErr = err
				}
				debugf("search type %s failed: %v", ty, err)
				continue
			}
			// clouddriver wraps each type's hits in a result envelope.
			for _, envelope := range mapList(results) {
				for _, r := range mapList(listField(envelope, "results")) {
					hits = append(hits, hit{
						Type:     dash(strOr(r, "type", "provider")),
						Name:     strOr(r, "serverGroup", "cluster", "loadBalancer", "application", "instanceId", "name", "id"),
						Account:  str(r, "account"),
						Region:   strOr(r, "region", "namespace"),
						Provider: str(r, "provider"),
					})
				}
			}
		}
		if len(hits) == 0 && firstErr != nil {
			return firstErr
		}
		if outputIsStructured() {
			return render(hits)
		}
		t := newTable("TYPE", "NAME", "ACCOUNT", "REGION", "PROVIDER")
		for _, h := range hits {
			t.add(h.Type, h.Name, dash(h.Account), dash(h.Region), dash(h.Provider))
		}
		t.print("Nothing matched. The index is clouddriver's cache, so very recent resources may not appear yet.")
		return nil
	},
}

// ---------------------------------------------------------------------------
// projects
// ---------------------------------------------------------------------------

var projectCmd = &cobra.Command{
	Use:     "project",
	Aliases: []string{"projects"},
	Short:   "Inspect projects",
	Long:    "Projects group applications and clusters into one dashboard. The archived spin CLI never supported them.",
	GroupID: GroupCore,
}

var projectListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List projects",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		projects, err := client.ListProjects(ctx)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(projects)
		}
		t := newTable("NAME", "ID", "OWNER", "APPLICATIONS", "UPDATED")
		for _, p := range projects {
			cfg := mapField(p, "config")
			t.add(
				str(p, "name"),
				str(p, "id"),
				dash(str(p, "email")),
				fmt.Sprint(len(listField(cfg, "applications"))),
				epochTime(p, "updateTs"),
			)
		}
		t.print("No projects configured.")
		return nil
	},
}

var projectGetCmd = &cobra.Command{
	Use:   "get [project]",
	Short: "Show one project",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		project, err := client.GetProject(ctx, args[0])
		if err != nil {
			return err
		}
		return render(project)
	},
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

var artifactCmd = &cobra.Command{
	Use:     "artifact",
	Aliases: []string{"artifacts"},
	Short:   "Inspect artifact accounts and versions",
	GroupID: GroupCore,
}

var artifactAccountsCmd = &cobra.Command{
	Use:     "accounts",
	Aliases: []string{"credentials"},
	Short:   "List artifact accounts",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		accounts, err := client.ListArtifactAccounts(ctx)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(accounts)
		}
		t := newTable("NAME", "TYPES")
		for _, a := range mapList(accounts) {
			types := []string{}
			for _, ty := range listField(a, "types") {
				types = append(types, fmt.Sprint(ty))
			}
			t.add(str(a, "name"), dash(strings.Join(types, ",")))
		}
		t.print("No artifact accounts configured.")
		return nil
	},
}

var artifactNamesType string

var artifactNamesCmd = &cobra.Command{
	Use:   "names [artifact-account]",
	Short: "List artifact names in an account",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		names, err := client.ListArtifactNames(ctx, args[0], artifactNamesType)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(names)
		}
		for _, n := range names {
			fmt.Println(n)
		}
		return nil
	},
}

var artifactVersionsType string

var artifactVersionsCmd = &cobra.Command{
	Use:   "versions [artifact-account] [artifact-name]",
	Short: "List an artifact's versions",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		versions, err := client.ListArtifactVersions(ctx, args[0], artifactVersionsType, args[1])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(versions)
		}
		for _, v := range versions {
			fmt.Println(v)
		}
		return nil
	},
}

func init() {
	accountListCmd.Flags().BoolVar(&accountListExpand, "expand", false, "include each account's full detail")
	accountCmd.AddCommand(accountListCmd, accountGetCmd)

	clusterCmd.AddCommand(clusterListCmd, clusterGetCmd)

	serverGroupListCmd.Flags().BoolVar(&serverGroupListExpand, "expand", false, "include instance detail")
	serverGroupCmd.AddCommand(serverGroupListCmd, serverGroupGetCmd)

	manifestCmd.AddCommand(manifestGetCmd)

	searchCmd.Flags().StringArrayVar(&searchTypes, "type", nil, "resource type to search (repeatable): applications, clusters, serverGroups, instances, loadBalancers, securityGroups, projects")
	searchCmd.Flags().StringVar(&searchPlatform, "platform", "", "restrict to a cloud provider")
	searchCmd.Flags().IntVar(&searchPageSize, "page-size", 25, "results per type")

	projectCmd.AddCommand(projectListCmd, projectGetCmd)

	artifactNamesCmd.Flags().StringVar(&artifactNamesType, "type", "", "artifact type filter")
	artifactVersionsCmd.Flags().StringVar(&artifactVersionsType, "type", "", "artifact type filter")
	artifactCmd.AddCommand(artifactAccountsCmd, artifactNamesCmd, artifactVersionsCmd)

	rootCmd.AddCommand(accountCmd, clusterCmd, serverGroupCmd, manifestCmd, searchCmd, projectCmd, artifactCmd)
}
