package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate a shell completion script",
	Long: `Generates a shell completion script.

Completion is dynamic where it helps: application, pipeline, account and service
names are fetched from the installation as you type.

  bash:  source <(sc completion bash)
  zsh:   source <(sc completion zsh)
  fish:  sc completion fish > ~/.config/fish/completions/sc.fish
  pwsh:  sc completion powershell | Out-String | Invoke-Expression`,
	Args:      cobra.ExactValidArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	GroupID:   GroupConfig,
	RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return rootCmd.GenBashCompletionV2(os.Stdout, true)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(completionCmd)
}
