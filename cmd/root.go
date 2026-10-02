package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avkcode/spinnaker-cli/pkg/gate"
	"github.com/avkcode/spinnaker-cli/pkg/output"
	"github.com/avkcode/spinnaker-cli/pkg/svc"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Exit codes. Scripts can branch on these without parsing messages.
const (
	ExitSuccess       = 0
	ExitAuthError     = 1
	ExitNetworkError  = 2
	ExitTimeout       = 3
	ExitConfigError   = 4
	ExitNotFound      = 5
	ExitConflict      = 6
	ExitUsageError    = 7
	ExitExecFailed    = 8 // a pipeline execution or task finished in a failed state
	ExitInternalError = 99
)

// Command groups for help output.
const (
	GroupCore     = "Core Commands"
	GroupOperator = "Operator Plane"
	GroupConfig   = "Configuration & Profiles"
)

// ConfigName is the config file base name under $HOME.
const ConfigName = ".spinnaker-cli"

var (
	cfgFile         string
	gateEndpoint    string
	gateUser        string
	gatePassword    string
	gateToken       string
	certFile        string
	keyFile         string
	outputFmt       string
	insecure        bool
	globalTimeout   time.Duration
	dryRun          bool
	logLevel        string
	contextOverride string

	// Operator-plane flags.
	namespace   string
	kubeconfig  string
	kubeContext string
	serviceURLs []string

	// Version is set at build time via -ldflags.
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "sc",
	Short: "Spinnaker CLI",
	Long: `sc is a high-performance CLI for Spinnaker.

It covers two planes:

  Core      the Gate API — applications, pipelines, executions, tasks,
            infrastructure, judgments. What a user or a pipeline does.
  Operator  the services behind Gate — orca, clouddriver, front50, igor, echo,
            fiat, rosco, kayenta, keel — via their actuator endpoints and the
            Kubernetes objects running them. What an operator does.

Both planes have an escape hatch (sc api / sc svc api), so anything this CLI has
not wrapped is still one command away.`,
	Version:           Version,
	SilenceUsage:      true,
	SilenceErrors:     true,
	CompletionOptions: cobra.CompletionOptions{HiddenDefaultCmd: true},
}

// Execute runs the root command and translates errors into exit codes.
func Execute() {
	err := rootCmd.Execute()
	auditInvocation(os.Args, err)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(classifyError(err))
	}
}

// ExecFailedError marks a command that completed its API work correctly but whose
// subject (an execution, a task) ended in a failed state. It exits non-zero so CI
// notices, without being confused for a CLI malfunction.
type ExecFailedError struct{ Msg string }

func (e *ExecFailedError) Error() string { return e.Msg }

// classifyError maps an error to an exit code.
func classifyError(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var execFailed *ExecFailedError
	if errors.As(err, &execFailed) {
		return ExitExecFailed
	}
	var apiErr *gate.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == 401 || apiErr.Status == 403:
			return ExitAuthError
		case apiErr.Status == 404:
			return ExitNotFound
		case apiErr.Status == 409:
			return ExitConflict
		case apiErr.Status >= 500:
			return ExitInternalError
		default:
			return ExitUsageError
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case containsAny(msg, "not authenticated", "unauthorized", "401", "403", "no gate endpoint", "credentials"):
		return ExitAuthError
	case containsAny(msg, "connection refused", "no such host", "dial tcp", "i/o timeout", "tls", "certificate"):
		return ExitNetworkError
	case containsAny(msg, "deadline exceeded", "timed out", "context canceled"):
		return ExitTimeout
	case containsAny(msg, "not found", "404"):
		return ExitNotFound
	case containsAny(msg, "already exists", "conflict", "409"):
		return ExitConflict
	case containsAny(msg, "kubeconfig", "no kubernetes config", "context "):
		return ExitConfigError
	case containsAny(msg, "invalid", "required", "must be", "usage", "unknown"):
		return ExitUsageError
	default:
		return ExitInternalError
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func init() {
	cobra.OnInitialize(initConfig)

	rootCmd.SetVersionTemplate(fmt.Sprintf("sc version %s (commit: %s, built: %s)\n", Version, Commit, BuildDate))

	pf := rootCmd.PersistentFlags()
	pf.StringVar(&cfgFile, "config", "", "config file (default is $HOME/"+ConfigName+".yaml)")
	pf.StringVar(&gateEndpoint, "gate", "", "Gate API base URL, including any context path (e.g. http://spinnaker.example.com/api/v1)")
	pf.StringVarP(&gateUser, "user", "u", "", "Gate username (basic auth)")
	pf.StringVarP(&gatePassword, "password", "p", "", "Gate password (basic auth)")
	pf.StringVarP(&gateToken, "token", "t", "", "Gate API token (spk_…) or OAuth2 bearer token")
	pf.StringVar(&certFile, "cert", "", "client certificate for x509 auth")
	pf.StringVar(&keyFile, "key", "", "client key for x509 auth")
	pf.StringVarP(&outputFmt, "output", "o", "table", "Output format: table, json, or yaml")
	pf.BoolVarP(&insecure, "insecure", "k", false, "Skip TLS certificate verification")
	pf.DurationVar(&globalTimeout, "timeout", 60*time.Second, "Timeout for commands (0 = no timeout)")
	pf.BoolVar(&dryRun, "dry-run", false, "Preview changes without applying them")
	pf.StringVar(&logLevel, "log-level", "info", "Log level: debug, info, warn, error")
	pf.StringVar(&contextOverride, "context", "", "sc context to use for this command (overrides current-context)")

	pf.StringVarP(&namespace, "namespace", "n", "", "Kubernetes namespace holding Spinnaker (default: spinnaker)")
	pf.StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig for the operator plane (default: $KUBECONFIG, in-cluster, then ~/.kube/config)")
	pf.StringVar(&kubeContext, "kube-context", "", "kubeconfig context to use")
	pf.StringArrayVar(&serviceURLs, "service-url", nil, "reach a service directly instead of via the cluster: service=url (repeatable)")

	for _, bind := range []string{"gate", "user", "password", "token", "output", "insecure", "timeout", "namespace", "kubeconfig"} {
		_ = viper.BindPFlag(bind, pf.Lookup(bind))
	}

	rootCmd.AddGroup(
		&cobra.Group{ID: GroupCore, Title: GroupCore},
		&cobra.Group{ID: GroupOperator, Title: GroupOperator},
		&cobra.Group{ID: GroupConfig, Title: GroupConfig},
	)
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		cobra.CheckErr(err)
		viper.AddConfigPath(home)
		viper.SetConfigType("yaml")
		viper.SetConfigName(ConfigName)
	}

	// SPINNAKER_GATE, SPINNAKER_USER, SPINNAKER_TOKEN, …
	viper.SetEnvPrefix("SPINNAKER")
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		validateConfig()
	} else if cfgFile != "" {
		fmt.Fprintf(os.Stderr, "Warning: could not read config file: %v\n", err)
	}
}

func validateConfig() {
	contexts := viper.GetStringMap("contexts")
	for name, v := range contexts {
		c, ok := v.(map[string]any)
		if !ok {
			fmt.Fprintf(os.Stderr, "Warning: context %q is not a valid map, ignoring\n", name)
			continue
		}
		if _, ok := c["gate"]; !ok {
			fmt.Fprintf(os.Stderr, "Warning: context %q is missing the required 'gate' field\n", name)
		}
	}
	if current := viper.GetString("current-context"); current != "" {
		if _, ok := contexts[current]; !ok {
			fmt.Fprintf(os.Stderr, "Warning: current-context %q is not defined under contexts\n", current)
		}
	}
}

// contextConfig is one named target installation.
type contextConfig struct {
	Gate        string
	User        string
	Password    string
	Token       string
	Cert        string
	Key         string
	Insecure    bool
	Namespace   string
	Kubeconfig  string
	KubeContext string
}

// resolveContext merges the named context (or current-context) with flag and
// environment overrides. Flags win over the context, which wins over defaults.
// A named context that does not exist is an error.
func resolveContext(name string) (contextConfig, error) {
	return resolveContextNamed(name, true)
}

// resolveContextForWrite is resolveContext for commands that create a context
// (login, context set), where the name need not exist yet.
func resolveContextForWrite(name string) contextConfig {
	cc, _ := resolveContextNamed(name, false)
	return cc
}

func resolveContextNamed(name string, mustExist bool) (contextConfig, error) {
	explicit := name
	if name == "" {
		name = viper.GetString("current-context")
	}

	var cc contextConfig
	if name != "" {
		contexts := viper.GetStringMap("contexts")
		raw, ok := contexts[name]
		if !ok {
			if explicit != "" && mustExist {
				return cc, fmt.Errorf("context %q not found; run 'sc context list'", name)
			}
		} else if m, ok := raw.(map[string]any); ok {
			cc.Gate = stringField(m, "gate")
			cc.User = stringField(m, "user")
			cc.Password = stringField(m, "password")
			cc.Token = stringField(m, "token")
			cc.Cert = stringField(m, "cert")
			cc.Key = stringField(m, "key")
			cc.Insecure = boolField(m, "insecure")
			cc.Namespace = stringField(m, "namespace")
			cc.Kubeconfig = stringField(m, "kubeconfig")
			cc.KubeContext = stringField(m, "kube-context")
		}
	}

	// Overrides, in increasing precedence: viper (config root + env), then flags.
	if v := viper.GetString("gate"); v != "" {
		cc.Gate = v
	}
	if v := viper.GetString("user"); v != "" {
		cc.User = v
	}
	if v := viper.GetString("password"); v != "" {
		cc.Password = v
	}
	if v := viper.GetString("token"); v != "" {
		cc.Token = v
	}
	if certFile != "" {
		cc.Cert = certFile
	}
	if keyFile != "" {
		cc.Key = keyFile
	}
	if insecure {
		cc.Insecure = true
	}
	if v := viper.GetString("namespace"); v != "" {
		cc.Namespace = v
	}
	if v := viper.GetString("kubeconfig"); v != "" {
		cc.Kubeconfig = v
	}
	if kubeContext != "" {
		cc.KubeContext = kubeContext
	}
	if cc.Namespace == "" {
		cc.Namespace = "spinnaker"
	}
	return cc, nil
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func boolField(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// getGate builds a Gate client for the active context.
func getGate() (*gate.Client, error) {
	return getGateWithContext(contextOverride)
}

func getGateWithContext(ctxName string) (*gate.Client, error) {
	cc, err := resolveContext(ctxName)
	if err != nil {
		return nil, err
	}
	return gateClientFrom(cc)
}

// gateClientFrom builds a Gate client from an already-resolved context, so that
// login can verify credentials for a context it has not saved yet.
func gateClientFrom(cc contextConfig) (*gate.Client, error) {
	if cc.Gate == "" {
		return nil, errors.New("no Gate endpoint set. Run 'sc login --gate <url> --user <user> --password <pw>' " +
			"or 'sc context set <name> --gate <url>'")
	}
	return gate.New(gate.Config{
		Endpoint:  cc.Gate,
		User:      cc.User,
		Password:  cc.Password,
		Token:     cc.Token,
		CertFile:  cc.Cert,
		KeyFile:   cc.Key,
		Insecure:  cc.Insecure,
		Timeout:   viper.GetDuration("timeout"),
		UserAgent: "spinnaker-cli/" + Version,
	})
}

// getSvc builds an operator-plane client for the active context.
func getSvc() (*svc.Client, error) {
	cc, err := resolveContext(contextOverride)
	if err != nil {
		return nil, err
	}
	urls, err := parseServiceURLs(serviceURLs)
	if err != nil {
		return nil, err
	}
	return svc.New(svc.Config{
		Namespace:   cc.Namespace,
		Kubeconfig:  cc.Kubeconfig,
		KubeContext: cc.KubeContext,
		ServiceURLs: urls,
		Timeout:     viper.GetDuration("timeout"),
		// Gate authenticates its own actuator endpoints, so the operator plane
		// reaches it through the context's Gate endpoint rather than the cluster.
		GateURL:      cc.Gate,
		GateUser:     cc.User,
		GatePassword: cc.Password,
		GateToken:    cc.Token,
		GateInsecure: cc.Insecure,
	}), nil
}

// parseServiceURLs turns repeated service=url flags into a map.
func parseServiceURLs(in []string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, entry := range in {
		name, url, ok := strings.Cut(entry, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("invalid --service-url %q: expected service=url", entry)
		}
		if _, err := svc.Lookup(name); err != nil {
			return nil, err
		}
		out[strings.ToLower(name)] = url
	}
	return out, nil
}

// cmdContext returns a context carrying the global timeout.
func cmdContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := viper.GetDuration("timeout")
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func getOutput() *output.Writer {
	return output.NewWriter(os.Stdout, viper.GetString("output"))
}

// outputIsStructured reports whether the format is machine readable.
func outputIsStructured() bool {
	f := viper.GetString("output")
	return f == "json" || f == "yaml"
}

// render prints v as JSON or YAML according to --output.
func render(v any) error {
	if viper.GetString("output") == "yaml" {
		return getOutput().PrintYAML(v)
	}
	return getOutput().PrintJSON(v)
}

// debugf prints to stderr when --log-level=debug.
func debugf(format string, args ...any) {
	if logLevel == "debug" {
		fmt.Fprintf(os.Stderr, "[debug] "+format+"\n", args...)
	}
}

func isDryRun() bool { return dryRun }

func dryRunMsg(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[dry-run] "+format+"\n", args...)
}

// audit appends a timestamped entry to ~/.spinnaker-cli-audit.log. Spinnaker
// writes are high-consequence, so there is a local who/what/when trail
// independent of the installation's own event stream.
func audit(action, detail string) {
	if os.Getenv("SC_NO_AUDIT") != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, ConfigName+"-audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [%s] %s - %s\n", time.Now().UTC().Format(time.RFC3339), os.Getenv("USER"), action, detail)
}

// auditInvocation records each invocation with secrets redacted.
func auditInvocation(args []string, err error) {
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	ctxName := contextOverride
	if ctxName == "" {
		ctxName = viper.GetString("current-context")
	}
	if ctxName == "" {
		ctxName = "-"
	}
	audit("invocation", fmt.Sprintf("context=%s cmd=[%s] result=%s", ctxName, redactArgs(args), result))
}

// secretFlags are the flags whose values must never reach the audit log.
var secretFlags = map[string]bool{
	"--password": true, "-p": true,
	"--token": true, "-t": true,
}

// redactArgs renders a command line with secret flag values masked.
func redactArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	parts := []string{filepath.Base(args[0])}
	redactNext := false
	for _, a := range args[1:] {
		switch {
		case redactNext:
			parts = append(parts, "***")
			redactNext = false
		case secretFlags[a]:
			parts = append(parts, a)
			redactNext = true
		default:
			if name, _, ok := strings.Cut(a, "="); ok && secretFlags[name] {
				parts = append(parts, name+"=***")
				continue
			}
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " ")
}
