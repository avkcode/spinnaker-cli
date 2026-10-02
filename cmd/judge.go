package cmd

import (
	"fmt"
	"os"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/spf13/cobra"
)

var judgeCmd = &cobra.Command{
	Use:     "judge",
	Aliases: []string{"judgment", "judgement"},
	Short:   "Answer manual judgment stages",
	Long: `Find and answer manual judgment stages — the stages where a pipeline stops and
waits for a human.

A pipeline paused on a manual judgment is holding a deployment open, so these are
the executions an operator most often needs to find quickly. There is no Gate
endpoint that lists them: 'sc judge list' derives the list by scanning running
executions for a running manualJudgment stage.`,
	GroupID: GroupCore,
}

var (
	judgeListApp   string
	judgeListLimit int
)

var judgeListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "pending"},
	Short:   "List executions waiting on a manual judgment",
	Example: `  sc judge list
  sc judge list --application demo`,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		pending, err := client.ListPendingJudgments(ctx, judgeListApp, judgeListLimit)
		if err != nil {
			return err
		}
		if outputIsStructured() {
			return render(pending)
		}
		t := newTable("EXECUTION", "APPLICATION", "PIPELINE", "STAGE", "WAITING", "INSTRUCTIONS")
		for _, p := range pending {
			waiting := "-"
			if p.StartTime > 0 {
				waiting = age(map[string]any{"t": float64(p.StartTime)}, "t")
			}
			t.add(p.ExecutionID, p.Application, ellipsis(p.Pipeline, 22), ellipsis(p.StageName, 20), waiting, ellipsis(p.Instructions, 44))
		}
		t.print("No executions are waiting on a manual judgment.")
		return nil
	},
}

var judgeInput string

func judgeDecisionCmd(decision string) *cobra.Command {
	verb := "Continue"
	short := "Approve a manual judgment, letting the pipeline proceed"
	if decision == gate.JudgmentStop {
		verb = "Stop"
		short = "Reject a manual judgment, stopping the pipeline"
	}
	return &cobra.Command{
		Use:   decision + " [execution-id] [stage-name-or-id]",
		Short: short,
		Long: fmt.Sprintf(`%ss a manual judgment stage.

The stage may be named or identified by id. When the execution has exactly one
pending judgment the stage argument may be omitted.

--input selects one of the stage's configured judgmentInputs, which pipelines read
downstream as ${#judgment("stage name")}.`, verb),
		Args: cobra.RangeArgs(1, 2),
		Example: fmt.Sprintf(`  sc judge %s 01M3Y...
  sc judge %s 01M3Y... "approve deploy"
  sc judge %s 01M3Y... "approve deploy" --input rollback`, decision, decision, decision),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := getGate()
			if err != nil {
				return err
			}
			ctx, cancel := cmdContext(cmd.Context())
			defer cancel()

			execID := args[0]
			exec, err := client.GetExecution(ctx, execID)
			if err != nil {
				return err
			}

			var stageID, stageName string
			if len(args) == 2 {
				stageID, stageName, err = resolveStage(exec, args[1])
				if err != nil {
					return err
				}
			} else {
				// With no stage given, the only unambiguous choice is a single
				// pending judgment.
				candidates := [][2]string{}
				for _, s := range mapList(listField(exec, "stages")) {
					if str(s, "type") == "manualJudgment" && str(s, "status") == gate.StatusRunning {
						candidates = append(candidates, [2]string{str(s, "id"), str(s, "name")})
					}
				}
				switch len(candidates) {
				case 1:
					stageID, stageName = candidates[0][0], candidates[0][1]
				case 0:
					return fmt.Errorf("execution %s has no manual judgment waiting for a decision", execID)
				default:
					names := []string{}
					for _, c := range candidates {
						names = append(names, c[1])
					}
					return fmt.Errorf("execution %s has %d pending judgments; name one of: %v", execID, len(candidates), names)
				}
			}

			if isDryRun() {
				dryRunMsg("would answer %q on stage %q of execution %s", decision, stageName, execID)
				return nil
			}
			if _, err := client.JudgeStage(ctx, execID, stageID, decision, judgeInput); err != nil {
				return err
			}
			audit("judge."+decision, fmt.Sprintf("%s/%s", execID, stageName))
			fmt.Fprintf(os.Stderr, "Answered %q on stage %q of execution %s\n", decision, stageName, execID)
			return nil
		},
	}
}

var judgeGetCmd = &cobra.Command{
	Use:   "get [execution-id]",
	Short: "Show the pending manual judgment(s) for one execution",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()
		exec, err := client.GetExecution(ctx, args[0])
		if err != nil {
			return err
		}
		type judgment struct {
			StageID      string   `json:"stageId" yaml:"stageId"`
			StageName    string   `json:"stageName" yaml:"stageName"`
			Status       string   `json:"status" yaml:"status"`
			Instructions string   `json:"instructions,omitempty" yaml:"instructions,omitempty"`
			Inputs       []string `json:"inputs,omitempty" yaml:"inputs,omitempty"`
			JudgedBy     string   `json:"judgedBy,omitempty" yaml:"judgedBy,omitempty"`
			Decision     string   `json:"decision,omitempty" yaml:"decision,omitempty"`
		}
		out := []judgment{}
		for _, s := range mapList(listField(exec, "stages")) {
			if str(s, "type") != "manualJudgment" {
				continue
			}
			sctx := mapField(s, "context")
			j := judgment{
				StageID:      str(s, "id"),
				StageName:    str(s, "name"),
				Status:       str(s, "status"),
				Instructions: str(sctx, "instructions"),
				JudgedBy:     str(sctx, "lastModifiedBy"),
				Decision:     str(sctx, "judgmentStatus"),
			}
			for _, in := range listField(sctx, "judgmentInputs") {
				switch v := in.(type) {
				case string:
					j.Inputs = append(j.Inputs, v)
				case map[string]any:
					j.Inputs = append(j.Inputs, str(v, "value"))
				}
			}
			out = append(out, j)
		}
		if outputIsStructured() {
			return render(out)
		}
		t := newTable("STAGE", "STATUS", "DECISION", "JUDGED BY", "INPUTS", "INSTRUCTIONS")
		for _, j := range out {
			inputs := "-"
			if len(j.Inputs) > 0 {
				inputs = fmt.Sprint(j.Inputs)
			}
			t.add(ellipsis(j.StageName, 22), j.Status, dash(j.Decision), dash(j.JudgedBy), inputs, ellipsis(j.Instructions, 40))
		}
		t.print("This execution has no manual judgment stages.")
		return nil
	},
}

func init() {
	judgeListCmd.Flags().StringVar(&judgeListApp, "application", "*", "application to scan ('*' for all)")
	judgeListCmd.Flags().IntVar(&judgeListLimit, "limit", 50, "executions to scan per pipeline")

	continueCmd := judgeDecisionCmd(gate.JudgmentContinue)
	stopCmd := judgeDecisionCmd(gate.JudgmentStop)
	for _, c := range []*cobra.Command{continueCmd, stopCmd} {
		c.Flags().StringVar(&judgeInput, "input", "", "select one of the stage's configured judgment inputs")
	}

	judgeCmd.AddCommand(judgeListCmd, judgeGetCmd, continueCmd, stopCmd)
	rootCmd.AddCommand(judgeCmd)
}
