// Package cmd builds the CLI's command tree.
//
// Layout convention: one file per domain. Every command obtains its
// dependencies from the shared *State, which resolves configuration and
// constructs the API client lazily — so `cloudconsole version` and
// `cloudconsole completion` work on a machine that has never been configured.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/config"
	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
	"github.com/studio-ch/cloudconsole-cli/internal/output"
)

// State is the per-invocation context shared by every command.
type State struct {
	// Global flag values.
	profile    string
	apiURL     string
	token      string
	outputFlag string
	noColor    bool
	quiet      bool
	noHeaders  bool
	debug      bool
	timeout    time.Duration

	resolved *config.Resolved
	client   *api.Client
	writer   *output.Writer

	// Injection points, defaulted to the real process streams by
	// Execute. Rendering has to go somewhere testable: cobra's SetOut
	// only redirects cobra's own output, not ours, so without these the
	// only observable part of a command would be its exit code.
	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader
	// confirm asks the user to approve a destructive action. Overridden
	// in tests; nil means "derive from the terminal".
	confirm func(prompt string) (bool, error)
}

func (s *State) out() io.Writer {
	if s.stdout != nil {
		return s.stdout
	}
	return os.Stdout
}

func (s *State) errOut() io.Writer {
	if s.stderr != nil {
		return s.stderr
	}
	return os.Stderr
}

func (s *State) stdoutIsProcessStdout() bool {
	return s.stdout == nil || s.stdout == io.Writer(os.Stdout)
}

// Confirm asks for approval of a destructive action.
//
// Refusing when there is no terminal — rather than assuming yes or
// assuming no and proceeding — is deliberate: a CI job that means to
// delete something has to say so with --yes, and one that does not mean
// to gets a clear error instead of a destroyed disk.
func (s *State) Confirm(prompt string) (bool, error) {
	if s.confirm != nil {
		return s.confirm(prompt)
	}
	if !output.IsTTY(os.Stdin) {
		return false, &usageError{errors.New(
			"refusing to perform a destructive action without confirmation; " +
				"pass --yes when running non-interactively")}
	}
	fmt.Fprint(s.errOut(), prompt)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// Resolved returns the effective configuration, loading it on first use.
func (s *State) Resolved() (*config.Resolved, error) {
	if s.resolved != nil {
		return s.resolved, nil
	}
	file, path, err := config.Load()
	if err != nil {
		return nil, &configError{err}
	}
	r, err := config.Resolve(file, config.Overrides{
		Profile: s.profile,
		APIURL:  s.apiURL,
		Token:   s.token,
		Output:  s.outputFlag,
	})
	if err != nil {
		return nil, &configError{err}
	}
	r.Path = path
	s.resolved = r
	return r, nil
}

// Client returns the API client, constructing it on first use.
//
// A missing credential is reported here rather than as a 401 three
// round-trips later, and the message names the two ways to supply one.
func (s *State) Client() (*api.Client, error) {
	if s.client != nil {
		return s.client, nil
	}
	r, err := s.Resolved()
	if err != nil {
		return nil, err
	}
	if r.Token == "" {
		return nil, &configError{fmt.Errorf(
			"no API token configured for profile %q.\n"+
				"Run 'cloudconsole auth login' to store one, or set CLOUDCONSOLE_API_TOKEN.\n"+
				"Issue a key in the panel under Settings → API keys.", r.ProfileName)}
	}

	var tracer *api.Tracer
	if s.debug || os.Getenv("CLOUDCONSOLE_DEBUG") == "1" {
		tracer = api.NewTracer(os.Stderr, r.Token)
	}

	c, err := api.New(api.Options{
		Origin:        r.APIURL,
		Token:         r.Token,
		Timeout:       s.timeout,
		Tracer:        tracer,
		AllowInsecure: os.Getenv("CLOUDCONSOLE_ALLOW_INSECURE") == "1",
	})
	if err != nil {
		return nil, &configError{err}
	}
	s.client = c
	return c, nil
}

// Writer returns the configured renderer.
func (s *State) Writer() (*output.Writer, error) {
	if s.writer != nil {
		return s.writer, nil
	}
	r, err := s.Resolved()
	if err != nil {
		return nil, err
	}
	format, err := output.ParseFormat(r.Output)
	if err != nil {
		return nil, &usageError{err}
	}
	// Only the real process stdout can be a terminal. A test buffer or a
	// redirect never is, and asking os.Stdout in that case would enable
	// colour for output nobody is looking at.
	isTTY := s.stdoutIsProcessStdout() && output.IsTTY(os.Stdout)
	s.writer = &output.Writer{
		Out:       s.out(),
		Err:       s.errOut(),
		Format:    format,
		Color:     !output.NoColor(s.noColor, isTTY),
		Quiet:     s.quiet,
		NoHeaders: s.noHeaders,
	}
	return s.writer, nil
}

// Execute builds and runs the command tree, returning the process exit
// code. main() does nothing but call this and os.Exit — a single exit
// site makes the code contract testable.
func Execute(ctx context.Context) exitcode.Code {
	s := &State{stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin}
	root := newRootCommand(s)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)

	if err := root.ExecuteContext(ctx); err != nil {
		return report(os.Stderr, err, s)
	}
	return exitcode.OK
}

func newRootCommand(s *State) *cobra.Command {
	root := &cobra.Command{
		Use:   "cloudconsole",
		Short: "Command-line interface for the Cloud Console",
		Long: "cloudconsole manages Cloud Console resources from the terminal and from CI.\n\n" +
			"Authenticate once with 'cloudconsole auth login' using an API key issued in the\n" +
			"panel under Settings → API keys, or set CLOUDCONSOLE_API_TOKEN in a CI job.\n\n" +
			"An API key is bound to a single organisation, so working with several\n" +
			"organisations means several profiles (see 'cloudconsole config --help').",
		SilenceUsage:  true, // usage on a runtime error is noise
		SilenceErrors: true, // we render errors ourselves, in report()
		// A root command with subcommands but no Run falls back to
		// printing help and exiting 0 — including for a mistyped
		// subcommand. That silently turns `cloudconsole instnce list` into a
		// success, which is exactly the kind of thing a CI pipeline
		// should fail on. Handle both cases explicitly instead.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return &usageError{fmt.Errorf(
					"unknown command %q — run 'cloudconsole --help' to see the available commands", args[0])}
			}
			return cmd.Help()
		},
	}

	f := root.PersistentFlags()
	f.StringVar(&s.profile, "profile", "", "configuration profile to use")
	f.StringVar(&s.apiURL, "api-url", "", "API origin (default https://api.cloud.flow.swiss)")
	f.StringVar(&s.token, "token", "", "API token (prefer CLOUDCONSOLE_API_TOKEN; a flag is visible in ps)")
	f.StringVarP(&s.outputFlag, "output", "o", "", "output format: table, wide, json, yaml")
	f.BoolVar(&s.noColor, "no-color", false, "disable coloured output")
	f.BoolVarP(&s.quiet, "quiet", "q", false, "print only identifying values")
	f.BoolVar(&s.noHeaders, "no-headers", false, "omit the table header row")
	f.BoolVar(&s.debug, "debug", false, "trace HTTP requests to stderr (credentials redacted)")
	f.DurationVar(&s.timeout, "timeout", 0, "per-request timeout (default 30s)")

	root.AddCommand(
		// Meta.
		newVersionCommand(s),
		newAuthCommand(s),
		newConfigCommand(s),
		newCompletionCommand(),
		newExitCodesCommand(),

		// Xcloud stack. The CLI covers the Xcloud compute stack only —
		// the classic Incus stack is deliberately out of scope.
		newInstanceCommand(s),
		newVolumeCommand(s),
		newImageCommand(s),
		newNetworkCommand(s),
		newSecurityGroupCommand(s),
		newElasticIPCommand(s),

		// Catalog and account.
		newRegionCommand(s),
		newQuotaCommand(s),
		newFlavorCommand(s),
		newSSHKeyCommand(s),
		newAPIKeyCommand(s),
	)
	return root
}

// newExitCodesCommand makes the exit-code contract discoverable from the
// binary, not only from the docs — the docs are not installed alongside
// it, and the contract is what CI branches on.
func newExitCodesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "exit-codes",
		Short: "Show what each exit code means",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "cloudconsole exit codes (stable since v0.1.0):")
			fmt.Fprintln(out)
			for _, c := range exitcode.All {
				fmt.Fprintf(out, "  %3d  %s\n", c.Int(), c.Description())
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "  >128  terminated by signal (code minus 128)")
			return nil
		},
	}
}
