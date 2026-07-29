package cmd

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/config"
)

func newConfigCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage profiles and settings",
		Long: "Configuration lives in a YAML file (see 'cloudconsole config path'), created with\n" +
			"0600 permissions because it holds an API token.\n\n" +
			"Every setting is resolved per field, in this order:\n" +
			"  1. a command-line flag        --api-url, --output, --profile\n" +
			"  2. an environment variable    CLOUDCONSOLE_API_URL, CLOUDCONSOLE_API_TOKEN, CLOUDCONSOLE_OUTPUT, CLOUDCONSOLE_PROFILE\n" +
			"  3. the selected profile\n" +
			"  4. the built-in default\n\n" +
			"Per field, not per source — so a token from the environment combines with a\n" +
			"URL from the profile, which is the usual CI arrangement.",
	}
	c.AddCommand(
		newConfigListCommand(s),
		newConfigUseCommand(s),
		newConfigPathCommand(),
		newConfigExplainCommand(s),
	)
	return c
}

func newConfigListCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List configured profiles",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, path, err := config.Load()
			if err != nil {
				return &configError{err}
			}
			out := cmd.OutOrStdout()
			if len(file.Profiles) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"No profiles configured (%s).\nRun 'cloudconsole auth login' to create one.\n", path)
				return nil
			}
			active, _ := s.Resolved()

			names := make([]string, 0, len(file.Profiles))
			for n := range file.Profiles {
				names = append(names, n)
			}
			sort.Strings(names)

			for _, n := range names {
				p := file.Profiles[n]
				marker := "  "
				if active != nil && active.ProfileName == n {
					marker = "* "
				}
				url := p.APIURL
				if url == "" {
					url = config.DefaultAPIURL
				}
				fmt.Fprintf(out, "%s%-12s %s", marker, n, url)
				if p.TenantHint != "" {
					fmt.Fprintf(out, "   %s", p.TenantHint)
				}
				switch {
				case p.Token != "":
					fmt.Fprintf(out, "   [%s]", p.KeyPrefix)
				case p.TokenCommand != "":
					fmt.Fprint(out, "   [token_command]")
				default:
					fmt.Fprint(out, "   [no credential]")
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
}

func newConfigUseCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:   "use <profile>",
		Short: "Set the default profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _, err := config.Load()
			if err != nil {
				return &configError{err}
			}
			if _, ok := file.Profiles[args[0]]; !ok {
				return &usageError{fmt.Errorf(
					"profile %q does not exist — 'cloudconsole config list' shows the configured ones", args[0])}
			}
			file.CurrentProfile = args[0]
			if _, err := config.Save(file); err != nil {
				return &configError{err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Default profile is now %q.\n", args[0])
			return nil
		},
	}
}

func newConfigPathCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file path",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return &configError{err}
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
}

// newConfigExplainCommand answers "why is this command talking to that
// endpoint?" — the question a per-field precedence chain inevitably
// raises, and one that is otherwise answered by guesswork.
func newConfigExplainCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:   "explain",
		Short: "Show each resolved setting and where it came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := s.Resolved()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%-10s %-40s %s\n", "SETTING", "VALUE", "SOURCE")
			fmt.Fprintf(out, "%-10s %-40s %s\n", "profile", r.ProfileName, r.ProfileFrom)
			fmt.Fprintf(out, "%-10s %-40s %s\n", "api_url", r.APIURL, r.APIURLFrom)
			token := "(none)"
			if r.Token != "" {
				token = maskToken(r.Token)
			}
			fmt.Fprintf(out, "%-10s %-40s %s\n", "token", token, r.TokenFrom)
			fmt.Fprintf(out, "%-10s %-40s %s\n", "output", r.Output, r.OutputFrom)
			fmt.Fprintf(out, "\nconfig file: %s\n", r.Path)
			return nil
		},
	}
}
