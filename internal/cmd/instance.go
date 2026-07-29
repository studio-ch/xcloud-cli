package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/xcloud-cli/internal/api"
	"github.com/studio-ch/xcloud-cli/internal/output"
	"github.com/studio-ch/xcloud-cli/internal/wait"
)

// instanceColumns is the table view. Deliberately narrow: an 80-column
// terminal is still the common case in CI logs, and everything else is
// one --output wide or --output json away.
func instanceColumns(now time.Time) []output.Column {
	return []output.Column{
		{Header: "id", Value: func(r map[string]any) string { return output.Field(r, "id") }},
		{Header: "name", Value: func(r map[string]any) string { return output.Field(r, "name") }},
		{Header: "status", Value: instanceStatusCell},
		{Header: "region", Value: func(r map[string]any) string { return output.Field(r, "regionSlug") }},
		{Header: "cpu", Value: func(r map[string]any) string { return output.Field(r, "cpuCores") }},
		{Header: "memory", Value: func(r map[string]any) string { return gib(r, "memoryGib") }},
		{Header: "disk", Value: func(r map[string]any) string { return gib(r, "diskGib") }},
		{Header: "address", Value: func(r map[string]any) string { return output.Field(r, "networkAddress") }},
		{Header: "age", Value: func(r map[string]any) string { return output.Age(output.Field(r, "createdAt"), now) }},

		{Header: "platform", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "platform") }},
		{Header: "image", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "imageRef") }},
		{Header: "flavor", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "flavorSlug") }},
		{Header: "node", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "node") }},
		{Header: "network", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "networkRef") }},
		{Header: "error", Wide: true, Value: func(r map[string]any) string { return output.Field(r, "lastError") }},
	}
}

// instanceStatusCell folds the pending action into the status column.
// A row reading "running" while a delete is in flight would be actively
// misleading, and the pending action is the more important of the two.
func instanceStatusCell(r map[string]any) string {
	status := output.Field(r, "status")
	if pending := output.Field(r, "pendingAction"); pending != "" {
		return fmt.Sprintf("%s (%s)", status, pending)
	}
	return status
}

func gib(r map[string]any, key string) string {
	v := output.Field(r, key)
	if v == "" {
		return ""
	}
	return v + " GiB"
}

func newInstanceCommand(s *State) *cobra.Command {
	c := &cobra.Command{
		Use:     "instance",
		Aliases: []string{"instances", "vm"},
		Short:   "Manage Xcloud instances (macOS and Linux VMs)",
		Long: "Manage Xcloud instances.\n\n" +
			"Most operations are asynchronous: the API accepts them and a worker carries\n" +
			"them out, so the command returns immediately. Pass --wait to block until the\n" +
			"instance reaches its new state — which is also what makes it safe to script\n" +
			"two operations in a row, since a second action while one is pending is\n" +
			"rejected with a conflict.",
	}
	c.AddCommand(
		newInstanceListCommand(s),
		newInstanceGetCommand(s),
		newInstanceCreateCommand(s),
		newInstanceDeleteCommand(s),
	)
	c.AddCommand(newInstanceLifecycleCommands(s)...)
	return c
}

func newInstanceListCommand(s *State) *cobra.Command {
	var region string

	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List Xcloud instances",
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

			query := url.Values{}
			if region != "" {
				query.Set("region", region)
			}
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodGet, Path: "/v1/xcloud/instances", Query: query,
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, instanceColumns(time.Now()))
		},
	}
	c.Flags().StringVar(&region, "region", "", "only list instances in this region (slug)")
	return c
}

func newInstanceGetCommand(s *State) *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Show one Xcloud instance",
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
			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodGet, Path: "/v1/xcloud/instances/" + args[0],
			})
			if err != nil {
				return err
			}
			return w.Render(resp.Body, instanceColumns(time.Now()))
		},
	}
}

func newInstanceCreateCommand(s *State) *cobra.Command {
	var (
		name       string
		region     string
		image      string
		platform   string
		flavor     string
		cpu        int
		memory     int
		disk       int
		network    string
		adminUser  string
		sshKeyIDs  []string
		allocateIP bool
		idemKey    string
		wf         waitFlags
	)

	c := &cobra.Command{
		Use:   "create",
		Short: "Create an Xcloud instance",
		Long: "Create an Xcloud instance.\n\n" +
			"--region accepts a slug ('xcloud region list' shows the ones available to\n" +
			"you) and is resolved to an id for you.\n\n" +
			"Creates are deduplicated: the CLI sends an idempotency key, so repeating\n" +
			"an interrupted create with the same --idempotency-key replays the original\n" +
			"result instead of provisioning a second instance. Keys expire after 24h.",
		Example: "  xcloud instance create --name build-01 --region ZRH1 \\\n" +
			"      --image ghcr.io/example/macos-sequoia:latest \\\n" +
			"      --cpu 10 --memory 28 --disk 480 --wait",
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

			body := map[string]any{
				"regionId":  regionID,
				"name":      name,
				"cpuCores":  cpu,
				"memoryGib": memory,
				"diskGib":   disk,
				"imageRef":  image,
			}
			if platform != "" {
				body["platform"] = platform
			}
			if flavor != "" {
				body["flavorSlug"] = flavor
			}
			if network != "" {
				body["networkRef"] = network
			}
			if adminUser != "" {
				body["adminUsername"] = adminUser
			}
			if len(sshKeyIDs) > 0 {
				body["sshKeyIds"] = sshKeyIDs
			}
			if allocateIP {
				body["pendingElasticIp"] = map[string]any{"mode": "allocate"}
			}

			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}

			// Always send a key, generating one when the caller did not
			// supply it. An instance is billable and a lost response
			// leaves the client unable to tell whether it exists —
			// with a key, repeating the identical request replays the
			// original 201 instead of provisioning a second VM.
			if idemKey == "" {
				idemKey = api.NewIdempotencyKey()
			}

			resp, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost, Path: "/v1/xcloud/instances",
				Body: payload, IdempotencyKey: idemKey,
			})
			if err != nil {
				return err
			}

			var created struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(resp.Body, &created)

			if wf.enabled && created.ID != "" {
				if err := awaitInstance(cmd, s, created.ID, wait.InstanceRunning(), wf, "creating"); err != nil {
					return err
				}
				// Re-read so the rendered row reflects the finished
				// instance rather than the pending one the POST returned.
				fresh, ferr := client.Do(cmd.Context(), api.RequestOptions{
					Method: http.MethodGet, Path: "/v1/xcloud/instances/" + created.ID,
				})
				if ferr == nil {
					return w.Render(fresh.Body, instanceColumns(time.Now()))
				}
			}
			return w.Render(resp.Body, instanceColumns(time.Now()))
		},
	}

	f := c.Flags()
	f.StringVar(&name, "name", "", "instance name (required)")
	f.StringVar(&region, "region", "", "region slug or id (required)")
	f.StringVar(&image, "image", "", "OCI image reference to boot (required)")
	f.IntVar(&cpu, "cpu", 0, "CPU cores (required)")
	f.IntVar(&memory, "memory", 0, "memory in GiB (required)")
	f.IntVar(&disk, "disk", 0, "disk in GiB (required)")
	f.StringVar(&platform, "platform", "", "macos or linux (default macos)")
	f.StringVar(&flavor, "flavor", "", "flavor slug, instead of explicit cpu/memory/disk")
	f.StringVar(&network, "network", "", "network reference (default 'default')")
	f.StringVar(&adminUser, "admin-username", "", "admin user to provision in the guest")
	f.StringSliceVar(&sshKeyIDs, "ssh-key", nil, "SSH key id to provision (repeatable)")
	f.BoolVar(&allocateIP, "allocate-elastic-ip", false, "allocate and attach a new elastic IP")
	f.StringVar(&idemKey, "idempotency-key", "", "reuse this key to safely retry an interrupted create (generated if unset)")
	wf.register(c)

	for _, required := range []string{"name", "region", "image"} {
		_ = c.MarkFlagRequired(required)
	}
	return c
}

func newInstanceDeleteCommand(s *State) *cobra.Command {
	var (
		wf          waitFlags
		releaseEIPs bool
		yes         bool
	)

	c := &cobra.Command{
		Use:     "delete <id>",
		Aliases: []string{"rm"},
		Short:   "Delete an Xcloud instance",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := s.Client()
			if err != nil {
				return err
			}
			w, err := s.Writer()
			if err != nil {
				return err
			}

			// Deleting a VM destroys its disk. Confirm unless the caller
			// opted out — a CI job passes --yes, a human gets one chance
			// to notice the wrong id.
			if !yes {
				ok, err := s.Confirm(fmt.Sprintf(
					"Delete instance %s and its disk? This cannot be undone. [y/N]: ", args[0]))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(w.Err, "Aborted.")
					return nil
				}
			}

			query := url.Values{}
			if releaseEIPs {
				query.Set("releaseElasticIps", "true")
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodDelete,
				Path:   "/v1/xcloud/instances/" + args[0],
				Query:  query,
			}); err != nil {
				return err
			}

			if !wf.enabled {
				if !w.Quiet {
					fmt.Fprintf(w.Err, "Delete queued for instance %s. Add --wait to block until it finishes.\n", args[0])
				}
				return nil
			}
			return awaitInstance(cmd, s, args[0], wait.InstanceGone(), wf, "deleting")
		},
	}
	c.Flags().BoolVar(&releaseEIPs, "release-elastic-ips", false, "also release elastic IPs attached to the instance")
	c.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	wf.register(c)
	return c
}

// newInstanceLifecycleCommands builds the simple POST-and-wait verbs.
// They differ only in endpoint, predicate and wording, so generating
// them keeps the five from drifting apart.
func newInstanceLifecycleCommands(s *State) []*cobra.Command {
	specs := []struct {
		use       string
		action    string
		short     string
		long      string
		verb      string
		predicate wait.Predicate
	}{
		{
			use: "start <id>", action: "start", verb: "starting",
			short:     "Start a stopped, suspended or parked instance",
			predicate: wait.InstanceRunning(),
		},
		{
			use: "stop <id>", action: "stop", verb: "stopping",
			short: "Stop an instance immediately",
			long: "Stops the instance by terminating it — the guest is not asked to shut\n" +
				"down cleanly. Use 'shutdown' for a graceful ACPI shutdown.",
			predicate: wait.InstanceStopped(),
		},
		{
			use: "shutdown <id>", action: "shutdown", verb: "shutting down",
			short: "Shut an instance down gracefully",
			long: "Asks the guest to shut down cleanly. This can take several minutes on\n" +
				"macOS; the server budgets around ten. Use 'stop' to terminate immediately.",
			predicate: wait.InstanceStopped(),
		},
		{
			use: "suspend <id>", action: "suspend", verb: "suspending",
			short: "Suspend an instance to disk",
			long: "Saves the guest's memory to disk and stops the VM, so it consumes no CPU\n" +
				"or RAM. Resume it with 'xcloud instance start'.",
			predicate: wait.InstanceSuspended(),
		},
	}

	cmds := make([]*cobra.Command, 0, len(specs)+1)
	for _, spec := range specs {
		spec := spec
		var wf waitFlags
		c := &cobra.Command{
			Use:   spec.use,
			Short: spec.short,
			Long:  spec.long,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return queueAction(cmd, s, args[0], spec.action, spec.predicate, wf, spec.verb)
			},
		}
		wf.register(c)
		cmds = append(cmds, c)
	}

	return append(cmds, newInstanceResizeCommand(s), newInstanceBootModeCommand(s))
}

func newInstanceResizeCommand(s *State) *cobra.Command {
	var (
		cpu, memory, disk int
		wf                waitFlags
	)

	c := &cobra.Command{
		Use:   "resize <id>",
		Short: "Change an instance's CPU, memory or disk",
		Long: "Resize an instance. It must be stopped, and disk can only grow — the API\n" +
			"rejects a shrink.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cpu == 0 && memory == 0 && disk == 0 {
				return &usageError{fmt.Errorf("specify at least one of --cpu, --memory or --disk")}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}

			body := map[string]any{}
			if cpu > 0 {
				body["cpuCores"] = cpu
			}
			if memory > 0 {
				body["memoryGib"] = memory
			}
			if disk > 0 {
				body["diskGib"] = disk
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return err
			}

			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost,
				Path:   "/v1/xcloud/instances/" + args[0] + "/resize",
				Body:   payload,
			}); err != nil {
				return err
			}

			w, _ := s.Writer()
			if !wf.enabled {
				if w != nil && !w.Quiet {
					fmt.Fprintf(w.Err, "Resize queued for instance %s. Add --wait to block until it finishes.\n", args[0])
				}
				return nil
			}
			return awaitInstance(cmd, s, args[0], wait.InstanceSettled(), wf, "resizing")
		},
	}
	c.Flags().IntVar(&cpu, "cpu", 0, "new CPU core count")
	c.Flags().IntVar(&memory, "memory", 0, "new memory in GiB")
	c.Flags().IntVar(&disk, "disk", 0, "new disk in GiB (grow only)")
	wf.register(c)
	return c
}

func newInstanceBootModeCommand(s *State) *cobra.Command {
	var (
		recovery bool
		normal   bool
		wf       waitFlags
	)

	c := &cobra.Command{
		Use:   "boot-mode <id>",
		Short: "Boot an instance into recovery, or back to normal",
		Long: "Switch the boot mode. Recovery boot is macOS-only; the API rejects it for\n" +
			"Linux instances.\n\n" +
			"Changing the boot mode recreates the VM upstream, so expect it to take about\n" +
			"as long as a fresh boot.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recovery == normal {
				return &usageError{fmt.Errorf("pass exactly one of --recovery or --normal")}
			}
			client, err := s.Client()
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]any{"bootIntoRecovery": recovery})
			if err != nil {
				return err
			}
			if _, err := client.Do(cmd.Context(), api.RequestOptions{
				Method: http.MethodPost,
				Path:   "/v1/xcloud/instances/" + args[0] + "/boot-mode",
				Body:   payload,
			}); err != nil {
				return err
			}

			verb := "switching to normal boot"
			if recovery {
				verb = "switching to recovery boot"
			}
			w, _ := s.Writer()
			if !wf.enabled {
				if w != nil && !w.Quiet {
					fmt.Fprintf(w.Err, "Boot-mode change queued for instance %s. Add --wait to block until it finishes.\n", args[0])
				}
				return nil
			}
			return awaitInstance(cmd, s, args[0], wait.InstanceSettled(), wf, verb)
		},
	}
	c.Flags().BoolVar(&recovery, "recovery", false, "boot into recovery (macOS only)")
	c.Flags().BoolVar(&normal, "normal", false, "boot normally")
	wf.register(c)
	return c
}
