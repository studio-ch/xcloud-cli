package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/studio-ch/xcloud-cli/internal/api"
	"github.com/studio-ch/xcloud-cli/internal/exitcode"
	"github.com/studio-ch/xcloud-cli/internal/output"
)

// configError is a local configuration or credential problem — nothing
// was sent to the API.
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

// usageError is a bad invocation we detected ourselves, as opposed to
// one cobra's flag parser rejected.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// waitTimeoutError is returned when --wait hits its deadline. It is
// deliberately NOT a failure of the mutation: the operation was accepted
// and may still succeed, we simply stopped watching.
type waitTimeoutError struct {
	Resource string
	Elapsed  string
}

func (e *waitTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s waiting for %s", e.Elapsed, e.Resource)
}

// report renders an error and returns the process exit code.
//
// Structure is always: what went wrong, the API's own words, the request
// id for support, then what to do about it. The request id matters more
// than it looks — it is the only handle support has to find the request
// in the API logs, and a user will not know to ask for it.
func report(w io.Writer, err error, s *State) exitcode.Code {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(w, "Aborted.")
		return exitcode.Code(130)
	}

	// Structured output must stay machine-readable on the error path
	// too: stdout stays empty, and stderr carries one JSON object.
	structured := false
	if s != nil && s.writer != nil {
		structured = s.writer.Format.IsStructured()
	}

	var problem *api.Problem
	if errors.As(err, &problem) {
		if structured {
			return reportJSON(w, problem)
		}
		return reportProblem(w, problem)
	}

	var transport *api.TransportError
	if errors.As(err, &transport) {
		fmt.Fprintf(w, "Error: %s\n", transport.Error())
		if transport.IsTimeout() {
			fmt.Fprintln(w, "Hint: the request timed out. Raise it with --timeout 60s, or check whether a proxy is in the way.")
		} else {
			fmt.Fprintln(w, "Hint: check network connectivity and any HTTPS_PROXY setting. 'xcloud version' probes the API's health endpoint.")
		}
		return exitcode.Network
	}

	var cfgErr *configError
	if errors.As(err, &cfgErr) {
		fmt.Fprintf(w, "Error: %s\n", cfgErr.Error())
		return exitcode.Config
	}

	var useErr *usageError
	if errors.As(err, &useErr) {
		fmt.Fprintf(w, "Error: %s\n", useErr.Error())
		return exitcode.Usage
	}

	var waitErr *waitTimeoutError
	if errors.As(err, &waitErr) {
		fmt.Fprintf(w, "Error: %s\n", waitErr.Error())
		fmt.Fprintln(w, "Hint: the operation was accepted and may still be running. Check with 'xcloud instance get', or raise --wait-timeout.")
		return exitcode.WaitTimeout
	}

	// Cobra's own flag-parsing errors land here.
	if isUsageError(err) {
		fmt.Fprintf(w, "Error: %s\n", err.Error())
		return exitcode.Usage
	}

	fmt.Fprintf(w, "Error: %s\n", err.Error())
	return exitcode.Unexpected
}

func isUsageError(err error) bool {
	msg := err.Error()
	for _, marker := range []string{
		"unknown command", "unknown flag", "unknown shorthand",
		"accepts ", "invalid argument", "required flag",
		"flag needs an argument",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// reportProblem renders an API error for a human.
func reportProblem(w io.Writer, p *api.Problem) exitcode.Code {
	headline, hint := describe(p)

	fmt.Fprintf(w, "Error: %s\n", headline)
	if p.Detail != "" && p.Detail != headline {
		fmt.Fprintf(w, "  %s\n", p.Detail)
	}
	if p.RequestID != "" {
		fmt.Fprintf(w, "  request-id: %s   (quote this to support)\n", p.RequestID)
	}
	if hint != "" {
		fmt.Fprintf(w, "Hint: %s\n", hint)
	}
	return p.ExitCode()
}

// reportJSON writes the problem document to stderr for -o json/yaml, so
// a script can branch on it without parsing prose.
func reportJSON(w io.Writer, p *api.Problem) exitcode.Code {
	payload := map[string]any{
		"status":    p.Status,
		"title":     p.Title,
		"detail":    p.Detail,
		"type":      p.Type,
		"requestId": p.RequestID,
		"method":    p.Method,
		"url":       p.URL,
		"attempts":  p.Attempts,
	}
	if p.Code != "" {
		payload["code"] = p.Code
	}
	if p.Service != "" {
		payload["service"] = p.Service
	}
	if p.Quota != nil {
		payload["quota"] = p.Quota
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"error": payload})
	return p.ExitCode()
}

// describe maps a problem onto a headline and an actionable hint.
//
// The hints are the part that earns its keep. "Forbidden" tells a user
// nothing; "this key is read-only, create one with the rw preset" ends
// the support ticket before it is filed.
func describe(p *api.Problem) (headline, hint string) {
	switch {
	case p.IsExpiredKey():
		return "Your API key has expired.",
			"Issue a new key in the panel under Settings → API keys, then run 'xcloud auth login'."

	case p.Status == 401:
		return "Authentication failed — the API rejected this key.",
			"Run 'xcloud auth login' with a key from Settings → API keys. Keys are shown only once, at creation."

	case p.IsReadOnlyKey():
		return "This API key is read-only.",
			"Create a key with the Read+Write preset. 'xcloud auth status' shows the scopes of the key in use."

	case p.Code == "service_disabled":
		service := p.Service
		if service == "" {
			service = "this service"
		}
		return fmt.Sprintf("The %s service is not enabled for your organisation.", service),
			"Contact a platform administrator to have it enabled."

	case p.Status == 403:
		return "Permission denied.",
			"'xcloud auth status' shows which key and organisation this command used."

	case p.Code == "upstream_missing":
		return "The upstream resource no longer exists; this record is stale.",
			"Delete the record to clean it up, or contact support if it should still exist."

	case p.Status == 404 || p.Status == 410:
		return "Not found.",
			"An API key is bound to one organisation, and a resource in another one reads as 'not found' rather than 'forbidden'. Check 'xcloud auth whoami'."

	case p.IsIdempotencyConflict():
		return "That idempotency key was already used with a different request body.",
			"Use a fresh --idempotency-key, or re-send the identical request. Keys expire after 24 hours."

	case p.IsBusy():
		return "The resource is busy with another operation.",
			"Wait for the current operation to finish — most commands accept --wait — then try again."

	case p.Status == 409:
		return "Conflict.", ""

	case p.Quota != nil:
		return fmt.Sprintf("Quota exceeded: %s — using %d of %d, this request needs %d more.",
				p.Quota.Key, p.Quota.Usage, p.Quota.Limit, p.Quota.Requested),
			"Delete unused resources, or ask support to raise the limit. 'xcloud quota list' shows all quotas."

	case p.Status == 412:
		return "Precondition failed.", ""

	case p.Status == 400 || p.Status == 422:
		return "The API rejected this request as invalid.",
			"Check the command's --help, or the reference at <api>/v1/docs."

	case p.Status == 429:
		limit := ""
		if p.RateLimit.Limit > 0 {
			limit = fmt.Sprintf(" (limit %d requests, refilling at 2/s)", p.RateLimit.Limit)
		}
		return fmt.Sprintf("Rate limited — gave up after %d attempts%s.", p.Attempts, limit),
			"Reduce parallelism or spread the calls out. The bucket is per API key."

	case p.Status >= 500:
		return fmt.Sprintf("The API returned %d after %d attempts.", p.Status, p.Attempts),
			"If this persists, quote the request-id above to support."
	}
	return p.Title, ""
}

// warnf writes a non-fatal warning to stderr. Warnings never touch
// stdout, so they cannot corrupt piped output.
func warnf(w *output.Writer, format string, args ...any) {
	if w == nil || w.Quiet {
		return
	}
	fmt.Fprintf(w.Err, "warning: "+format+"\n", args...)
}
