package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/xcloud-cli/internal/api"
	"github.com/studio-ch/xcloud-cli/internal/output"
)

// simpleGet is the shape most read-only commands take: one GET, render
// the body through the configured columns.
func simpleGet(s *State, path string, query url.Values, columns func(time.Time) []output.Column) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		client, err := s.Client()
		if err != nil {
			return err
		}
		w, err := s.Writer()
		if err != nil {
			return err
		}
		resp, err := client.Do(cmd.Context(), api.RequestOptions{
			Method: http.MethodGet, Path: path, Query: query,
		})
		if err != nil {
			return err
		}
		return w.Render(resp.Body, columns(time.Now()))
	}
}

func newRegionCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "region",
		Aliases: []string{"regions"},
		Short:   "List regions available to your organisation",
	}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List regions",
		Args:    cobra.NoArgs,
		RunE: simpleGet(s, "/v1/regions", nil, func(time.Time) []output.Column {
			return []output.Column{
				{Header: "slug", Value: func(r map[string]any) string { return output.Field(r, "slug") }},
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "status", Value: func(r map[string]any) string { return output.Field(r, "status") }},
				{Header: "arch", Value: func(r map[string]any) string { return output.Field(r, "architecture") }},
				{Header: "services", Value: regionServicesCell},
				{Header: "id", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "id") }},
			}
		}),
	})
	return c
}

// regionServicesCell renders which compute stacks a region offers. It
// matters because a region can serve one stack and not the other, and
// "no suitable region" is otherwise a confusing failure.
func regionServicesCell(r map[string]any) string {
	services, ok := r["services"].(map[string]any)
	if !ok {
		return ""
	}
	var enabled []string
	for _, name := range []string{"compute", "xcloud"} {
		if on, _ := services[name].(bool); on {
			enabled = append(enabled, name)
		}
	}
	return strings.Join(enabled, ",")
}

func newQuotaCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "quota",
		Aliases: []string{"quotas"},
		Short:   "Show quota limits and current usage",
	}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List quotas",
		Long: "Lists every quota with its effective limit and current usage.\n\n" +
			"A limit of -1 means unlimited. Exceeding a hard quota makes the API reject\n" +
			"the request with exit code 7.",
		Args: cobra.NoArgs,
		RunE: simpleGet(s, "/v1/me/quotas", nil, func(time.Time) []output.Column {
			return []output.Column{
				{Header: "quota", Value: func(r map[string]any) string { return output.Field(r, "key") }},
				{Header: "usage", Value: func(r map[string]any) string { return output.Field(r, "usage") }},
				{Header: "limit", Value: quotaLimitCell},
				{Header: "available", Value: quotaAvailableCell},
				{Header: "severity", Value: func(r map[string]any) string { return output.Field(r, "severity") }},
				{Header: "default", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "defaultValue") }},
				{Header: "override", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "overrideValue") }},
			}
		}),
	})
	return c
}

// quotaLimitCell renders the sentinel -1 as "unlimited". Printing "-1"
// in a limit column reads like a bug.
func quotaLimitCell(r map[string]any) string {
	if output.Field(r, "effectiveValue") == "-1" {
		return "unlimited"
	}
	return output.Field(r, "effectiveValue")
}

func quotaAvailableCell(r map[string]any) string {
	if output.Field(r, "effectiveValue") == "-1" {
		return "unlimited"
	}
	return output.Field(r, "available")
}

func newFlavorCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "flavor",
		Aliases: []string{"flavors"},
		Short:   "List Xcloud instance flavors",
	}
	c.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List flavors",
		Args:    cobra.NoArgs,
		RunE: simpleGet(s, "/v1/xcloud/flavors", nil, func(time.Time) []output.Column {
			return []output.Column{
				{Header: "slug", Value: func(r map[string]any) string { return output.Field(r, "slug") }},
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "cpu", Value: func(r map[string]any) string { return output.Field(r, "cpuCores") }},
				{Header: "memory", Value: func(r map[string]any) string { return gib(r, "memoryGib") }},
				{Header: "disk", Value: func(r map[string]any) string { return gib(r, "diskGib") }},
			}
		}),
	})
	return c
}

// newAPIKeyCommand is list-only by necessity: every mutating handler in
// apps/api/src/routes/v1/api-keys.ts requires a user session, so a key
// cannot mint or revoke another key. Issuing and revoking stay panel
// operations. Listing is genuinely useful though — it answers "which of
// my keys is this, and can it write?".
func newAPIKeyCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "api-key",
		Aliases: []string{"api-keys"},
		Short:   "List the API keys of your organisation",
		Long: "Lists API keys. Creating and revoking keys requires a signed-in user and\n" +
			"is done in the panel under Settings → API keys.\n\n" +
			"'xcloud auth status' identifies which of these keys the CLI is using.",
	}
	c.AddCommand(&cobra.Command{
		Use: "list", Aliases: []string{"ls"},
		Short: "List API keys", Args: cobra.NoArgs,
		RunE: simpleGet(s, "/v1/api-keys", nil, func(now time.Time) []output.Column {
			return []output.Column{
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "prefix", Value: func(r map[string]any) string { return output.Field(r, "prefix") }},
				{Header: "access", Value: func(r map[string]any) string { return output.Field(r, "access") }},
				{Header: "state", Value: apiKeyStateCell},
				{Header: "expires", Value: func(r map[string]any) string { return dateOnly(output.Field(r, "expiresAt")) }},
				{Header: "last used", Value: func(r map[string]any) string { return output.Age(output.Field(r, "lastUsedAt"), now) }},
				{Header: "id", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "id") }},
			}
		}),
	})
	return c
}

// apiKeyStateCell collapses revoked/expired/active into one column. A key
// that silently stopped working is the thing a user is looking for here.
func apiKeyStateCell(r map[string]any) string {
	if output.Field(r, "revokedAt") != "" {
		return "revoked"
	}
	if output.Field(r, "expired") == "yes" {
		return "expired"
	}
	return "active"
}

// dateOnly trims an RFC 3339 timestamp to its date. An expiry is a
// calendar fact; the time of day is noise in a table.
func dateOnly(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func newSSHKeyCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "ssh-key",
		Aliases: []string{"ssh-keys"},
		Short:   "Manage SSH public keys",
		Long: "SSH public keys are provisioned into instances when they are created,\n" +
			"via 'xcloud instance create --ssh-key <id>'.",
	}
	c.AddCommand(
		&cobra.Command{
			Use:     "list",
			Aliases: []string{"ls"},
			Short:   "List SSH keys",
			Args:    cobra.NoArgs,
			RunE: simpleGet(s, "/v1/ssh-keys", nil, func(now time.Time) []output.Column {
				return []output.Column{
					{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
					{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
					{Header: "fingerprint", Value: func(r map[string]any) string { return output.Field(r, "fingerprint") }},
					{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},
					{Header: "publicKey", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "publicKey") }},
				}
			}),
		},
		newSSHKeyCreateCommand(s),
		newSSHKeyDeleteCommand(s),
	)
	return c
}

func newSSHKeyCreateCommand(s *State) *cobra.Command {
	var (
		name    string
		keyFile string
		keyBody string
	)

	c := &cobra.Command{
		Use:   "create",
		Short: "Add an SSH public key",
		Long: "Add an SSH public key.\n\n" +
			"Reading the key from a file is the usual path — pasting a key on the command\n" +
			"line works but puts it in your shell history.",
		Example: "  xcloud ssh-key create --name laptop --public-key-file ~/.ssh/id_ed25519.pub",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (keyFile == "") == (keyBody == "") {
				return &usageError{fmt.Errorf("pass exactly one of --public-key-file or --public-key")}
			}
			material := keyBody
			if keyFile != "" {
				raw, err := os.ReadFile(keyFile)
				if err != nil {
					return &usageError{fmt.Errorf("read %s: %w", keyFile, err)}
				}
				material = string(raw)
			}
			material = strings.TrimSpace(material)

			// Catch the classic mistake locally rather than letting the
			// API reject it: a private key must never leave the machine,
			// so this check is worth more than the round-trip it saves.
			if strings.Contains(material, "PRIVATE KEY") {
				return &usageError{fmt.Errorf(
					"that is a PRIVATE key — pass the matching .pub file instead. " +
						"The private key must never leave your machine")}
			}

			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"name": name, "publicKey": material})
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/ssh-keys", Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, []output.Column{
				{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "fingerprint", Value: func(r map[string]any) string { return output.Field(r, "fingerprint") }},
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "a label for the key (required)")
	c.Flags().StringVar(&keyFile, "public-key-file", "", "path to a .pub file")
	c.Flags().StringVar(&keyBody, "public-key", "", "the public key material itself")
	_ = c.MarkFlagRequired("name")
	return c
}

func newSSHKeyDeleteCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Remove an SSH key",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete, Path: "/v1/ssh-keys/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Removed SSH key %s.\n", args[0])
			}
			return nil
		},
	}
}
