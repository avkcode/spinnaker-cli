package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

var taskCmd = &cobra.Command{
	Use:     "task",
	Aliases: []string{"tasks"},
	Short:   "Inspect and submit ad-hoc orca tasks",
	Long: `Inspect and submit ad-hoc orca tasks.

A task is an imperative operation outside any pipeline: creating an application,
deploying a manifest, resizing a server group, deleting a load balancer. Deck
submits one every time a button is pressed, which makes 'sc task submit' the
general-purpose escape hatch for any clouddriver operation that has no dedicated
command — the CLI equivalent of gate-mcp's submit_orchestration.`,
	GroupID: GroupCore,
}

var (
	taskListLimit    int
	taskListPage     int
	taskListStatuses []string
)

var taskListCmd = &cobra.Command{
	Use:     "list [application]",
	Aliases: []string{"ls"},
	Short:   "List an application's tasks",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		tasks, err := client.ListApplicationTasks(ctx, args[0], taskListPage, taskListLimit, taskListStatuses)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(tasks)
		}
		t := newTable("ID", "DESCRIPTION", "STATUS", "STARTED", "DURATION", "BY")
		for _, tk := range tasks {
			t.add(
				str(tk, "id"),
				ellipsis(dash(str(tk, "name")), 40),
				str(tk, "status"),
				epochTime(tk, "startTime"),
				execDuration(tk),
				dash(ellipsis(taskOwner(tk), 20)),
			)
		}
		t.print("No tasks found for this application.")
		return nil
	},
	ValidArgsFunction: completeApplications,
}

// taskOwner finds who submitted a task. orca records it on the authentication
// block, falling back to the trigger for tasks created by a pipeline.
func taskOwner(task map[string]any) string {
	if auth := mapField(task, "authentication"); auth != nil {
		if u := str(auth, "user"); u != "" {
			return u
		}
	}
	return str(mapField(task, "trigger"), "user")
}

var taskGetCmd = &cobra.Command{
	Use:   "get [task-id]",
	Short: "Print a task as JSON",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		task, err := client.GetTask(ctx, args[0])
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(task)
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintf(tw, "Task:\t%s\n", str(task, "id"))
		fmt.Fprintf(tw, "Description:\t%s\n", dash(str(task, "name")))
		fmt.Fprintf(tw, "Application:\t%s\n", dash(str(task, "application")))
		fmt.Fprintf(tw, "Status:\t%s\n", str(task, "status"))
		fmt.Fprintf(tw, "Started:\t%s\n", epochTime(task, "startTime"))
		fmt.Fprintf(tw, "Duration:\t%s\n", execDuration(task))
		fmt.Fprintf(tw, "Submitted by:\t%s\n", dash(taskOwner(task)))
		if msg := gate.TaskFailureMessage(task); msg != "" {
			fmt.Fprintf(tw, "Failure:\t%s\n", msg)
		}
		w.FlushTable(tw)

		// orca calls a task's stages "steps".
		if steps := mapList(listField(task, "steps")); len(steps) > 0 {
			fmt.Println()
			st := newTable("STEP", "STATUS", "DURATION")
			for _, s := range steps {
				st.add(dash(str(s, "name")), str(s, "status"), execDuration(s))
			}
			st.print("")
		}
		return nil
	},
}

var (
	taskSubmitFile        string
	taskSubmitApp         string
	taskSubmitDescription string
	taskSubmitWait        bool
)

var taskSubmitCmd = &cobra.Command{
	Use:     "submit",
	Aliases: []string{"create"},
	Short:   "Submit an ad-hoc orca task",
	Long: `Submits an ad-hoc orca task from a JSON/YAML file.

The file is either a full task document ({application, description, job: [...]})
or just the job array. Every clouddriver operation is reachable this way, which
makes this the escape hatch for anything sc has not wrapped — the counterpart to
'sc api' for write operations that need orchestration.

Operation types and their fields are documented per provider; the most reliable
reference is what Deck sends, which 'sc task get' on an existing task will show.`,
	Example: `  sc task submit -f deploy-manifest.json --application demo --wait
  echo '[{"type":"disableServerGroup","serverGroupName":"demo-v001","region":"default","credentials":"managing"}]' | \
    sc task submit -f - --application demo --description "disable demo-v001"`,
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := readInput(taskSubmitFile)
		if err != nil {
			return err
		}

		req := gate.TaskRequest{Application: taskSubmitApp, Description: taskSubmitDescription}
		// Accept either shape: a task document, or a bare job array.
		if jobs, err := readJSONOrYAMLListBytes(raw); err == nil && len(jobs) > 0 {
			for _, j := range jobs {
				req.Job = append(req.Job, j)
			}
		} else {
			doc, err := readJSONOrYAMLBytes(raw)
			if err != nil {
				return fmt.Errorf("input must be a task document or a job array: %w", err)
			}
			for _, j := range mapList(listField(doc, "job")) {
				req.Job = append(req.Job, j)
			}
			if req.Application == "" {
				req.Application = str(doc, "application")
			}
			if req.Description == "" {
				req.Description = str(doc, "description")
			}
		}
		if len(req.Job) == 0 {
			return fmt.Errorf("no job operations found in the input")
		}
		if req.Application == "" {
			return fmt.Errorf("--application is required (orca scopes tasks to an application)")
		}
		if req.Description == "" {
			req.Description = fmt.Sprintf("sc task: %s", str(req.Job[0], "type"))
		}

		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would submit task %q to application %s with %d operation(s)", req.Description, req.Application, len(req.Job))
			return nil
		}
		ctx := cmd.Context()
		submitCtx, cancel := cmdContext(ctx)
		taskID, err := client.CreateTask(submitCtx, req)
		cancel()
		if err != nil {
			return err
		}
		audit("task.submit", fmt.Sprintf("%s: %s", req.Application, req.Description))
		fmt.Fprintf(os.Stderr, "Submitted task %s\n", taskID)

		if !taskSubmitWait {
			fmt.Println(taskID)
			return nil
		}
		task, err := client.WaitForTask(ctx, taskID, taskWaitInterval, taskWaitTimeout)
		if err != nil {
			return &ExecFailedError{Msg: err.Error()}
		}
		fmt.Fprintf(os.Stderr, "Task %s %s\n", taskID, gate.TaskStatus(task))
		if outputIsStructured() {
			return render(task)
		}
		return nil
	},
}

var (
	taskWaitTimeout  time.Duration
	taskWaitInterval time.Duration
)

var taskWaitCmd = &cobra.Command{
	Use:   "wait [task-id]",
	Short: "Wait for a task to finish",
	Long:  "Waits for a task to reach a terminal status. Exits 0 on success and 8 on failure.",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		task, err := client.WaitForTask(cmd.Context(), args[0], taskWaitInterval, taskWaitTimeout)
		if err != nil {
			return &ExecFailedError{Msg: err.Error()}
		}
		fmt.Fprintf(os.Stderr, "Task %s %s\n", args[0], gate.TaskStatus(task))
		if outputIsStructured() {
			return render(task)
		}
		return nil
	},
}

var taskCancelCmd = &cobra.Command{
	Use:   "cancel [task-id...]",
	Short: "Cancel one or more running tasks",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		if isDryRun() {
			dryRunMsg("would cancel %d task(s): %v", len(args), args)
			return nil
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		if len(args) == 1 {
			if err := client.CancelTask(ctx, args[0]); err != nil {
				return err
			}
		} else if err := client.CancelTasks(ctx, args); err != nil {
			return err
		}
		audit("task.cancel", fmt.Sprint(args))
		fmt.Fprintf(os.Stderr, "Cancelled %d task(s)\n", len(args))
		return nil
	},
}

func init() {
	taskListCmd.Flags().IntVar(&taskListLimit, "limit", 25, "maximum tasks to return")
	taskListCmd.Flags().IntVar(&taskListPage, "page", 0, "page number")
	taskListCmd.Flags().StringArrayVar(&taskListStatuses, "status", nil, "filter by status (repeatable)")

	taskSubmitCmd.Flags().StringVarP(&taskSubmitFile, "file", "f", "-", "JSON/YAML task document or job array ('-' for stdin)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitApp, "application", "", "application to scope the task to (required)")
	taskSubmitCmd.Flags().StringVar(&taskSubmitDescription, "description", "", "human-readable description")
	taskSubmitCmd.Flags().BoolVar(&taskSubmitWait, "wait", false, "wait for the task to finish; exit 8 if it failed")
	taskSubmitCmd.Flags().DurationVar(&taskWaitTimeout, "wait-timeout", 0, "with --wait, give up after this long (0 = no limit)")
	taskSubmitCmd.Flags().DurationVar(&taskWaitInterval, "interval", 2*time.Second, "with --wait, poll interval")

	taskWaitCmd.Flags().DurationVar(&taskWaitTimeout, "wait-timeout", 0, "give up after this long (0 = no limit)")
	taskWaitCmd.Flags().DurationVar(&taskWaitInterval, "interval", 2*time.Second, "poll interval")

	taskCmd.AddCommand(taskListCmd, taskGetCmd, taskSubmitCmd, taskWaitCmd, taskCancelCmd)
	rootCmd.AddCommand(taskCmd)
}
