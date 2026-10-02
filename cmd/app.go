package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

var appCmd = &cobra.Command{
	Use:     "app",
	Aliases: []string{"application", "apps"},
	Short:   "Manage applications",
	Long: `Applications are Spinnaker's top-level grouping: pipelines, clusters and
permissions all hang off one.

An installation shows two kinds. Registered applications have front50 metadata
(an owner email, a creation time) because someone created them in Spinnaker.
Inferred applications are synthesized by clouddriver purely from cached
infrastructure — anything it found running under a Spinnaker-style name. Only
registered applications can own pipelines, which is why 'sc app list' separates
them.`,
	GroupID: GroupCore,
}

var (
	appListAccount    string
	appListOwner      string
	appListRegistered bool
	appListInferred   bool
)

var appListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List applications",
	Example: `  sc app list
  sc app list --registered
  sc app list --account managing
  sc app list --owner me@example.com -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		apps, err := client.ListApplications(ctx, gate.ListApplicationsOptions{
			Account: appListAccount,
			Owner:   appListOwner,
		})
		if err != nil {
			return err
		}

		// front50 records an email and createTs; clouddriver-inferred entries have
		// neither, carrying only the accounts they were seen in.
		filtered := make([]map[string]any, 0, len(apps))
		for _, a := range apps {
			registered := str(a, "email") != "" || str(a, "createTs") != ""
			if appListRegistered && !registered {
				continue
			}
			if appListInferred && registered {
				continue
			}
			filtered = append(filtered, a)
		}

		if outputIsStructured() {
			return render(filtered)
		}

		t := newTable("NAME", "OWNER", "ACCOUNTS", "PROVIDERS", "SOURCE", "UPDATED")
		for _, a := range filtered {
			source := "inferred"
			if str(a, "email") != "" || str(a, "createTs") != "" {
				source = "registered"
			}
			t.add(
				str(a, "name"),
				dash(str(a, "email")),
				dash(ellipsis(str(a, "accounts"), 30)),
				dash(str(a, "cloudProviders")),
				source,
				epochTime(a, "updateTs"),
			)
		}
		t.print("No applications found.")
		return nil
	},
}

var appGetExpand bool

var appGetCmd = &cobra.Command{
	Use:   "get [application]",
	Short: "Show an application's details",
	Args:  cobra.ExactArgs(1),
	Example: `  sc app get demo
  sc app get demo -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		app, err := client.GetApplication(ctx, args[0], appGetExpand)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(app)
		}

		attrs := mapField(app, "attributes")
		if attrs == nil {
			attrs = app
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintf(tw, "Name:\t%s\n", str(attrs, "name"))
		fmt.Fprintf(tw, "Owner:\t%s\n", dash(str(attrs, "email")))
		fmt.Fprintf(tw, "Description:\t%s\n", dash(str(attrs, "description")))
		fmt.Fprintf(tw, "Cloud providers:\t%s\n", dash(str(attrs, "cloudProviders")))
		fmt.Fprintf(tw, "Accounts:\t%s\n", dash(str(attrs, "accounts")))
		fmt.Fprintf(tw, "Created:\t%s\n", epochTime(attrs, "createTs"))
		fmt.Fprintf(tw, "Updated:\t%s\n", epochTime(attrs, "updateTs"))
		fmt.Fprintf(tw, "Last modified by:\t%s\n", dash(str(attrs, "lastModifiedBy")))
		w.FlushTable(tw)

		// With expand=true clouddriver attaches the application's clusters.
		if clusters := mapField(app, "clusters"); len(clusters) > 0 {
			fmt.Println()
			ct := newTable("ACCOUNT", "CLUSTER", "SERVER GROUPS")
			for _, account := range sortedKeys(clusters) {
				for _, c := range mapList(listField(clusters, account)) {
					ct.add(account, str(c, "name"), fmt.Sprint(len(listField(c, "serverGroups"))))
				}
			}
			ct.print("")
		}
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var (
	appCreateEmail       string
	appCreateDescription string
	appCreateProviders   string
	appCreateFile        string
	appCreateWait        bool
)

var appCreateCmd = &cobra.Command{
	Use:     "create [application]",
	Aliases: []string{"save", "apply"},
	Short:   "Create or update an application",
	Long: `Creates or updates an application.

Spinnaker has no REST "create application" endpoint: application writes are
submitted to orca as an upsertApplication task, exactly as Deck does, so that
front50 persistence, Fiat permission propagation and the event stream all behave
identically. --wait blocks until that task completes.`,
	Args: cobra.MaximumNArgs(1),
	Example: `  sc app create demo --email me@example.com --cloud-providers kubernetes
  sc app create demo --email me@example.com --wait
  sc app create -f app.json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		var app gate.Application
		if appCreateFile != "" {
			raw, err := readJSONOrYAML(appCreateFile)
			if err != nil {
				return err
			}
			app.Name = str(raw, "name")
			app.Email = str(raw, "email")
			app.Description = str(raw, "description")
			app.CloudProviders = str(raw, "cloudProviders")
			app.Permissions = mapField(raw, "permissions")
		}
		if len(args) == 1 {
			app.Name = args[0]
		}
		if appCreateEmail != "" {
			app.Email = appCreateEmail
		}
		if appCreateDescription != "" {
			app.Description = appCreateDescription
		}
		if appCreateProviders != "" {
			app.CloudProviders = appCreateProviders
		}
		if app.Name == "" {
			return fmt.Errorf("application name is required (as an argument or 'name' in --file)")
		}
		if app.Email == "" {
			return fmt.Errorf("--email is required: front50 rejects an application without an owner")
		}

		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would create/update application %s (owner %s, providers %s)", app.Name, app.Email, dash(app.CloudProviders))
			return nil
		}
		// The submit is bounded by --timeout; the waits that follow are not, since
		// they legitimately outlast a single request.
		ctx := cmd.Context()
		submitCtx, cancel := cmdContext(ctx)
		taskID, err := client.SaveApplication(submitCtx, app, gateUser)
		cancel()
		if err != nil {
			return err
		}
		audit("app.create", app.Name)

		if !appCreateWait {
			fmt.Fprintf(os.Stderr, "Submitted task %s to create application %s\n", taskID, app.Name)
			if outputIsStructured() {
				return render(map[string]any{"application": app.Name, "task": taskID})
			}
			fmt.Println(taskID)
			return nil
		}
		task, err := client.WaitForTask(ctx, taskID, 0, 5*time.Minute)
		if err != nil {
			return err
		}
		// The task completing does not mean the application is readable yet: Gate
		// serves applications from front50's cached collection, which refreshes on
		// its own cycle. Without this wait, --wait would report success and the very
		// next command would 404.
		if err := waitForApplicationReadable(ctx, client, app.Name, 90*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "Application %s created (task %s %s)\n", app.Name, taskID, gate.TaskStatus(task))
		if outputIsStructured() {
			return render(task)
		}
		return nil
	},
}

// waitForApplicationReadable polls until Gate can serve the application.
//
// front50 answers application reads from a periodically refreshed cache, so an
// upsertApplication task can finish before the application is visible. This
// closes that window so that --wait means "ready to use".
func waitForApplicationReadable(ctx context.Context, client *gate.Client, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := client.GetApplication(ctx, name, false); err == nil {
			return nil
		} else if !gate.IsNotFound(err) {
			return nil // a different failure is the caller's problem to surface
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("application %s was created but is not readable yet after %s; "+
				"front50 serves applications from a cache that refreshes on its own cycle, so retry shortly", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

var (
	appDeleteWait  bool
	appDeleteForce bool
)

var appDeleteCmd = &cobra.Command{
	Use:     "delete [application]",
	Aliases: []string{"rm"},
	Short:   "Delete an application's metadata",
	Long: `Deletes an application's front50 metadata and its pipeline definitions.

This does not delete deployed infrastructure. Server groups, load balancers and
manifests the application created keep running; they simply stop being grouped
under it in Spinnaker.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would delete application %s", name)
			return nil
		}
		if !appDeleteForce {
			if err := confirm(fmt.Sprintf("Delete application %q and its pipeline definitions?", name)); err != nil {
				return err
			}
		}
		ctx := cmd.Context()
		submitCtx, cancel := cmdContext(ctx)
		taskID, err := client.DeleteApplication(submitCtx, name, gateUser)
		cancel()
		if err != nil {
			return err
		}
		audit("app.delete", name)
		if !appDeleteWait {
			fmt.Fprintf(os.Stderr, "Submitted task %s to delete application %s\n", taskID, name)
			return nil
		}
		if _, err := client.WaitForTask(ctx, taskID, 0, 5*time.Minute); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Deleted application %s\n", name)
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var appHistoryLimit int

var appHistoryCmd = &cobra.Command{
	Use:   "history [application]",
	Short: "Show an application's revision history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		history, err := client.GetApplicationHistory(ctx, args[0], appHistoryLimit)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(history)
		}
		t := newTable("UPDATED", "MODIFIED BY", "EMAIL", "DESCRIPTION")
		for _, h := range history {
			t.add(epochTime(h, "lastModified"), dash(str(h, "lastModifiedBy")), dash(str(h, "email")), dash(ellipsis(str(h, "description"), 40)))
		}
		t.print("No history for this application.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

var appResourcesCmd = &cobra.Command{
	Use:   "resources [application]",
	Short: "List cached Kubernetes resources that are not server groups or load balancers",
	Long: `Lists an application's "raw resources" — the Kubernetes objects clouddriver
cached that do not map onto Spinnaker's server-group/load-balancer model:
ConfigMaps, Secrets, custom resources and so on.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		res, err := client.GetApplicationRawResources(ctx, args[0])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(res)
		}
		t := newTable("KIND", "NAME", "ACCOUNT", "NAMESPACE")
		for _, r := range res {
			t.add(dash(str(r, "kind")), dash(strOr(r, "name", "displayName")), dash(str(r, "account")), dash(strOr(r, "namespace", "region", "location")))
		}
		t.print("No raw resources cached for this application.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

// completeApplications completes application names from the installation.
func completeApplications(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	client, err := getGate()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := cmdContext(cmd.Context())
	defer cancel()
	apps, err := client.ListApplications(ctx, gate.ListApplicationsOptions{})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := []string{}
	for _, a := range apps {
		if n := str(a, "name"); strings.HasPrefix(n, toComplete) {
			names = append(names, n)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func init() {
	appListCmd.Flags().StringVar(&appListAccount, "account", "", "only applications deployed in this account")
	appListCmd.Flags().StringVar(&appListOwner, "owner", "", "only applications with this owner email")
	appListCmd.Flags().BoolVar(&appListRegistered, "registered", false, "only applications with front50 metadata")
	appListCmd.Flags().BoolVar(&appListInferred, "inferred", false, "only applications inferred from cached infrastructure")

	appGetCmd.Flags().BoolVar(&appGetExpand, "expand", true, "include the clusters clouddriver has cached")

	appCreateCmd.Flags().StringVar(&appCreateEmail, "email", "", "owner email (required)")
	appCreateCmd.Flags().StringVar(&appCreateDescription, "description", "", "description")
	appCreateCmd.Flags().StringVar(&appCreateProviders, "cloud-providers", "", "comma-separated cloud providers, e.g. kubernetes")
	appCreateCmd.Flags().StringVarP(&appCreateFile, "file", "f", "", "read the application from a JSON/YAML file ('-' for stdin)")
	appCreateCmd.Flags().BoolVar(&appCreateWait, "wait", false, "wait for the orca task to complete")

	appDeleteCmd.Flags().BoolVar(&appDeleteWait, "wait", false, "wait for the orca task to complete")
	appDeleteCmd.Flags().BoolVar(&appDeleteForce, "force", false, "skip the confirmation prompt")

	appHistoryCmd.Flags().IntVar(&appHistoryLimit, "limit", 20, "number of revisions to show")

	appCmd.AddCommand(appListCmd, appGetCmd, appCreateCmd, appDeleteCmd, appHistoryCmd, appResourcesCmd)
	rootCmd.AddCommand(appCmd)
}
