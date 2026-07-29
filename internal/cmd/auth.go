package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/config"
)

// tenantInfo is the subset of GET /v1/tenant the CLI renders.
//
// Hand-written rather than taken from the generated client: the API's
// OpenAPI document declares almost no named components, so every
// response is an anonymous inline struct with no nameable Go type. A
// small view struct per command keeps rendering readable and decoupled
// from codegen churn — a new server field cannot break it.
type tenantInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultRegion string `json:"defaultRegion"`
}

// apiKeyInfo is one row of GET /v1/api-keys.
type apiKeyInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Access     string     `json:"access"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	Expired    bool       `json:"expired"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

func newAuthCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:   "auth",
		Short: "Manage API credentials",
		Long: "Authentication uses an API key issued in the panel under Settings → API keys.\n\n" +
			"A key is bound to exactly one organisation — the server ignores any\n" +
			"organisation hint a client sends — so working with several organisations\n" +
			"means one profile per key, selected with --profile.",
	}
	c.AddCommand(
		newAuthLoginCommand(s),
		newAuthLogoutCommand(s),
		newAuthStatusCommand(s),
		newAuthWhoamiCommand(s),
	)
	return c
}

func newAuthLoginCommand(s *State) *cobra.Command {
	var tokenStdin bool

	c := &cobra.Command{
		Use:   "login",
		Short: "Store an API key in a profile",
		Long: "Prompts for an API key, verifies it against the API, and stores it in the\n" +
			"profile's configuration file with 0600 permissions.\n\n" +
			"For scripts, pipe the key in instead:\n" +
			"  op read op://Private/cloud/api-key | cloudconsole auth login --token-stdin\n\n" +
			"To avoid storing the secret at all, set 'token_command' in the profile and\n" +
			"the CLI will shell out for it on each invocation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			token, err := readToken(cmd, tokenStdin, s.token)
			if err != nil {
				return err
			}
			if !config.LooksLikeAPIKey(token) {
				return &usageError{fmt.Errorf(
					"that does not look like a Cloud Console API key " +
						"(expected it to start with sk_live_ or sk_test_ followed by at least 12 characters)")}
			}

			r, err := s.Resolved()
			if err != nil {
				return err
			}

			// Verify before storing. Writing a bad key to disk and
			// letting the user discover it on the next command would be
			// a poor trade for one round-trip.
			probe, err := api.New(api.Options{
				Origin: r.APIURL, Token: token, Timeout: s.timeout,
				AllowInsecure: os.Getenv("CLOUDCONSOLE_ALLOW_INSECURE") == "1",
			})
			if err != nil {
				return &configError{err}
			}
			tenant, err := fetchTenant(ctx, probe)
			if err != nil {
				return err
			}

			file, _, err := config.Load()
			if err != nil {
				return &configError{err}
			}
			if file.Profiles == nil {
				file.Profiles = map[string]*config.Profile{}
			}
			name := r.ProfileName

			// Refuse to silently repoint a profile at a different
			// organisation: someone who runs `auth login` in the wrong
			// terminal should not discover it by deleting the wrong VM.
			if existing := file.Profiles[name]; existing != nil &&
				existing.TenantID != "" && existing.TenantID != tenant.ID {
				return &usageError{fmt.Errorf(
					"profile %q is already bound to organisation %q (%s), but this key belongs to %q (%s).\n"+
						"Use a different profile:  cloudconsole auth login --profile <name>",
					name, existing.TenantHint, existing.TenantID, tenant.Name, tenant.ID)}
			}

			p := file.Profiles[name]
			if p == nil {
				p = &config.Profile{}
				file.Profiles[name] = p
			}
			p.Token = token
			p.APIURL = r.APIURL
			p.TenantID = tenant.ID
			p.TenantHint = tenant.Name
			p.KeyPrefix = config.ExtractKeyPrefix(token)
			if file.CurrentProfile == "" {
				file.CurrentProfile = name
			}

			path, err := config.Save(file)
			if err != nil {
				return &configError{err}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Signed in to %s (%s)\n", tenant.Name, tenant.ID)
			fmt.Fprintf(out, "Profile %q saved to %s\n", name, path)
			if key := findOwnKey(ctx, probe, token); key != nil {
				fmt.Fprintf(out, "Key %q — %s\n", key.Name, describeScopes(key))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the API key from stdin instead of prompting")
	return c
}

// readToken obtains the credential without ever putting it in argv.
//
// A --token flag exists on the root command for one-off use, but it is
// not the documented path: anything in argv is visible in `ps` output
// and lands in shell history.
func readToken(cmd *cobra.Command, fromStdin bool, flagToken string) (string, error) {
	if flagToken != "" {
		return strings.TrimSpace(flagToken), nil
	}
	if fromStdin {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read token from stdin: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", &usageError{fmt.Errorf(
			"stdin is not a terminal; pass --token-stdin to read the key from a pipe")}
	}
	fmt.Fprint(cmd.ErrOrStderr(), "API key: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func newAuthLogoutCommand(s *State) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored credential from a profile",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := s.Resolved()
			if err != nil {
				return err
			}
			file, _, err := config.Load()
			if err != nil {
				return &configError{err}
			}
			p := file.Profiles[r.ProfileName]
			if p == nil {
				return &usageError{fmt.Errorf("profile %q does not exist", r.ProfileName)}
			}
			if all {
				delete(file.Profiles, r.ProfileName)
				if file.CurrentProfile == r.ProfileName {
					file.CurrentProfile = ""
				}
			} else {
				// Keep api_url so `auth login` on the same profile does
				// not need the endpoint typed again.
				p.Token, p.TokenCommand, p.KeyPrefix = "", "", ""
				p.TenantID, p.TenantHint = "", ""
			}
			if _, err := config.Save(file); err != nil {
				return &configError{err}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the credential from profile %q.\n", r.ProfileName)
			// Worth stating plainly: a local logout leaves the key live.
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Note: this only removed the local copy. The key itself is still valid — revoke it in the panel under Settings → API keys.")
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "remove the whole profile, not just its credential")
	return c
}

func newAuthStatusCommand(s *State) *cobra.Command {
	var offline bool

	c := &cobra.Command{
		Use:   "status",
		Short: "Show the active profile, credential and organisation",
		Long: "Verifies the configured key against the API and reports which organisation\n" +
			"it belongs to, what it is allowed to do, and when it expires.\n\n" +
			"There is no /v1/me for an API key — it has no user context — so this reads\n" +
			"GET /v1/tenant and GET /v1/api-keys instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := s.Resolved()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			fmt.Fprintf(out, "Profile    %s", r.ProfileName)
			if r.Path != "" {
				fmt.Fprintf(out, "   (%s)", r.Path)
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "Endpoint   %s/v1   [%s]\n", r.APIURL, r.APIURLFrom)

			if r.Token == "" {
				fmt.Fprintln(out, "Credential none configured")
				fmt.Fprintln(out, "Status     not signed in")
				return &configError{fmt.Errorf(
					"no API token for profile %q — run 'cloudconsole auth login' or set CLOUDCONSOLE_API_TOKEN", r.ProfileName)}
			}
			fmt.Fprintf(out, "Credential %s   [%s]\n", maskToken(r.Token), r.TokenFrom)

			if offline {
				if r.TenantHint != "" {
					fmt.Fprintf(out, "Organisation %s (%s)   [cached]\n", r.TenantHint, r.TenantID)
				}
				fmt.Fprintln(out, "Status     not verified (--offline)")
				return nil
			}

			client, err := s.Client()
			if err != nil {
				return err
			}
			tenant, err := fetchTenant(ctx, client)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Organisation %s   (%s)\n", tenant.Name, tenant.ID)
			if tenant.DefaultRegion != "" {
				fmt.Fprintf(out, "Region     %s (default)\n", tenant.DefaultRegion)
			}

			if key := findOwnKey(ctx, client, r.Token); key != nil {
				fmt.Fprintf(out, "Key        %s   %s\n", key.Name, key.Prefix)
				fmt.Fprintf(out, "Access     %s\n", describeScopes(key))
				if key.ExpiresAt != nil {
					days := int(time.Until(*key.ExpiresAt).Hours() / 24)
					fmt.Fprintf(out, "Expires    %s   (%d days)\n", key.ExpiresAt.Format(time.DateOnly), days)
				} else {
					fmt.Fprintln(out, "Expires    never")
				}
				if key.LastUsedAt != nil {
					fmt.Fprintf(out, "Last used  %s\n", key.LastUsedAt.Format(time.RFC3339))
				}
			}
			fmt.Fprintln(out, "Status     ok")
			return nil
		},
	}
	c.Flags().BoolVar(&offline, "offline", false, "report cached values without contacting the API")
	return c
}

func newAuthWhoamiCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the organisation and key in one line",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			client, err := s.Client()
			if err != nil {
				return err
			}
			tenant, err := fetchTenant(ctx, client)
			if err != nil {
				return err
			}
			r, _ := s.Resolved()

			line := fmt.Sprintf("%s (%s)", tenant.Name, tenant.ID)
			if key := findOwnKey(ctx, client, r.Token); key != nil {
				line += fmt.Sprintf(" · key %q (%s) · %s", key.Name, key.Prefix, describeScopes(key))
			}
			fmt.Fprintln(cmd.OutOrStdout(), line)
			return nil
		},
	}
}

// fetchTenant is the canonical key-validation probe.
//
// GET /v1/tenant works for an API key (the router's write-scope guard
// lets every GET through, and only PATCH carries a user-context check),
// and a 200 proves the key is live, unrevoked and unexpired while also
// naming the organisation it is bound to.
//
// GET /v1/me would be the obvious choice and is the wrong one: it
// returns 403 "API keys have no user context" unconditionally, which
// would surface to the user as a permissions problem. And /v1/healthz is
// anonymous, so it returns 200 for a garbage token.
func fetchTenant(ctx context.Context, c *api.Client) (*tenantInfo, error) {
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/tenant"})
	if err != nil {
		return nil, err
	}
	var t tenantInfo
	if err := json.Unmarshal(resp.Body, &t); err != nil {
		return nil, fmt.Errorf("could not read the organisation from the API response: %w", err)
	}
	return &t, nil
}

// findOwnKey identifies which of the tenant's keys is the one in use, by
// deriving the public lookup prefix locally and matching it against the
// list. Answering "is my key read-only?" without this would mean asking
// the user to compare strings by eye.
//
// Best effort: a failure here degrades the output rather than the
// command, since the key already proved itself in fetchTenant.
func findOwnKey(ctx context.Context, c *api.Client, token string) *apiKeyInfo {
	prefix := config.ExtractKeyPrefix(token)
	if prefix == "" {
		return nil
	}
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/api-keys"})
	if err != nil {
		return nil
	}
	var envelope struct {
		Data []apiKeyInfo `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return nil
	}
	for i := range envelope.Data {
		if envelope.Data[i].Prefix == prefix {
			return &envelope.Data[i]
		}
	}
	return nil
}

func describeScopes(k *apiKeyInfo) string {
	if k.RevokedAt != nil {
		return "REVOKED"
	}
	if k.Expired {
		return "EXPIRED"
	}
	for _, s := range k.Scopes {
		if s == "write:resources" {
			return "read + write"
		}
	}
	if len(k.Scopes) == 0 {
		return k.Access
	}
	return "read only"
}

// maskToken shows the public lookup prefix and nothing else. The prefix
// is safe to display — the server stores it in plaintext and the panel
// shows it — and it is exactly enough to tell two keys apart.
func maskToken(token string) string {
	prefix := config.ExtractKeyPrefix(token)
	if prefix == "" {
		return "sk_***"
	}
	return fmt.Sprintf("%s… (%d chars)", prefix, len(token))
}
