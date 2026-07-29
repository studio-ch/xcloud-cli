package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/xcloud-cli/internal/api"
	"github.com/studio-ch/xcloud-cli/internal/output"
)

func volumeColumns(now time.Time) []output.Column {
	return []output.Column{
		{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
		{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
		{Header: "size", Value: func(r map[string]any) string { return gib(r, "sizeGib") }},
		{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
		{Header: "attached to", Value: volumeAttachmentCell},
		{Header: "device", Value: func(r map[string]any) string { return output.Field(r, "device") }},
		{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},
	}
}

// volumeAttachmentCell renders the attachment as something actionable.
// Whether a volume is free is the question this list exists to answer —
// a detached volume can be attached, a deleted one must be detached
// first — so it gets its own column rather than hiding in wide output.
func volumeAttachmentCell(r map[string]any) string {
	if name := output.Field(r, "attachedInstanceName"); name != "" {
		return name
	}
	if id := output.Field(r, "attachedInstanceId"); id != "" {
		return id
	}
	return "(detached)"
}

func newVolumeCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "volume",
		Aliases: []string{"volumes"},
		Short:   "Manage Xcloud data volumes",
		Long: "Data volumes are independent of instances: they survive an instance being\n" +
			"deleted, and can be moved between instances by detaching and re-attaching.\n\n" +
			"Unlike instance operations, volume operations are synchronous — there is no\n" +
			"worker involved, so there is nothing to --wait for.",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List data volumes", Args: cobra.NoArgs,
			RunE: simpleGet(s, "/v1/xcloud/volumes", nil, volumeColumns),
		},
		&cobra.Command{
			Use: "get <id>", Short: "Show one data volume", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return renderGet(cmd, s, "/v1/xcloud/volumes/"+args[0], volumeColumns)
			},
		},
		newVolumeCreateCommand(s),
		newVolumeResizeCommand(s),
		newVolumeAttachCommand(s),
		newVolumeDetachCommand(s),
		newVolumeDeleteCommand(s),
	)
	return c
}

// renderGet is the single-resource counterpart to simpleGet.
func renderGet(cmd *cobra.Command, s *State, path string, columns func(time.Time) []output.Column) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	w, err := s.Writer()
	if err != nil {
		return err
	}
	resp, err := client.Do(cmd.Context(), api.RequestOptions{Method: http.MethodGet, Path: path})
	if err != nil {
		return err
	}
	return w.Render(resp.Body, columns(time.Now()))
}

func newVolumeCreateCommand(s *State) *cobra.Command {
	var (
		name     string
		region   string
		size     int
		attachTo string
	)
	c := &cobra.Command{
		Use:     "create",
		Short:   "Create a data volume",
		Example: "  xcloud volume create --name scratch --region ZRH1 --size 500",
		Args:    cobra.NoArgs,
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
			body := map[string]any{"regionId": regionID, "name": name, "sizeGib": size}
			if attachTo != "" {
				body["attachToInstanceId"] = attachTo
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/volumes", Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, volumeColumns(time.Now()))
		},
	}
	c.Flags().StringVar(&name, "name", "", "volume name (required)")
	c.Flags().StringVar(&region, "region", "", "region slug or id (required)")
	c.Flags().IntVar(&size, "size", 0, "size in GiB (required)")
	c.Flags().StringVar(&attachTo, "attach-to", "", "attach to this instance id immediately")
	for _, r := range []string{"name", "region", "size"} {
		_ = c.MarkFlagRequired(r)
	}
	return c
}

func newVolumeResizeCommand(s *State) *cobra.Command {
	var size int
	c := &cobra.Command{
		Use:   "resize <id>",
		Short: "Grow a data volume",
		Long:  "Grow a data volume. Volumes cannot shrink; the API rejects a smaller size.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"sizeGib": size})
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/volumes/" + args[0] + "/resize", Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, volumeColumns(time.Now()))
		},
	}
	c.Flags().IntVar(&size, "size", 0, "new size in GiB (required, grow only)")
	_ = c.MarkFlagRequired("size")
	return c
}

func newVolumeAttachCommand(s *State) *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "attach <volume-id>",
		Short: "Attach a volume to an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"volumeId": args[0]})
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost,
				Path:   "/v1/xcloud/instances/" + instance + "/volumes",
				Body:   payload,
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Attached volume %s to instance %s.\n", args[0], instance)
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "instance id to attach to (required)")
	_ = c.MarkFlagRequired("instance")
	return c
}

func newVolumeDetachCommand(s *State) *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "detach <volume-id>",
		Short: "Detach a volume from an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete,
				Path:   "/v1/xcloud/instances/" + instance + "/volumes/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Detached volume %s from instance %s.\n", args[0], instance)
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "instance id to detach from (required)")
	_ = c.MarkFlagRequired("instance")
	return c
}

func newVolumeDeleteCommand(s *State) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete a data volume",
		Long:    "Delete a data volume. It must be detached first.",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Delete volume %s and its data? This cannot be undone. [y/N]: ", args[0]))
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
				Method: http.MethodDelete, Path: "/v1/xcloud/volumes/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Deleted volume %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}

func newElasticIPCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "elastic-ip",
		Aliases: []string{"elastic-ips", "eip"},
		Short:   "Manage Xcloud elastic IPs",
	}
	c.AddCommand(
		&cobra.Command{
			Use: "list", Aliases: []string{"ls"},
			Short: "List elastic IPs", Args: cobra.NoArgs,
			RunE: simpleGet(s, "/v1/xcloud/elastic-ips", nil, func(now time.Time) []output.Column {
				return []output.Column{
					{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
					{Header: "address", Value: func(r map[string]any) string { return output.Field(r, "address") }},
					{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
					{Header: "attached to", Value: eipTargetCell},
					{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},
				}
			}),
		},
		newElasticIPAllocateCommand(s),
		newElasticIPTargetCommand(s),
		newElasticIPReleaseCommand(s),
	)
	return c
}

func eipTargetCell(r map[string]any) string {
	if name := output.Field(r, "instanceName"); name != "" {
		return name
	}
	if id := output.Field(r, "instanceId"); id != "" {
		return id
	}
	return "(unattached)"
}

func newElasticIPAllocateCommand(s *State) *cobra.Command {
	var region, instance string
	c := &cobra.Command{
		Use:   "allocate",
		Short: "Allocate a new elastic IP",
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
			body := map[string]any{"regionId": regionID}
			if instance != "" {
				body["instanceId"] = instance
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/elastic-ips", Body: payload,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, func(now time.Time) []output.Column {
				return []output.Column{
					{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
					{Header: "address", Value: func(r map[string]any) string { return output.Field(r, "address") }},
					{Header: "attached to", Value: eipTargetCell},
				}
			}(time.Now()))
		},
	}
	c.Flags().StringVar(&region, "region", "", "region slug or id (required)")
	c.Flags().StringVar(&instance, "instance", "", "attach to this instance immediately")
	_ = c.MarkFlagRequired("region")
	return c
}

func newElasticIPTargetCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:   "target",
		Short: "Attach or detach an elastic IP",
	}

	var instance string
	set := &cobra.Command{
		Use:   "set <eip-id>",
		Short: "Point an elastic IP at an instance",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return patchEIPTarget(cmd, s, args[0], instance)
		},
	}
	set.Flags().StringVar(&instance, "instance", "", "instance id (required)")
	_ = set.MarkFlagRequired("instance")

	clear := &cobra.Command{
		Use:   "clear <eip-id>",
		Short: "Detach an elastic IP, keeping the address allocated",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return patchEIPTarget(cmd, s, args[0], "")
		},
	}

	c.AddCommand(set, clear)
	return c
}

// patchEIPTarget sends the target change. An empty instance id detaches:
// the API models "no target" as an explicit null rather than an omitted
// field, so the key has to be present either way.
func patchEIPTarget(cmd *cobra.Command, s *State, eipID, instanceID string) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	var target any
	if instanceID != "" {
		target = instanceID
	}
	payload, err := json.Marshal(map[string]any{"instanceId": target})
	if err != nil {
		return err
	}
	if _, err := client.Do(cmd.Context(), api.RequestOptions{
		Method: http.MethodPatch,
		Path:   "/v1/xcloud/elastic-ips/" + eipID + "/target",
		Body:   payload,
	}); err != nil {
		return err
	}
	w, _ := s.Writer()
	if w != nil && !w.Quiet {
		if instanceID == "" {
			fmt.Fprintf(w.Err, "Detached elastic IP %s.\n", eipID)
		} else {
			fmt.Fprintf(w.Err, "Elastic IP %s now points at instance %s.\n", eipID, instanceID)
		}
	}
	return nil
}

func newElasticIPReleaseCommand(s *State) *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "release <id>",
		Short: "Release an elastic IP",
		Long: "Release an elastic IP back to the pool. The address is lost — you will not\n" +
			"get the same one back. Use 'target clear' to detach without releasing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Release elastic IP %s? The address cannot be reclaimed. [y/N]: ", args[0]))
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
				Method: http.MethodDelete, Path: "/v1/xcloud/elastic-ips/" + args[0],
			}); err != nil {
				return err
			}
			w, _ := s.Writer()
			if w != nil && !w.Quiet {
				fmt.Fprintf(w.Err, "Released elastic IP %s.\n", args[0])
			}
			return nil
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return c
}
