package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/studio-ch/xcloud-cli/internal/config"
)

// Tracer writes redacted HTTP traces for --debug.
//
// All debug output funnels through Redact() — a single chokepoint, so
// there is exactly one place to audit and exactly one place a test has
// to defeat to prove the token never escapes. A nil *Tracer is a valid
// no-op tracer, which keeps every call site free of nil checks.
type Tracer struct {
	mu     sync.Mutex
	w      io.Writer
	secret string
}

// NewTracer returns a tracer writing to w, redacting the given token.
func NewTracer(w io.Writer, token string) *Tracer {
	return &Tracer{w: w, secret: token}
}

// Redact replaces the credential anywhere it appears with its public
// lookup prefix plus a length marker.
//
// Showing the prefix is safe by construction: the server stores it in
// plaintext in api_keys.prefix and the panel displays it. It is also the
// single most useful thing to show, because it lets a user confirm which
// key a failing command actually used.
func (t *Tracer) Redact(s string) string {
	if t == nil || t.secret == "" {
		return s
	}
	prefix := config.ExtractKeyPrefix(t.secret)
	if prefix == "" {
		prefix = "sk_***"
	}
	masked := fmt.Sprintf("%s…[redacted %d chars]", prefix, len(t.secret)-len(prefix))
	return strings.ReplaceAll(s, t.secret, masked)
}

func (t *Tracer) printf(format string, args ...any) {
	if t == nil || t.w == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintln(t.w, t.Redact(strings.TrimRight(fmt.Sprintf(format, args...), "\n")))
}

func (t *Tracer) request(req *http.Request, attempt int) {
	if t == nil || t.w == nil {
		return
	}
	t.printf("> %s %s  req=%s  attempt=%d", req.Method, req.URL.Path, short(req.Header.Get("x-request-id")), attempt)
	for _, h := range []string{"Authorization", "Idempotency-Key", "Content-Type", "User-Agent"} {
		if v := req.Header.Get(h); v != "" {
			t.printf(">   %s: %s", h, v)
		}
	}
}

func (t *Tracer) response(reqID string, resp *http.Response, rl RateLimit, took time.Duration) {
	if t == nil || t.w == nil {
		return
	}
	line := fmt.Sprintf("< %d %s  in %s  req=%s",
		resp.StatusCode, http.StatusText(resp.StatusCode), took.Round(time.Millisecond), short(reqID))
	if rl.Limit > 0 {
		line += fmt.Sprintf("  ratelimit=%d/%d", rl.Remaining, rl.Limit)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		line += "  retry-after=" + ra + "s"
	}
	t.printf("%s", line)
	// Surface the deprecation signal even in normal traces: it is the
	// designed channel for telling a client it is about to break.
	if resp.Header.Get("Deprecation") != "" {
		t.printf("<   Deprecation: %s  Sunset: %s",
			resp.Header.Get("Deprecation"), resp.Header.Get("Sunset"))
	}
}

func (t *Tracer) transportError(reqID string, err error, took time.Duration) {
	t.printf("! transport error after %s  req=%s: %v", took.Round(time.Millisecond), short(reqID), err)
}

func (t *Tracer) retrying(d time.Duration, next int) {
	t.printf("  retrying in %s (attempt %d)", d.Round(time.Millisecond), next)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
