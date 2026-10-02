package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var pipelineCmd = &cobra.Command{
	Use:     "pipeline",
	Aliases: []string{"pipelines", "pl"},
	Short:   "Manage pipeline definitions",
	Long: `Manage pipeline definitions (what Spinnaker calls pipeline configs).

A definition is distinct from an execution: 'sc pipeline' manages the former and
'sc exec' the latter. Definitions are JSON documents stored in front50, so
'sc pipeline get' and 'sc pipeline apply' are enough to keep them in version
control.`,
	GroupID: GroupCore,
}

var pipelineListCmd = &cobra.Command{
	Use:     "list [application]",
	Aliases: []string{"ls"},
	Short:   "List an application's pipeline definitions",
	Long:    "Lists an application's pipeline definitions, or every definition on the installation when no application is given.",
	Args:    cobra.MaximumNArgs(1),
	Example: `  sc pipeline list demo
  sc pipeline list            # every application`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		var pipelines gate.JSONList
		if len(args) == 1 {
			pipelines, err = client.ListPipelineConfigs(ctx, args[0])
		} else {
			pipelines, err = client.ListAllPipelineConfigs(ctx)
		}
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(pipelines)
		}

		t := newTable("APPLICATION", "NAME", "ID", "STAGES", "TRIGGERS", "STATE")
		for _, p := range pipelines {
			state := "enabled"
			if boolean(p, "disabled") {
				state = "disabled"
			}
			t.add(
				str(p, "application"),
				str(p, "name"),
				str(p, "id"),
				fmt.Sprint(len(listField(p, "stages"))),
				triggerSummary(p),
				state,
			)
		}
		t.print("No pipeline definitions found.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

// triggerSummary condenses a pipeline's triggers into "type×n" labels, so a
// listing shows what starts each pipeline without expanding the definition.
func triggerSummary(p map[string]any) string {
	triggers := mapList(listField(p, "triggers"))
	if len(triggers) == 0 {
		return "manual"
	}
	counts := map[string]int{}
	order := []string{}
	for _, tr := range triggers {
		ty := str(tr, "type")
		if ty == "" {
			ty = "unknown"
		}
		if !boolean(tr, "enabled") {
			ty += "(off)"
		}
		if counts[ty] == 0 {
			order = append(order, ty)
		}
		counts[ty]++
	}
	parts := make([]string, 0, len(order))
	for _, ty := range order {
		if counts[ty] > 1 {
			parts = append(parts, fmt.Sprintf("%s×%d", ty, counts[ty]))
			continue
		}
		parts = append(parts, ty)
	}
	return strings.Join(parts, ",")
}

var pipelineGetCmd = &cobra.Command{
	Use:   "get [application] [pipeline]",
	Short: "Print a pipeline definition as JSON",
	Long: `Prints a pipeline definition as JSON — the export half of pipelines-as-code.

The output round-trips through 'sc pipeline apply', so a definition can be
exported, committed, reviewed and re-applied.`,
	Args: cobra.ExactArgs(2),
	Example: `  sc pipeline get demo deploy > deploy.json
  sc pipeline get demo deploy -o yaml`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		pipeline, err := client.GetPipelineConfig(ctx, args[0], args[1])
		if err != nil {
			return err
		}
		if len(pipeline) == 0 {
			return fmt.Errorf("pipeline %q not found in application %q", args[1], args[0])
		}
		// A definition is a document, so JSON is the right default here even
		// though most commands default to a table.
		if viper.GetString("output") == "yaml" {
			return getOutput().PrintYAML(pipeline)
		}
		return getOutput().PrintJSON(pipeline)
	},
	ValidArgsFunction: completePipelineArgs,
}

var (
	pipelineApplyFile string
	pipelineApplyApp  string
	pipelineApplyName string
	pipelineApplyBulk bool
)

var pipelineApplyCmd = &cobra.Command{
	Use:     "apply",
	Aliases: []string{"save"},
	Short:   "Create or update pipeline definitions from a file",
	Long: `Creates or updates pipeline definitions from a JSON or YAML file.

An "id" in the document makes this an update of that definition; without one,
front50 assigns a new id. --application and --name override the values in the
file, which is what makes one definition reusable across applications.

--bulk reads a JSON/YAML array and saves every definition in a single call,
which is substantially faster than looping for a whole-application sync.`,
	Example: `  sc pipeline apply -f deploy.json
  sc pipeline apply -f deploy.json --application demo --name deploy-staging
  sc pipeline apply -f all-pipelines.json --bulk
  sc pipeline get demo deploy | sc pipeline apply -f - --application demo2`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		if pipelineApplyBulk {
			pipelines, err := readJSONOrYAMLList(pipelineApplyFile)
			if err != nil {
				return err
			}
			for i, p := range pipelines {
				if str(p, "application") == "" || str(p, "name") == "" {
					return fmt.Errorf("pipeline at index %d is missing 'application' or 'name'", i)
				}
			}
			if isDryRun() {
				dryRunMsg("would save %d pipeline definitions", len(pipelines))
				return nil
			}
			result, err := client.BulkSavePipelineConfigs(ctx, pipelines)
			if err != nil {
				return err
			}
			audit("pipeline.bulksave", fmt.Sprintf("%d pipelines", len(pipelines)))
			if outputIsStructured() {
				return render(result)
			}
			fmt.Fprintf(os.Stderr, "Saved %d pipeline definitions\n", len(pipelines))
			return nil
		}

		pipeline, err := readJSONOrYAML(pipelineApplyFile)
		if err != nil {
			return err
		}
		if pipelineApplyApp != "" {
			pipeline["application"] = pipelineApplyApp
		}
		if pipelineApplyName != "" {
			pipeline["name"] = pipelineApplyName
		}
		app, name := str(pipeline, "application"), str(pipeline, "name")
		if app == "" || name == "" {
			return fmt.Errorf("pipeline must have 'application' and 'name' (set them in the file or via --application/--name)")
		}
		// Re-applying an export under a new application must not keep the old id,
		// which belongs to the original definition.
		if pipelineApplyApp != "" && str(pipeline, "application") != app {
			delete(pipeline, "id")
		}

		if isDryRun() {
			dryRunMsg("would save pipeline %s/%s (%d stages)", app, name, len(listField(pipeline, "stages")))
			return nil
		}
		if err := client.SavePipelineConfig(ctx, pipeline); err != nil {
			return err
		}
		audit("pipeline.save", app+"/"+name)
		fmt.Fprintf(os.Stderr, "Saved pipeline %s/%s\n", app, name)
		return nil
	},
}

var pipelineDeleteForce bool

var pipelineDeleteCmd = &cobra.Command{
	Use:     "delete [application] [pipeline]",
	Aliases: []string{"rm"},
	Short:   "Delete a pipeline definition",
	Args:    cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would delete pipeline %s/%s", args[0], args[1])
			return nil
		}
		if !pipelineDeleteForce {
			if err := confirm(fmt.Sprintf("Delete pipeline definition %q in application %q?", args[1], args[0])); err != nil {
				return err
			}
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.DeletePipelineConfig(ctx, args[0], args[1]); err != nil {
			return err
		}
		audit("pipeline.delete", args[0]+"/"+args[1])
		fmt.Fprintf(os.Stderr, "Deleted pipeline %s/%s\n", args[0], args[1])
		return nil
	},
	ValidArgsFunction: completePipelineArgs,
}

var pipelineRenameCmd = &cobra.Command{
	Use:   "rename [application] [from] [to]",
	Short: "Rename a pipeline definition, preserving its execution history",
	Long:  "Renames a pipeline definition in place. Its id — and therefore its execution history — is preserved.",
	Args:  cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would rename pipeline %s/%s to %s", args[0], args[1], args[2])
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if err := client.RenamePipelineConfig(ctx, args[0], args[1], args[2]); err != nil {
			return err
		}
		audit("pipeline.rename", fmt.Sprintf("%s/%s -> %s", args[0], args[1], args[2]))
		fmt.Fprintf(os.Stderr, "Renamed pipeline %s/%s to %s\n", args[0], args[1], args[2])
		return nil
	},
	ValidArgsFunction: completePipelineArgs,
}

func pipelineToggleCmd(enable bool) *cobra.Command {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	return &cobra.Command{
		Use:   verb + " [application] [pipeline]",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a pipeline definition",
		Long: fmt.Sprintf(`%ss a pipeline definition.

front50 has no dedicated endpoint for this, so the definition is read, its
'disabled' flag set and the whole document saved back. A concurrent edit through
Deck can therefore be overwritten.`, strings.ToUpper(verb[:1])+verb[1:]),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := getGate()
			if err != nil {
				return err
			}
			if isDryRun() {
				dryRunMsg("would %s pipeline %s/%s", verb, args[0], args[1])
				return nil
			}
			ctx, cancel := cmdContext(cmd.Context())
			defer cancel()
			if err := client.SetPipelineConfigDisabled(ctx, args[0], args[1], !enable); err != nil {
				return err
			}
			audit("pipeline."+verb, args[0]+"/"+args[1])
			fmt.Fprintf(os.Stderr, "%sd pipeline %s/%s\n", strings.ToUpper(verb[:1])+verb[1:], args[0], args[1])
			return nil
		},
		ValidArgsFunction: completePipelineArgs,
	}
}

var (
	pipelineRunParams   []string
	pipelineRunWait     bool
	pipelineRunFollow   bool
	pipelineRunViaEcho  bool
	pipelineRunType     string
	pipelineRunArtifact string
)

var pipelineRunCmd = &cobra.Command{
	Use:     "run [application] [pipeline]",
	Aliases: []string{"start", "trigger", "exec"},
	Short:   "Trigger a pipeline execution",
	Long: `Triggers a pipeline and prints the new execution id.

--wait blocks until the execution reaches a terminal status and exits non-zero
(8) if it failed, so CI can gate on the result. --follow additionally renders
each stage as it changes state.`,
	Args: cobra.ExactArgs(2),
	Example: `  sc pipeline run demo deploy
  sc pipeline run demo deploy -P BRANCH=main -P REPLICAS=3
  sc pipeline run demo deploy --follow
  sc pipeline run demo deploy --wait -o json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		app, name := args[0], args[1]
		params, err := parseKeyValues(pipelineRunParams)
		if err != nil {
			return err
		}
		var artifacts []gate.JSONMap
		if pipelineRunArtifact != "" {
			raw, err := readJSONOrYAMLList(pipelineRunArtifact)
			if err != nil {
				return err
			}
			for _, a := range raw {
				artifacts = append(artifacts, a)
			}
		}

		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would trigger pipeline %s/%s with parameters %v", app, name, params)
			return nil
		}
		// Triggering is quick, but --wait/--follow can run for the length of a
		// deployment, so the wait is not bounded by the global request timeout.
		ctx := cmd.Context()
		triggerCtx, cancel := cmdContext(ctx)
		execID, err := client.RunPipeline(triggerCtx, app, name, gate.RunPipelineOptions{
			Parameters: params,
			Artifacts:  artifacts,
			Type:       pipelineRunType,
			User:       gateUser,
			ViaEcho:    pipelineRunViaEcho,
		})
		cancel()
		if err != nil {
			return err
		}
		audit("pipeline.run", app+"/"+name+" -> "+execID)

		if pipelineRunViaEcho {
			fmt.Fprintf(os.Stderr, "Submitted %s/%s via echo (eventId %s); the execution id is assigned asynchronously\n", app, name, execID)
			fmt.Println(execID)
			return nil
		}
		fmt.Fprintf(os.Stderr, "Triggered %s/%s as execution %s\n", app, name, execID)

		if !pipelineRunWait && !pipelineRunFollow {
			if outputIsStructured() {
				return render(map[string]any{"application": app, "pipeline": name, "executionId": execID})
			}
			fmt.Println(execID)
			return nil
		}
		return followExecution(ctx, client, execID, pipelineRunFollow)
	},
	ValidArgsFunction: completePipelineArgs,
}

var pipelineHistoryLimit int

var pipelineHistoryCmd = &cobra.Command{
	Use:   "history [pipeline-config-id]",
	Short: "Show a pipeline definition's revision history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		history, err := client.GetPipelineConfigHistory(ctx, args[0], pipelineHistoryLimit)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(history)
		}
		t := newTable("UPDATED", "MODIFIED BY", "NAME", "STAGES")
		for _, h := range history {
			t.add(epochTime(h, "updateTs"), dash(str(h, "lastModifiedBy")), str(h, "name"), fmt.Sprint(len(listField(h, "stages"))))
		}
		t.print("No revision history for this pipeline.")
		return nil
	},
}

var pipelineTemplateCmd = &cobra.Command{
	Use:     "template",
	Aliases: []string{"templates"},
	Short:   "Inspect v2 managed pipeline templates",
}

var pipelineTemplateListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List v2 pipeline templates",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		templates, err := client.ListPipelineTemplates(ctx, nil)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(templates)
		}
		t := newTable("ID", "NAME", "OWNER", "SCOPES")
		for _, tpl := range templates {
			meta := mapField(tpl, "metadata")
			scopes := []string{}
			for _, s := range listField(meta, "scopes") {
				scopes = append(scopes, fmt.Sprint(s))
			}
			t.add(str(tpl, "id"), dash(str(meta, "name")), dash(str(meta, "owner")), dash(strings.Join(scopes, ",")))
		}
		t.print("No pipeline templates found.")
		return nil
	},
}

var pipelineTemplateGetCmd = &cobra.Command{
	Use:   "get [template-id]",
	Short: "Print a v2 pipeline template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		tpl, err := client.GetPipelineTemplate(ctx, args[0])
		if err != nil {
			return err
		}
		return render(tpl)
	},
}

var pipelineConvertCmd = &cobra.Command{
	Use:   "convert-to-template [pipeline-config-id]",
	Short: "Render an existing pipeline definition as a v2 pipeline template",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		out, err := client.ConvertPipelineConfigToTemplate(ctx, args[0])
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimSpace(out))
		return nil
	},
}

// completePipelineArgs completes [application] then [pipeline].
func completePipelineArgs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completeApplications(cmd, args, toComplete)
	}
	if len(args) > 1 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	client, err := getGate()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := cmdContext(cmd.Context())
	defer cancel()
	pipelines, err := client.ListPipelineConfigs(ctx, args[0])
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := []string{}
	for _, p := range pipelines {
		if n := str(p, "name"); strings.HasPrefix(n, toComplete) {
			names = append(names, n)
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// jsonCompact renders a value as single-line JSON for table cells.
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func init() {
	pipelineApplyCmd.Flags().StringVarP(&pipelineApplyFile, "file", "f", "-", "JSON/YAML file to read ('-' for stdin)")
	pipelineApplyCmd.Flags().StringVar(&pipelineApplyApp, "application", "", "override the application in the file")
	pipelineApplyCmd.Flags().StringVar(&pipelineApplyName, "name", "", "override the pipeline name in the file")
	pipelineApplyCmd.Flags().BoolVar(&pipelineApplyBulk, "bulk", false, "the file is an array of pipeline definitions; save them in one call")

	pipelineDeleteCmd.Flags().BoolVar(&pipelineDeleteForce, "force", false, "skip the confirmation prompt")

	pipelineRunCmd.Flags().StringArrayVarP(&pipelineRunParams, "parameter", "P", nil, "pipeline parameter KEY=VALUE (repeatable)")
	pipelineRunCmd.Flags().BoolVar(&pipelineRunWait, "wait", false, "wait for the execution to finish; exit 8 if it failed")
	pipelineRunCmd.Flags().BoolVar(&pipelineRunFollow, "follow", false, "wait and render each stage as it progresses")
	pipelineRunCmd.Flags().BoolVar(&pipelineRunViaEcho, "via-echo", false, "trigger through echo instead of orca (returns an eventId)")
	pipelineRunCmd.Flags().StringVar(&pipelineRunType, "trigger-type", "manual", "trigger type to record")
	pipelineRunCmd.Flags().StringVar(&pipelineRunArtifact, "artifacts", "", "JSON/YAML file of trigger artifacts")

	pipelineHistoryCmd.Flags().IntVar(&pipelineHistoryLimit, "limit", 20, "number of revisions to show")

	pipelineTemplateCmd.AddCommand(pipelineTemplateListCmd, pipelineTemplateGetCmd)
	pipelineCmd.AddCommand(
		pipelineListCmd, pipelineGetCmd, pipelineApplyCmd, pipelineDeleteCmd, pipelineRenameCmd,
		pipelineToggleCmd(true), pipelineToggleCmd(false), pipelineRunCmd, pipelineHistoryCmd,
		pipelineTemplateCmd, pipelineConvertCmd,
	)
	rootCmd.AddCommand(pipelineCmd)
}
