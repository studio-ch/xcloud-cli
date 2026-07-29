package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/xcloud-cli/internal/api"
	"github.com/studio-ch/xcloud-cli/internal/buildinfo"
)

type healthInfo struct {
	Status  string  `json:"status"`
	Uptime  float64 `json:"uptime"`
	Version string  `json:"version"`
}

func newVersionCommand(s *State) *cobra.Command {
	var clientOnly bool

	c := &cobra.Command{
		Use:   "version",
		Short: "Show the client version, and the version of the API it talks to",
		Long: "Reports the CLI's own build and, unless --client is given, probes the\n" +
			"configured API's health endpoint. That endpoint is unauthenticated, so\n" +
			"this works before 'xcloud auth login'.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "client   %s\n", buildinfo.String())
			if clientOnly {
				return nil
			}

			r, err := s.Resolved()
			if err != nil {
				// A broken config must not stop `version` from telling
				// the user what binary they are running.
				fmt.Fprintf(cmd.ErrOrStderr(), "server   unavailable (%v)\n", err)
				return nil
			}

			health, herr := fetchHealth(cmd.Context(), r.APIURL, s.timeout)
			if herr != nil {
				fmt.Fprintf(out, "server   unreachable at %s (%v)\n", r.APIURL, herr)
				return nil
			}

			// APP_VERSION is not set on the deployed api container yet,
			// so the endpoint reports "0.0.0". Printing that verbatim
			// would look like a real version; say what it is instead.
			version := health.Version
			if version == "" || version == "0.0.0" {
				version = "unknown"
			}
			fmt.Fprintf(out, "server   %s   %s   (%s, up %s)\n",
				version, r.APIURL, health.Status, formatUptime(health.Uptime))
			fmt.Fprintln(out, "api      /v1   (additive-only; see docs/public-api.md)")
			return nil
		},
	}
	c.Flags().BoolVar(&clientOnly, "client", false, "print only the client version, without contacting the API")
	return c
}

// fetchHealth probes GET /v1/healthz. It builds its own unauthenticated
// client: `xcloud version` must work with no credential configured, and
// the health endpoint is mounted before the auth middleware.
func fetchHealth(ctx context.Context, origin string, timeout time.Duration) (*healthInfo, error) {
	c, err := api.New(api.Options{Origin: origin, Timeout: timeout, AllowInsecure: true})
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/healthz"})
	if err != nil {
		return nil, err
	}
	var h healthInfo
	if err := json.Unmarshal(resp.Body, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

func formatUptime(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
