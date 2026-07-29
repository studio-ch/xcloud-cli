// Package api is the CLI's HTTP transport against the Cloud Console
// public API. It wraps the generated client in internal/api/gen with the
// concerns codegen cannot express: credential injection, the retry
// policy, RFC 9457 error decoding, and redacted request tracing.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

// Problem is a decoded RFC 9457 problem-details response, plus the
// transport-side context needed to make it actionable.
//
// Two API responses deviate from the plain {type,title,status,detail}
// shape documented in docs/public-api.md §5, and both matter enough to
// model explicitly:
//
//   - A 412 from the quota service uses
//     type=https://studio-cp.dev/errors/quota-exceeded and carries a
//     `quota` member with the numbers. Rendering "Precondition Failed"
//     when we could render "you are using 25 of 25 cloudconsole instances" is
//     a wasted opportunity.
//   - A 403 from the per-tenant service kill switch carries both `code`
//     and `service`, so we can name which service is disabled.
type Problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail"`
	Instance string       `json:"instance,omitempty"`
	Code     string       `json:"code,omitempty"`
	Service  string       `json:"service,omitempty"`
	Quota    *QuotaDetail `json:"quota,omitempty"`

	// Transport context, never part of the wire body.
	RequestID  string        `json:"-"`
	Method     string        `json:"-"`
	URL        string        `json:"-"`
	Attempts   int           `json:"-"`
	RetryAfter time.Duration `json:"-"`
	RateLimit  RateLimit     `json:"-"`
	Raw        []byte        `json:"-"`
}

// QuotaDetail is the `quota` extension member on a 412.
type QuotaDetail struct {
	Key       string `json:"key"`
	Limit     int64  `json:"limit"`
	Usage     int64  `json:"usage"`
	Requested int64  `json:"requested"`
}

// RateLimit is the token-bucket state the API reports on every response.
type RateLimit struct {
	Limit     int
	Remaining int
	// Reset is when the bucket is FULL again — not when the next single
	// token frees up. It is therefore useless for backoff; only
	// Retry-After answers "when may I try again". Kept for display.
	Reset int64
}

func (p *Problem) Error() string {
	if p.Detail != "" {
		return fmt.Sprintf("%s: %s", p.Title, p.Detail)
	}
	if p.Title != "" {
		return p.Title
	}
	return fmt.Sprintf("HTTP %d", p.Status)
}

// ExitCode maps the problem onto the CLI's public exit-code contract.
func (p *Problem) ExitCode() exitcode.Code {
	return exitcode.FromHTTPStatus(p.Status)
}

// IsExpiredKey distinguishes an expired credential from a merely invalid
// one. Both are 401, but the remedies differ ("issue a new key" versus
// "check you pasted the right one"), so it is worth telling them apart.
// The detail string is set in apps/api/src/middleware/auth.ts.
func (p *Problem) IsExpiredKey() bool {
	return p.Status == http.StatusUnauthorized &&
		strings.Contains(strings.ToLower(p.Detail), "expired")
}

// IsReadOnlyKey reports a 403 caused by a key lacking write:resources,
// as opposed to a disabled service. See
// apps/api/src/middleware/scope.ts for the message this matches.
func (p *Problem) IsReadOnlyKey() bool {
	return p.Status == http.StatusForbidden &&
		p.Code == "" &&
		strings.Contains(strings.ToLower(p.Detail), "read-only")
}

// IsBusy reports the "resource is busy with another lifecycle action"
// flavour of 409, as opposed to an idempotency-key collision. The two
// need different advice, and the caller cannot tell them apart from the
// status alone.
func (p *Problem) IsBusy() bool {
	if p.Status != http.StatusConflict {
		return false
	}
	d := strings.ToLower(p.Detail)
	return strings.Contains(d, "busy") || strings.Contains(d, "pending")
}

// IsIdempotencyConflict reports the "same key, different body" 409 from
// apps/api/src/middleware/idempotency.ts.
func (p *Problem) IsIdempotencyConflict() bool {
	return p.Status == http.StatusConflict &&
		strings.Contains(strings.ToLower(p.Detail), "idempotency")
}

// parseProblem turns a non-2xx response body into a *Problem.
//
// It is deliberately liberal about content type. The API mostly sends
// application/problem+json, but some middleware returns plain
// application/json with the same shape, and the top-level onError
// handler returns text/plain. A reverse proxy in front of the API can
// return an HTML error page that never reached the application at all.
// A CLI that only understood the happy path would print nothing useful
// in exactly the situations where the user most needs a clue.
func parseProblem(resp *http.Response, body []byte, method, url string, attempts int) *Problem {
	p := &Problem{
		Status:    resp.StatusCode,
		Method:    method,
		URL:       url,
		Attempts:  attempts,
		RequestID: resp.Header.Get("x-request-id"),
		RateLimit: parseRateLimit(resp.Header),
		Raw:       body,
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		p.RetryAfter = parseRetryAfter(ra)
	}

	if len(body) > 0 {
		var wire Problem
		if err := json.Unmarshal(body, &wire); err == nil && (wire.Title != "" || wire.Detail != "" || wire.Status != 0) {
			p.Type, p.Title, p.Detail = wire.Type, wire.Title, wire.Detail
			p.Instance, p.Code, p.Service, p.Quota = wire.Instance, wire.Code, wire.Service, wire.Quota
			// Trust our own observed status over the body's, in case a
			// proxy rewrote one of them.
			if p.Title != "" {
				return p
			}
		}
	}

	// Not a problem document: synthesise something honest from the
	// status line, keeping a snippet of the body as the detail so an
	// HTML error page still tells the user which proxy answered.
	p.Type = "about:blank"
	p.Title = http.StatusText(resp.StatusCode)
	if p.Title == "" {
		p.Title = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	if p.Detail == "" {
		p.Detail = snippet(body)
	}
	return p
}

// snippet trims a non-JSON error body down to something printable on a
// terminal, collapsing whitespace so an HTML page does not spray the
// screen.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func parseRateLimit(h http.Header) RateLimit {
	return RateLimit{
		Limit:     atoiDefault(h.Get("X-RateLimit-Limit"), 0),
		Remaining: atoiDefault(h.Get("X-RateLimit-Remaining"), -1),
		Reset:     int64(atoiDefault(h.Get("X-RateLimit-Reset"), 0)),
	}
}

// parseRetryAfter handles the delta-seconds form the API uses. The HTTP
// spec also allows an absolute date; we accept it rather than silently
// treating it as zero and hammering the server.
func parseRetryAfter(v string) time.Duration {
	if secs := atoiDefault(v, -1); secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	return n
}
