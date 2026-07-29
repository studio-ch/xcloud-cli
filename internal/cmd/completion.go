package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newCompletionCommand replaces cobra's generated default only to carry
// per-shell installation instructions. Users who have to search the web
// for where to put the script mostly do not bother, and completion is
// the single cheapest usability win a CLI with this many subcommands
// has.
func newCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completion <bash|zsh|fish|powershell>",
		Short:     "Generate a shell completion script",
		Args:      cobra.ExactValidArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		Long: `Generate a shell completion script.

Homebrew installs completions automatically; the instructions below are for
manual installs.

bash:
  # once, if bash-completion is not installed:
  #   Linux: sudo apt install bash-completion   macOS: brew install bash-completion
  xcloud completion bash > /etc/bash_completion.d/xcloud          # system-wide
  xcloud completion bash > ~/.local/share/bash-completion/completions/xcloud

zsh:
  # if completion is not yet enabled, add to ~/.zshrc:  autoload -U compinit; compinit
  xcloud completion zsh > "${fpath[1]}/_xcloud"
  # then restart the shell

fish:
  xcloud completion fish > ~/.config/fish/completions/xcloud.fish

powershell:
  xcloud completion powershell | Out-String | Invoke-Expression
  # to persist, append that line to $PROFILE
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(out, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(out)
			case "fish":
				return cmd.Root().GenFishCompletion(out, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(out)
			default:
				return &usageError{fmt.Errorf("unsupported shell %q", args[0])}
			}
		},
	}
}
