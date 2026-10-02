package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// configPath returns the config file sc reads and writes.
func configPath() (string, error) {
	if cfgFile != "" {
		return cfgFile, nil
	}
	if used := viper.ConfigFileUsed(); used != "" {
		return used, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ConfigName+".yaml"), nil
}

// writeConfig persists viper's current state at 0600, since it holds credentials.
func writeConfig() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := viper.WriteConfigAs(path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("tightening permissions on %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// login / logout
// ---------------------------------------------------------------------------

var (
	loginContextName string
	loginNamespace   string
	loginKubeconfig  string
	loginNoVerify    bool
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate against a Spinnaker installation and save the context",
	Long: `Saves Gate credentials to the config file and makes them the current context.

The endpoint must include Gate's servlet context path. The upstream kustomize
install mounts Gate under /api/v1 behind a single ingress, so the endpoint is
typically http://<host>/api/v1 rather than http://<host>:8084.

Credentials are verified against /auth/user unless --no-verify is passed; that
call also reports the Fiat roles and accounts the identity can reach.`,
	GroupID: GroupConfig,
	Example: `  sc login --gate http://spinnaker.example.com/api/v1 --user admin --password secret
  sc login --gate https://spinnaker.example.com/api/v1 --token spk_abc123
  sc login --gate https://spinnaker.example.com/api/v1 --cert client.crt --key client.key
  sc login --gate http://localhost:8084 --user admin --password secret --context local`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// login creates the context it names, so a name that does not exist yet is
		// expected; an existing one is updated in place.
		cc := resolveContextForWrite(loginContextName)
		if cc.Gate == "" {
			return fmt.Errorf("--gate is required")
		}
		if cc.User == "" && cc.Token == "" && cc.Cert == "" {
			return fmt.Errorf("one of --user/--password, --token, or --cert/--key is required")
		}

		name := loginContextName
		if name == "" {
			name = "default"
		}

		if !loginNoVerify {
			client, err := gateClientFrom(cc)
			if err != nil {
				return err
			}
			ctx, cancel := cmdContext(cmd.Context())
			defer cancel()
			user, err := client.AuthUser(ctx)
			if err != nil {
				return fmt.Errorf("verifying credentials: %w", err)
			}
			version, verr := client.Version(ctx)
			who, _ := user["username"].(string)
			if who == "" {
				who = "(anonymous)"
			}
			line := fmt.Sprintf("Authenticated as %s", who)
			if verr == nil {
				if v, ok := version["version"].(string); ok {
					line += fmt.Sprintf(" against Spinnaker %s", v)
				}
			}
			fmt.Fprintln(os.Stderr, line)
			if roles, ok := user["roles"].([]any); ok && len(roles) > 0 {
				strs := make([]string, 0, len(roles))
				for _, r := range roles {
					strs = append(strs, fmt.Sprint(r))
				}
				fmt.Fprintf(os.Stderr, "Roles: %s\n", strings.Join(strs, ", "))
			}
		}

		entry := map[string]any{"gate": cc.Gate}
		if cc.User != "" {
			entry["user"] = cc.User
		}
		if cc.Password != "" {
			entry["password"] = cc.Password
		}
		if cc.Token != "" {
			entry["token"] = cc.Token
		}
		if cc.Cert != "" {
			entry["cert"] = cc.Cert
		}
		if cc.Key != "" {
			entry["key"] = cc.Key
		}
		if cc.Insecure {
			entry["insecure"] = true
		}
		ns := loginNamespace
		if ns == "" {
			ns = cc.Namespace
		}
		if ns != "" {
			entry["namespace"] = ns
		}
		if loginKubeconfig != "" {
			entry["kubeconfig"] = loginKubeconfig
		} else if cc.Kubeconfig != "" {
			entry["kubeconfig"] = cc.Kubeconfig
		}

		viper.Set("contexts."+name, entry)
		viper.Set("current-context", name)
		if err := writeConfig(); err != nil {
			return err
		}
		path, _ := configPath()
		fmt.Fprintf(os.Stderr, "Saved context %q to %s\n", name, path)
		return nil
	},
}

var logoutCmd = &cobra.Command{
	Use:     "logout",
	Short:   "Remove the stored credentials for a context",
	GroupID: GroupConfig,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := contextOverride
		if name == "" {
			name = viper.GetString("current-context")
		}
		if name == "" {
			return fmt.Errorf("no context selected; nothing to do")
		}
		contexts := viper.GetStringMap("contexts")
		raw, ok := contexts[name]
		if !ok {
			return fmt.Errorf("context %q not found", name)
		}
		entry, _ := raw.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		// Keep the endpoint so the context stays usable after re-authenticating.
		for _, secret := range []string{"password", "token", "cert", "key"} {
			delete(entry, secret)
		}
		viper.Set("contexts."+name, entry)
		if err := writeConfig(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Cleared credentials for context %q\n", name)
		return nil
	},
}

// ---------------------------------------------------------------------------
// context
// ---------------------------------------------------------------------------

var contextCmd = &cobra.Command{
	Use:     "context",
	Short:   "Manage Spinnaker installation contexts",
	GroupID: GroupConfig,
}

var contextListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List configured contexts",
	RunE: func(cmd *cobra.Command, args []string) error {
		contexts := viper.GetStringMap("contexts")
		current := viper.GetString("current-context")
		names := make([]string, 0, len(contexts))
		for n := range contexts {
			names = append(names, n)
		}
		sort.Strings(names)

		type row struct {
			Name      string `json:"name" yaml:"name"`
			Current   bool   `json:"current" yaml:"current"`
			Gate      string `json:"gate" yaml:"gate"`
			Auth      string `json:"auth" yaml:"auth"`
			Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
		}
		rows := make([]row, 0, len(names))
		for _, n := range names {
			m, _ := contexts[n].(map[string]any)
			r := row{Name: n, Current: n == current, Gate: stringField(m, "gate"), Namespace: stringField(m, "namespace")}
			switch {
			case stringField(m, "token") != "":
				r.Auth = "token"
			case stringField(m, "cert") != "":
				r.Auth = "x509"
			case stringField(m, "user") != "":
				r.Auth = "basic (" + stringField(m, "user") + ")"
			default:
				r.Auth = "none"
			}
			rows = append(rows, r)
		}

		if outputIsStructured() {
			return render(rows)
		}
		if len(rows) == 0 {
			fmt.Fprintln(os.Stderr, "No contexts configured. Run 'sc login --gate <url> --user <user> --password <pw>'.")
			return nil
		}
		w := getOutput()
		tw := w.Table()
		fmt.Fprintln(tw, "CURRENT\tNAME\tGATE\tAUTH\tNAMESPACE")
		for _, r := range rows {
			mark := ""
			if r.Current {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", mark, r.Name, r.Gate, r.Auth, r.Namespace)
		}
		w.FlushTable(tw)
		return nil
	},
}

var contextUseCmd = &cobra.Command{
	Use:   "use [name]",
	Short: "Switch the current context",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		contexts := viper.GetStringMap("contexts")
		if _, ok := contexts[args[0]]; !ok {
			return fmt.Errorf("context %q not found; run 'sc context list'", args[0])
		}
		viper.Set("current-context", args[0])
		if err := writeConfig(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Switched to context %q\n", args[0])
		return nil
	},
	ValidArgsFunction: completeContexts,
}

var contextSetCmd = &cobra.Command{
	Use:   "set [name]",
	Short: "Create or update a context",
	Long:  "Creates or updates a context from the global connection flags. Only the flags you pass are changed.",
	Args:  cobra.ExactArgs(1),
	Example: `  sc context set prod --gate https://spinnaker.example.com/api/v1 --token spk_abc
  sc context set lab --gate http://lab/api/v1 --user admin --password secret --namespace spinnaker`,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		contexts := viper.GetStringMap("contexts")
		entry, _ := contexts[name].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		set := func(key, value string) {
			if value != "" {
				entry[key] = value
			}
		}
		set("gate", viper.GetString("gate"))
		set("user", viper.GetString("user"))
		set("password", viper.GetString("password"))
		set("token", viper.GetString("token"))
		set("cert", certFile)
		set("key", keyFile)
		set("namespace", viper.GetString("namespace"))
		set("kubeconfig", viper.GetString("kubeconfig"))
		set("kube-context", kubeContext)
		if insecure {
			entry["insecure"] = true
		}
		if stringField(entry, "gate") == "" {
			return fmt.Errorf("--gate is required when creating a context")
		}
		viper.Set("contexts."+name, entry)
		if viper.GetString("current-context") == "" {
			viper.Set("current-context", name)
		}
		if err := writeConfig(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Saved context %q\n", name)
		return nil
	},
}

var contextDeleteCmd = &cobra.Command{
	Use:     "delete [name]",
	Aliases: []string{"rm"},
	Short:   "Delete a context",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		contexts := viper.GetStringMap("contexts")
		if _, ok := contexts[args[0]]; !ok {
			return fmt.Errorf("context %q not found", args[0])
		}
		// viper has no delete, so the map is rebuilt without the entry.
		remaining := map[string]any{}
		for k, v := range contexts {
			if k != args[0] {
				remaining[k] = v
			}
		}
		viper.Set("contexts", remaining)
		if viper.GetString("current-context") == args[0] {
			viper.Set("current-context", "")
		}
		if err := writeConfig(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Deleted context %q\n", args[0])
		return nil
	},
	ValidArgsFunction: completeContexts,
}

var configCmd = &cobra.Command{
	Use:     "config",
	Short:   "Inspect sc configuration",
	GroupID: GroupConfig,
}

var configViewCmd = &cobra.Command{
	Use:   "view",
	Short: "Show the effective configuration with secrets redacted",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := configPath()
		contexts := viper.GetStringMap("contexts")
		redacted := map[string]any{}
		for name, raw := range contexts {
			m, _ := raw.(map[string]any)
			copied := map[string]any{}
			for k, v := range m {
				if k == "password" || k == "token" {
					copied[k] = "***"
					continue
				}
				copied[k] = v
			}
			redacted[name] = copied
		}
		view := map[string]any{
			"configFile":     path,
			"currentContext": viper.GetString("current-context"),
			"contexts":       redacted,
		}
		if outputIsStructured() {
			return render(view)
		}
		return getOutput().PrintYAML(view)
	},
}

var configPathCmd = &cobra.Command{
	Use:   "path",
	Short: "Print the config file path",
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := configPath()
		if err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	},
}

// completeContexts completes configured context names.
func completeContexts(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	contexts := viper.GetStringMap("contexts")
	names := make([]string, 0, len(contexts))
	for n := range contexts {
		if strings.HasPrefix(n, toComplete) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

func init() {
	loginCmd.Flags().StringVar(&loginContextName, "context-name", "", "name to save this context under (default: \"default\")")
	loginCmd.Flags().StringVar(&loginNamespace, "save-namespace", "", "Kubernetes namespace to record for the operator plane")
	loginCmd.Flags().StringVar(&loginKubeconfig, "save-kubeconfig", "", "kubeconfig path to record for the operator plane")
	loginCmd.Flags().BoolVar(&loginNoVerify, "no-verify", false, "save without verifying the credentials against Gate")

	contextCmd.AddCommand(contextListCmd, contextUseCmd, contextSetCmd, contextDeleteCmd)
	configCmd.AddCommand(configViewCmd, configPathCmd)
	rootCmd.AddCommand(loginCmd, logoutCmd, contextCmd, configCmd)
}
