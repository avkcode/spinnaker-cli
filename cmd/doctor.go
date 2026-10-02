package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	scsvc "github.com/avkcode/spinnaker-cli/pkg/svc"
	"github.com/spf13/cobra"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Check connectivity and identity against Gate",
	Long: `Checks that Gate is reachable, that the configured credentials authenticate, and
reports the version and the identity's roles.

This only covers Gate. Gate's /health reports Gate's own status and aggregates
nothing, so a healthy answer here does not mean the installation is healthy — use
'sc svc list' for that.`,
	GroupID: GroupCore,
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := getGate()
		if err != nil {
			return err
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		result := map[string]any{"endpoint": client.Endpoint()}
		health, herr := client.Health(ctx)
		if herr != nil {
			result["reachable"] = false
			result["error"] = herr.Error()
			if outputIsStructured() {
				_ = render(result)
			}
			return herr
		}
		result["reachable"] = true
		result["gateHealth"] = health

		if version, err := client.Version(ctx); err == nil {
			result["version"] = version["version"]
		}
		user, uerr := client.AuthUser(ctx)
		if uerr != nil {
			result["authenticated"] = false
			result["authError"] = uerr.Error()
		} else {
			username, _ := user["username"].(string)
			result["authenticated"] = username != ""
			result["username"] = username
			result["roles"] = user["roles"]
		}
		if accounts, err := client.ListAccounts(ctx, false); err == nil {
			names := []string{}
			for _, a := range accounts {
				names = append(names, str(a, "name"))
			}
			result["accounts"] = names
		}

		if outputIsStructured() {
			return render(result)
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintf(tw, "Endpoint:\t%s\n", result["endpoint"])
		fmt.Fprintf(tw, "Gate status:\t%v\n", health["status"])
		if v, ok := result["version"]; ok {
			fmt.Fprintf(tw, "Version:\t%v\n", v)
		}
		if username, ok := result["username"].(string); ok && username != "" {
			fmt.Fprintf(tw, "Authenticated as:\t%s\n", username)
		} else {
			fmt.Fprintf(tw, "Authenticated as:\t(anonymous — Gate accepted the request without an identity)\n")
		}
		if roles, ok := result["roles"].([]any); ok && len(roles) > 0 {
			strs := []string{}
			for _, r := range roles {
				strs = append(strs, fmt.Sprint(r))
			}
			fmt.Fprintf(tw, "Roles:\t%s\n", strings.Join(strs, ", "))
		}
		if accounts, ok := result["accounts"].([]string); ok {
			fmt.Fprintf(tw, "Accounts:\t%s\n", dash(strings.Join(accounts, ", ")))
		}
		w.FlushTable(tw)
		return nil
	},
}

// ---------------------------------------------------------------------------
// doctor
// ---------------------------------------------------------------------------

// finding is one diagnostic observation.
type finding struct {
	// Level is OK, WARN or FAIL.
	Level   string `json:"level" yaml:"level"`
	Area    string `json:"area" yaml:"area"`
	Message string `json:"message" yaml:"message"`
	// Hint is the suggested next step, when there is a concrete one.
	Hint string `json:"hint,omitempty" yaml:"hint,omitempty"`
}

const (
	levelOK   = "OK"
	levelWarn = "WARN"
	levelFail = "FAIL"
)

var doctorExecution string

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose an installation, or one failed execution",
	Long: `Runs a battery of checks across both planes and reports what is wrong.

With no arguments it checks the installation: Gate reachability and identity, every
service's deployment state and health, whether the operator plane's actuator
endpoints are exposed, and whether any accounts are configured.

With --execution it diagnoses one failed execution instead: which stages failed,
the exception each reported, and — where the failure points at a service — that
service's current health.`,
	GroupID: GroupOperator,
	Example: `  sc doctor
  sc doctor --execution 01M3Y567A2CTDWCP8T7QAR7GH8`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		var findings []finding
		if doctorExecution != "" {
			f, err := diagnoseExecution(ctx, doctorExecution)
			if err != nil {
				return err
			}
			findings = f
		} else {
			findings = diagnoseInstallation(ctx)
		}

		if outputIsStructured() {
			if err := render(findings); err != nil {
				return err
			}
		} else {
			t := newTable("", "AREA", "FINDING")
			for _, f := range findings {
				msg := f.Message
				if f.Hint != "" {
					msg += "\n\t\t→ " + f.Hint
				}
				t.add(f.Level, f.Area, msg)
			}
			t.print("Nothing to report.")
		}

		for _, f := range findings {
			if f.Level == levelFail {
				return &ExecFailedError{Msg: "doctor found problems; see the FAIL entries above"}
			}
		}
		return nil
	},
}

// diagnoseInstallation checks Gate and the operator plane.
func diagnoseInstallation(ctx context.Context) []finding {
	findings := []finding{}

	client, err := getGate()
	if err != nil {
		findings = append(findings, finding{levelFail, "gate", err.Error(), "run 'sc login --gate <url> --user <user> --password <pw>'"})
	} else {
		if health, err := client.Health(ctx); err != nil {
			findings = append(findings, finding{levelFail, "gate", "Gate is not reachable: " + err.Error(),
				"check the endpoint (it must include Gate's context path, e.g. /api/v1) and credentials"})
		} else {
			status := fmt.Sprint(health["status"])
			level := levelOK
			if status != "UP" {
				level = levelFail
			}
			findings = append(findings, finding{level, "gate", "Gate reports " + status, ""})
		}

		if user, err := client.AuthUser(ctx); err == nil {
			if username, _ := user["username"].(string); username != "" {
				findings = append(findings, finding{levelOK, "auth", "authenticated as " + username, ""})
			} else {
				findings = append(findings, finding{levelWarn, "auth",
					"Gate accepted the request without an identity",
					"anonymous access means Fiat is off and every caller has full permissions; fine for a lab, not for shared use"})
			}
		}

		if accounts, err := client.ListAccounts(ctx, false); err == nil {
			if len(accounts) == 0 {
				findings = append(findings, finding{levelFail, "accounts", "no cloud accounts are configured",
					"nothing can be deployed until a provider account exists"})
			} else {
				names := []string{}
				unauthorized := []string{}
				for _, a := range accounts {
					names = append(names, str(a, "name"))
					if !boolean(a, "authorized") {
						unauthorized = append(unauthorized, str(a, "name"))
					}
				}
				findings = append(findings, finding{levelOK, "accounts",
					fmt.Sprintf("%d account(s): %s", len(accounts), strings.Join(names, ", ")), ""})
				if len(unauthorized) > 0 {
					findings = append(findings, finding{levelWarn, "accounts",
						"not authorized for: " + strings.Join(unauthorized, ", "),
						"the current identity lacks permission on these accounts"})
				}
			}
		}
	}

	// Operator plane.
	sclient, err := getSvc()
	if err != nil {
		findings = append(findings, finding{levelWarn, "operator", err.Error(), "the operator-plane checks were skipped"})
		return findings
	}
	if !sclient.HasKube() {
		findings = append(findings, finding{levelWarn, "operator",
			"no Kubernetes access, so service-level checks were skipped",
			"set --kubeconfig/$KUBECONFIG, or use --service-url for a non-Kubernetes install"})
		return findings
	}
	findings = append(findings, finding{levelOK, "operator", "cluster credentials from " + sclient.KubeSource(), ""})

	statuses, err := sclient.List(ctx)
	if err != nil {
		findings = append(findings, finding{levelFail, "operator", "listing services failed: " + err.Error(),
			"check --namespace (default: spinnaker) and that the credentials can read Deployments"})
		return findings
	}
	for _, s := range statuses {
		switch {
		case !s.Deployed:
			level := levelWarn
			if isOptionalService(s.Name) {
				level = levelOK
			}
			findings = append(findings, finding{level, s.Name, "not deployed", s.Note})
		case s.Health == "UP" || s.Health == "N/A":
			findings = append(findings, finding{levelOK, s.Name, fmt.Sprintf("%s replicas, %s", s.Replicas, dash(s.Version)), ""})
		case s.Health == "SCALED_TO_ZERO":
			level := levelWarn
			if isOptionalService(s.Name) {
				level = levelOK
			}
			findings = append(findings, finding{level, s.Name, "scaled to zero", s.Note})
		case s.Health == "UNREACHABLE":
			findings = append(findings, finding{levelFail, s.Name,
				fmt.Sprintf("%s replicas but the health endpoint is unreachable", s.Replicas),
				"check 'sc svc logs " + s.Name + "' and 'sc svc pods " + s.Name + "'"})
		default:
			findings = append(findings, finding{levelFail, s.Name,
				fmt.Sprintf("health is %s (%s replicas)", s.Health, s.Replicas),
				"run 'sc svc health " + s.Name + "' for per-component detail"})
		}
	}

	// Is the operator plane usable at all? /env standing in for the rest.
	if _, err := sclient.ActiveProfiles(ctx, "orca"); err != nil {
		findings = append(findings, finding{levelWarn, "actuator",
			"actuator endpoints beyond /health are not exposed",
			"'sc svc env/loggers/metrics/threads' need them; run 'sc svc actuator-config' for the configuration"})
	} else {
		findings = append(findings, finding{levelOK, "actuator", "operator-plane endpoints are exposed", ""})
	}
	return findings
}

func isOptionalService(name string) bool {
	for _, s := range scsvc.Catalog {
		if s.Name == name {
			return s.OptionalByDefault
		}
	}
	return false
}

// diagnoseExecution explains why one execution failed.
func diagnoseExecution(ctx context.Context, execID string) ([]finding, error) {
	client, err := getGate()
	if err != nil {
		return nil, err
	}
	exec, err := client.GetExecution(ctx, execID)
	if err != nil {
		return nil, err
	}

	status := str(exec, "status")
	findings := []finding{{
		Level:   levelOK,
		Area:    "execution",
		Message: fmt.Sprintf("%s/%s is %s (%s)", str(exec, "application"), str(exec, "name"), status, execDuration(exec)),
	}}
	if gate.IsFailureStatus(status) {
		findings[0].Level = levelFail
	}

	failedStages := 0
	for _, r := range stageRows(exec, true) {
		if !gate.IsFailureStatus(r.Status) {
			continue
		}
		failedStages++
		findings = append(findings, finding{
			Level:   levelFail,
			Area:    "stage/" + strings.TrimSpace(strings.TrimPrefix(r.Name, "↳")),
			Message: fmt.Sprintf("%s after %s: %s", r.Status, r.Duration, dash(r.Message)),
			Hint:    "sc exec restart-stage " + execID + " " + strings.TrimSpace(strings.TrimPrefix(r.Name, "↳ ")),
		})
	}
	if failedStages == 0 && gate.IsFailureStatus(status) {
		findings = append(findings, finding{levelWarn, "stages",
			"the execution failed but no stage reports a failure",
			"this is the signature of a cancellation or of orca losing the execution; check 'sc exec get " + execID + "'"})
	}

	// A stuck or paused execution is usually waiting on a human.
	for _, s := range mapList(listField(exec, "stages")) {
		if str(s, "type") == "manualJudgment" && str(s, "status") == gate.StatusRunning {
			findings = append(findings, finding{levelWarn, "judgment",
				fmt.Sprintf("stage %q is waiting on a manual judgment", str(s, "name")),
				"sc judge continue " + execID + " " + fmt.Sprintf("%q", str(s, "name"))})
		}
	}

	// When a stage failed, the service that runs it is worth checking.
	if failedStages > 0 {
		sclient, serr := getSvc()
		if serr == nil && sclient.HasKube() {
			for _, name := range []string{"orca", "clouddriver"} {
				health, err := sclient.Health(ctx, name)
				if err != nil {
					findings = append(findings, finding{levelWarn, name, "health is unreachable: " + err.Error(), ""})
					continue
				}
				st := fmt.Sprint(health["status"])
				level := levelOK
				if st != "UP" {
					level = levelFail
				}
				findings = append(findings, finding{level, name, "reports " + st, ""})
			}
		}
	}
	return findings, nil
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show sc and Spinnaker versions",
	Long:  "Shows this CLI's version and, when a context is configured, the installation's Spinnaker version and per-service image versions.",
	RunE: func(cmd *cobra.Command, args []string) error {
		info := map[string]any{
			"sc": map[string]any{"version": Version, "commit": Commit, "built": BuildDate},
		}
		ctx, cancel := cmdContext(cmd.Context())
		defer cancel()

		var wg sync.WaitGroup
		var mu sync.Mutex
		wg.Add(2)
		go func() {
			defer wg.Done()
			client, err := getGate()
			if err != nil {
				return
			}
			v, err := client.Version(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			info["spinnaker"] = v["version"]
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			sclient, err := getSvc()
			if err != nil || !sclient.HasKube() {
				return
			}
			statuses, err := sclient.List(ctx)
			if err != nil {
				return
			}
			versions := map[string]string{}
			for _, s := range statuses {
				if s.Version != "" {
					versions[s.Name] = s.Version
				}
			}
			mu.Lock()
			if len(versions) > 0 {
				info["services"] = versions
			}
			mu.Unlock()
		}()
		wg.Wait()

		if outputIsStructured() {
			return render(info)
		}
		fmt.Printf("sc %s (commit %s, built %s)\n", Version, Commit, BuildDate)
		if v, ok := info["spinnaker"]; ok {
			fmt.Printf("Spinnaker %v\n", v)
		}
		if versions, ok := info["services"].(map[string]string); ok {
			fmt.Println()
			t := newTable("SERVICE", "IMAGE VERSION")
			for _, name := range sortedKeys(versions) {
				t.add(name, versions[name])
			}
			t.print("")
		}
		return nil
	},
}

func init() {
	doctorCmd.Flags().StringVar(&doctorExecution, "execution", "", "diagnose this execution instead of the installation")
	rootCmd.AddCommand(healthCmd, doctorCmd, versionCmd)
}
