package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/studio-ch/cloudconsole-cli/internal/api"
	"github.com/studio-ch/cloudconsole-cli/internal/wait"
)

// waitFlags are the --wait knobs shared by every asynchronous command.
type waitFlags struct {
	enabled bool
	timeout time.Duration
}

func (w *waitFlags) register(c *cobra.Command) {
	c.Flags().BoolVar(&w.enabled, "wait", false,
		"wait for the operation to finish before returning")
	c.Flags().DurationVar(&w.timeout, "wait-timeout", wait.DefaultTimeout,
		"how long to wait with --wait")
}

// instanceState is the subset of an Xcloud instance the waiter needs.
type instanceState struct {
	Status        string  `json:"status"`
	PendingAction *string `json:"pendingAction"`
	LastError     *string `json:"lastError"`
}

// fetchInstanceState reads one instance, translating a 404 into
// "does not exist" rather than an error — a delete completes precisely
// when the row stops existing.
func fetchInstanceState(ctx context.Context, c *api.Client, id string) (wait.State, error) {
	resp, err := c.Do(ctx, api.RequestOptions{
		Method: http.MethodGet,
		Path:   "/v1/xcloud/instances/" + id,
	})
	if err != nil {
		var p *api.Problem
		if errors.As(err, &p) && (p.Status == http.StatusNotFound || p.Status == http.StatusGone) {
			return wait.State{Exists: false}, nil
		}
		return wait.State{}, err
	}
	var s instanceState
	if err := json.Unmarshal(resp.Body, &s); err != nil {
		return wait.State{}, fmt.Errorf("could not read the instance state: %w", err)
	}
	return wait.State{
		Exists:        true,
		Status:        s.Status,
		PendingAction: deref(s.PendingAction),
		LastError:     deref(s.LastError),
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// awaitInstance runs a wait with a stderr progress line.
//
// Progress goes to stderr unconditionally, so
// `cloudconsole instance create --wait -o json | jq` stays valid — the spinner
// must never end up in the payload.
func awaitInstance(cmd *cobra.Command, s *State, id string, predicate wait.Predicate, flags waitFlags, verb string) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	w, err := s.Writer()
	if err != nil {
		return err
	}

	var lastLine string
	progress := func(st wait.State, elapsed time.Duration) {
		if w.Quiet {
			return
		}
		label := st.Status
		if st.PendingAction != "" {
			label = st.PendingAction
		}
		line := fmt.Sprintf("  %s… (%s, %s elapsed)", verb, label, elapsed.Round(time.Second))
		if line != lastLine {
			fmt.Fprintln(w.Err, line)
			lastLine = line
		}
	}

	final, err := wait.For(cmd.Context(), wait.Options{
		Timeout:  flags.timeout,
		Progress: progress,
		Fetch: func(ctx context.Context) (wait.State, error) {
			return fetchInstanceState(ctx, client, id)
		},
	}, predicate)

	var timeout *wait.TimeoutError
	if errors.As(err, &timeout) {
		return &waitTimeoutError{
			Resource: fmt.Sprintf("instance %s", id),
			Elapsed:  timeout.Elapsed.Round(time.Second).String(),
		}
	}
	if err != nil {
		return err
	}
	if !w.Quiet {
		if final.Exists {
			fmt.Fprintf(w.Err, "  done (%s)\n", final.Status)
		} else {
			fmt.Fprintln(w.Err, "  done (deleted)")
		}
	}
	return nil
}

// queueAction posts a lifecycle action and optionally waits for it.
//
// These endpoints return 202 with no useful body — the state lives on
// the row — so there is nothing to render, and the interesting output is
// either the wait's progress or nothing at all.
func queueAction(cmd *cobra.Command, s *State, id, action string, predicate wait.Predicate, flags waitFlags, verb string) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	if _, err := client.Do(cmd.Context(), api.RequestOptions{
		Method: http.MethodPost,
		Path:   fmt.Sprintf("/v1/xcloud/instances/%s/%s", id, action),
	}); err != nil {
		return err
	}

	w, _ := s.Writer()
	if !flags.enabled {
		if w != nil && !w.Quiet {
			fmt.Fprintf(w.Err, "%s queued for instance %s. Add --wait to block until it finishes.\n", verb, id)
		}
		return nil
	}
	return awaitInstance(cmd, s, id, predicate, flags, verb)
}

// resolveRegion turns a region slug into the UUID the API wants.
//
// The create endpoint takes `regionId` as a UUID, which no human types
// from memory. Accepting a slug and resolving it here is the difference
// between a usable command and one that requires a lookup first. A value
// that already looks like a UUID is passed through untouched.
func resolveRegion(ctx context.Context, c *api.Client, value string) (string, error) {
	if looksLikeUUID(value) {
		return value, nil
	}
	resp, err := c.Do(ctx, api.RequestOptions{Method: http.MethodGet, Path: "/v1/regions"})
	if err != nil {
		return "", err
	}
	var envelope struct {
		Data []struct {
			ID       string `json:"id"`
			Slug     string `json:"slug"`
			Services struct {
				Xcloud bool `json:"xcloud"`
			} `json:"services"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &envelope); err != nil {
		return "", fmt.Errorf("could not read the region list: %w", err)
	}

	var available []string
	for _, r := range envelope.Data {
		if r.Slug == value {
			return r.ID, nil
		}
		if r.Services.Xcloud {
			available = append(available, r.Slug)
		}
	}
	return "", &usageError{fmt.Errorf(
		"unknown region %q; regions available to you: %v", value, available)}
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
