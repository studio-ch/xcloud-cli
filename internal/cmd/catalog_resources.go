package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/output"
)

func newImageCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "image",
		Aliases: []string{"images"},
		Short:   "Manage the Xcloud image catalog",
		Long: "Lists the images an instance can boot: the shared cluster catalog plus any\n" +
			"OCI images your organisation has registered.\n\n" +
			"Images are built elsewhere (for example with the Packer plugin) and pushed to\n" +
			"a registry; 'register' only adds an existing image to the catalog.",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List images", Args: cobra.NoArgs,
			RunE: simpleGet(s, "/v1/xcloud/images", nil, func(now time.Time) []output.Column {
				return []output.Column{
					{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
					{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
					{Header: "platform", Value: func(r map[string]any) string { return output.Field(r, "platform") }},
					{Header: "scope", Value: imageScopeCell},
					{Header: "reference", Value: func(r map[string]any) string { return output.Field(r, "reference") }},
					{Header: "regionId", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "regionId") }},
					{Header: "labels", Wide: true, Value: func(r map[string]any) string { return compactMap(r["labels"]) }},
				}
			}),
		},
		newImageRegisterCommand(s),
		newImageDeleteCommand(s),
	)
	return c
}

// imageScopeCell distinguishes the shared catalog from the tenant's own
// images. It is the difference between an image you can delete and one
// you cannot, so it is worth a column.
func imageScopeCell(r map[string]any) string {
	if ns := output.Field(r, "namespace"); ns == "default" {
		return "shared"
	}
	return "organisation"
}

// compactMap renders a labels object as k=v pairs on one line.
func compactMap(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	out := ""
	for k, val := range m {
		if out != "" {
			out += ","
		}
		out += fmt.Sprintf("%s=%s", k, output.Str(val))
	}
	return out
}

func newImageRegisterCommand(s *State) *cobra.Command {
	var (
		name, region, reference, platform string
	)
	c := &cobra.Command{
		Use:   "register",
		Short: "Register an OCI image in your organisation's catalog",
		Example: "  cloudconsole image register --name macos-sequoia --region ZRH1 \\\n" +
			"      --reference ghcr.io/example/macos-sequoia:latest --platform macos",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			regionID, err := resolveRegion(cmd.Context(), client, region)
			if err != nil {
				return err
			}
			body := map[string]any{"regionId": regionID, "name": name, "reference": reference}
			if platform != "" {
				body["platform"] = platform
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/images", Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, []output.Column{
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
				{Header: "reference", Value: func(r map[string]any) string { return output.Field(r, "reference") }},
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "catalog name for the image (required)")
	c.Flags().StringVar(&region, "region", "", "region slug or id (required)")
	c.Flags().StringVar(&reference, "reference", "", "OCI reference, e.g. ghcr.io/org/image:tag (required)")
	c.Flags().StringVar(&platform, "platform", "", "macos or linux")
	for _, r := range []string{"name", "region", "reference"} {
		_ = c.MarkFlagRequired(r)
	}
	return c
}

func newImageDeleteCommand(s *State) *cobra.Command {
	var region string
	var yes bool
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Remove an image from your organisation's catalog",
		Long: "Removes the catalog entry. The image itself stays in its registry, and\n" +
			"shared cluster images cannot be removed this way.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			regionID, err := resolveRegion(cmd.Context(), client, region)
			if err != nil {
				return err
			}
			if !yes {
				ok, cerr := s.Confirm(fmt.Sprintf("Remove image %q from the catalog? [y/N]: ", args[0]))
				if cerr != nil {
					return cerr
				}
				if !ok {
					w, _ := s.Writer()
					if w != nil {
						fmt.Fprintln(w.Err, "Aborted.")
					}
					return nil
				}
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete,
				Path:   "/v1/xcloud/images/" + regionID + "/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Removed image %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().StringVar(&region, "region", "", "region slug or id (required)")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	_ = c.MarkFlagRequired("region")
	return c
}

func newNetworkCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "network",
		Aliases: []string{"networks"},
		Short:   "Manage Xcloud networks",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List networks", Args: cobra.NoArgs,
			RunE: simpleGet(s, "/v1/xcloud/networks", nil, func(time.Time) []output.Column {
				return []output.Column{
					{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
					{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
					{Header: "mode", Value: func(r map[string]any) string { return output.Field(r, "mode") }},
					{Header: "cidr", Value: func(r map[string]any) string { return output.Field(r, "cidr") }},
					{Header: "gateway", Value: func(r map[string]any) string { return output.Field(r, "gateway") }},
					{Header: "dhcp", Value: func(r map[string]any) string { return output.Field(r, "dhcp") }},
				}
			}),
		},
		newNetworkCreateCommand(s),
		newNetworkDeleteCommand(s),
	)
	return c
}

func newNetworkCreateCommand(s *State) *cobra.Command {
	var (
		name, region, mode, cidr, gateway string
		dhcp                              bool
		idemKey                           string
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a network",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			regionID, err := resolveRegion(cmd.Context(), client, region)
			if err != nil {
				return err
			}
			body := map[string]any{"regionId": regionID, "name": name}
			if mode != "" {
				body["mode"] = mode
			}
			if cidr != "" {
				body["cidr"] = cidr
			}
			if gateway != "" {
				body["gateway"] = gateway
			}
			if cmd.Flags().Changed("dhcp") {
				body["dhcp"] = dhcp
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			if idemKey == "" {
				idemKey = api.NewIdempotencyKey()
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/networks",
				Body: payload, IdempotencyKey: idemKey,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, []output.Column{
				{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
				{Header: "mode", Value: func(r map[string]any) string { return output.Field(r, "mode") }},
				{Header: "cidr", Value: func(r map[string]any) string { return output.Field(r, "cidr") }},
			})
		},
	}
	c.Flags().StringVar(&name, "name", "", "network name (required)")
	c.Flags().StringVar(&region, "region", "", "region slug or id (required)")
	c.Flags().StringVar(&mode, "mode", "", "network mode")
	c.Flags().StringVar(&cidr, "cidr", "", "CIDR, e.g. 10.0.0.0/24")
	c.Flags().StringVar(&gateway, "gateway", "", "gateway address")
	c.Flags().BoolVar(&dhcp, "dhcp", false, "enable DHCP")
	c.Flags().StringVar(&idemKey, "idempotency-key", "", "reuse this key to safely retry (generated if unset)")
	for _, r := range []string{"name", "region"} {
		_ = c.MarkFlagRequired(r)
	}
	return c
}

func newNetworkDeleteCommand(s *State) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a network",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf("Delete network %q? [y/N]: ", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					w, _ := s.Writer()
					if w != nil {
						fmt.Fprintln(w.Err, "Aborted.")
					}
					return nil
				}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete, Path: "/v1/xcloud/networks/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Deleted network %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}

func newSecurityGroupCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "security-group",
		Aliases: []string{"security-groups", "sg"},
		Short:   "Manage Xcloud security groups",
		Long: "Security groups are per-region firewall rule sets. Attach them to an\n" +
			"instance with 'cloudconsole instance security-group set'.\n\n" +
			"Note that updating a group REPLACES its entire rule set — the API has no\n" +
			"add-one-rule operation, so read the group first if you mean to extend it.",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List security groups", Args: cobra.NoArgs,
			RunE: simpleGet(s, "/v1/xcloud-security-groups", nil, func(time.Time) []output.Column {
				return []output.Column{
					{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
					{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
					{Header: "rules", Value: func(r map[string]any) string { return countOf(r["rules"]) }},
					{Header: "labels", Wide: true, Value: func(r map[string]any) string { return compactMap(r["labels"]) }},
				}
			}),
		},
		&cobra.Command{
			Use: "get <name>", Short: "Show one security group and its rules", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return renderGet(cmd, s, "/v1/xcloud-security-groups/"+args[0], func(time.Time) []output.Column {
					return []output.Column{
						{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
						{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
						{Header: "rules", Value: func(r map[string]any) string { return countOf(r["rules"]) }},
					}
				})
			},
		},
		newSecurityGroupDeleteCommand(s),
	)
	return c
}

func countOf(v any) string {
	if list, ok := v.([]any); ok {
		return fmt.Sprintf("%d", len(list))
	}
	return "0"
}

func newSecurityGroupDeleteCommand(s *State) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a security group",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Delete security group %q? Instances using it lose those rules. [y/N]: ", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					w, _ := s.Writer()
					if w != nil {
						fmt.Fprintln(w.Err, "Aborted.")
					}
					return nil
				}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete, Path: "/v1/xcloud-security-groups/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Deleted security group %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}
