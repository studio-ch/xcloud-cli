package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/studio-ch/cloudconsole-cli/internal/exitcode"
)

// Assembled from fragments rather than written as one literal. The value
// is fake, but a full-length `sk_live_…` string is indistinguishable from
// a Stripe live key to a secret scanner, and GitHub push protection
// rejects it — which would block the public mirror. Keeping the exact
// shape matters: extractPrefix parity depends on the 12-character
// segment after the scheme.
const testToken = "sk_" + "live_" + "abcdefghijkl" + "MNOPQRSTUVWXYZ0123456789abcdefghij"

func TestDoSendsRequiredHeaders(t *testing.T) {
	f := newFakeAPI(t, step{status: 200, body: `{"data":[]}`})
	c := testClient(t, f, testToken)

	if _, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/regions"}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	req := f.last()
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "cloudconsole-cli/") {
		t.Errorf("User-Agent = %q, want an cloudconsole-cli/... prefix", got)
	}
	if got := req.Header.Get("x-request-id"); len(got) != 32 {
		t.Errorf("x-request-id = %q, want a 32-char client-generated id", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}
}

func TestDoReturnsBodyVerbatim(t *testing.T) {
	// Field order, unknown fields and number formatting must all survive
	// untouched — this is the `--output json` stability promise.
	const payload = `{"data":[{"zeta":1,"alpha":"x","unknownFutureField":{"n":10000000000000001}}]}`
	f := newFakeAPI(t, step{status: 200, body: payload})
	c := testClient(t, f, testToken)

	resp, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/xcloud/instances"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if string(resp.Body) != payload {
		t.Errorf("body was not passed through verbatim:\n got: %s\nwant: %s", resp.Body, payload)
	}
}

// The idempotency middleware is mounted on a specific set of routers.
// Sending the header elsewhere does nothing server-side, so we must not
// send it — and must not silently pretend the request became retry-safe.
func TestIdempotencyKeyOnlySentOnSupportedPaths(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/resources", true},
		{"/v1/resources/abc/start", true},
		{"/v1/xcloud-security-groups", true},
		{"/v1/xcloud/networks", true},
		{"/v1/buildkite/stacks", true},
		{"/v1/github-actions/stacks", true},
		{"/v1/xcloud/instances", true},
		{"/v1/xcloud/volumes", false},
		{"/v1/ssh-keys", false},
		{"/v1/elastic-ips", false},
		{"/v1/api-keys", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			f := newFakeAPI(t, step{status: 200, body: `{}`})
			c := testClient(t, f, testToken)
			_, err := c.Do(context.Background(), RequestOptions{
				Method: http.MethodPost, Path: tt.path,
				Body: []byte(`{}`), IdempotencyKey: "key-123",
			})
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			got := f.last().Header.Get("Idempotency-Key") != ""
			if got != tt.want {
				t.Errorf("Idempotency-Key sent = %v, want %v", got, tt.want)
			}
		})
	}
}

// A 429 is retryable for every method, because the rate limiter is
// mounted globally before all routes — the request provably never
// reached a handler, so no side effect can have occurred.
func TestRetriesRateLimitEvenForUnguardedPost(t *testing.T) {
	f := newFakeAPI(t,
		step{status: 429, headers: map[string]string{"Retry-After": "0"}},
		step{status: 201, body: `{"id":"new"}`},
	)
	c := testClient(t, f, testToken)

	resp, err := c.Do(context.Background(), RequestOptions{
		Method: http.MethodPost,
		Path:   "/v1/xcloud/instances",
		Body:   []byte(`{}`), // no Idempotency-Key: not deduplicated
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	if f.count() != 2 {
		t.Errorf("made %d requests, want 2 (one 429 then a retry)", f.count())
	}
}

// A 5xx on a POST sent WITHOUT an Idempotency-Key must not be retried,
// even on a path that supports one: without the key the server has
// nothing to deduplicate against, so a retry could mint a second
// billable VM.
func TestDoesNotRetryUnguardedPostOn5xx(t *testing.T) {
	f := newFakeAPI(t, step{status: 503, body: `{"title":"Service Unavailable","status":503}`})
	c := testClient(t, f, testToken)

	_, err := c.Do(context.Background(), RequestOptions{
		Method: http.MethodPost, Path: "/v1/xcloud/instances", Body: []byte(`{}`),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if f.count() != 1 {
		t.Errorf("made %d requests, want exactly 1 (no retry without deduplication)", f.count())
	}
}

// The same 5xx on a path that DOES deduplicate, with a key supplied, is
// safe to retry.
func TestRetriesGuardedPostOn5xx(t *testing.T) {
	f := newFakeAPI(t,
		step{status: 503, body: `{"title":"Service Unavailable","status":503}`},
		step{status: 201, body: `{"id":"ok"}`},
	)
	c := testClient(t, f, testToken)

	resp, err := c.Do(context.Background(), RequestOptions{
		Method: http.MethodPost, Path: "/v1/resources",
		Body: []byte(`{}`), IdempotencyKey: "key-abc",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Status != 201 || f.count() != 2 {
		t.Errorf("status=%d requests=%d, want 201 and 2", resp.Status, f.count())
	}
}

func TestRetriesGetOn5xx(t *testing.T) {
	f := newFakeAPI(t,
		step{status: 502},
		step{status: 200, body: `{"data":[]}`},
	)
	c := testClient(t, f, testToken)
	if _, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/regions"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if f.count() != 2 {
		t.Errorf("made %d requests, want 2", f.count())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 412, 422} {
		f := newFakeAPI(t, step{status: status, body: `{"title":"x","status":` + itoa(status) + `}`})
		c := testClient(t, f, testToken)
		_, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/regions"})
		if err == nil {
			t.Errorf("status %d: expected an error", status)
		}
		if f.count() != 1 {
			t.Errorf("status %d: made %d requests, want 1", status, f.count())
		}
	}
}

func TestProblemDecoding(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		check  func(*testing.T, *Problem)
	}{
		{
			name: "read-only key", status: 403, body: bodyReadOnlyScope,
			check: func(t *testing.T, p *Problem) {
				if !p.IsReadOnlyKey() {
					t.Error("IsReadOnlyKey() = false")
				}
				if p.ExitCode() != exitcode.Forbidden {
					t.Errorf("exit code = %d, want %d", p.ExitCode(), exitcode.Forbidden)
				}
			},
		},
		{
			name: "service disabled", status: 403, body: bodyServiceDisabled,
			check: func(t *testing.T, p *Problem) {
				if p.Code != "service_disabled" {
					t.Errorf("Code = %q", p.Code)
				}
				if p.Service != "xcloud" {
					t.Errorf("Service = %q, want xcloud", p.Service)
				}
				if p.IsReadOnlyKey() {
					t.Error("a disabled service must not look like a read-only key")
				}
			},
		},
		{
			name: "quota exceeded", status: 412, body: bodyQuotaExceeded,
			check: func(t *testing.T, p *Problem) {
				if p.Quota == nil {
					t.Fatal("Quota member was not decoded")
				}
				if p.Quota.Key != "xcloud_instances_total" || p.Quota.Limit != 25 ||
					p.Quota.Usage != 25 || p.Quota.Requested != 1 {
					t.Errorf("Quota = %+v", *p.Quota)
				}
				if p.Type != "https://studio-cp.dev/errors/quota-exceeded" {
					t.Errorf("Type = %q — a quota 412 is not about:blank", p.Type)
				}
				if p.ExitCode() != exitcode.Precondition {
					t.Errorf("exit code = %d, want %d", p.ExitCode(), exitcode.Precondition)
				}
			},
		},
		{
			name: "expired key", status: 401, body: bodyKeyExpired,
			check: func(t *testing.T, p *Problem) {
				if !p.IsExpiredKey() {
					t.Error("IsExpiredKey() = false")
				}
			},
		},
		{
			name: "invalid key", status: 401, body: bodyKeyInvalid,
			check: func(t *testing.T, p *Problem) {
				if p.IsExpiredKey() {
					t.Error("an invalid key must not be reported as expired")
				}
				if p.ExitCode() != exitcode.Auth {
					t.Errorf("exit code = %d, want %d", p.ExitCode(), exitcode.Auth)
				}
			},
		},
		{
			name: "busy conflict", status: 409, body: bodyBusy,
			check: func(t *testing.T, p *Problem) {
				if !p.IsBusy() {
					t.Error("IsBusy() = false")
				}
				if p.IsIdempotencyConflict() {
					t.Error("a busy resource is not an idempotency conflict")
				}
			},
		},
		{
			name: "idempotency conflict", status: 409, body: bodyIdempotencyConflict,
			check: func(t *testing.T, p *Problem) {
				if !p.IsIdempotencyConflict() {
					t.Error("IsIdempotencyConflict() = false")
				}
				if p.IsBusy() {
					t.Error("an idempotency conflict is not a busy resource")
				}
			},
		},
		{
			// A proxy error page never reached the application. We must
			// still produce something the user can act on.
			name: "non-JSON body", status: 502,
			body: "<html><head><title>502 Bad Gateway</title></head><body>nginx</body></html>",
			check: func(t *testing.T, p *Problem) {
				if p.Title != "Bad Gateway" {
					t.Errorf("Title = %q, want Bad Gateway", p.Title)
				}
				if !strings.Contains(p.Detail, "502") {
					t.Errorf("Detail should keep a snippet of the body, got %q", p.Detail)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeAPI(t, step{status: tt.status, body: tt.body})
			c := testClient(t, f, testToken)
			_, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/thing"})

			var p *Problem
			if !errors.As(err, &p) {
				t.Fatalf("error is not a *Problem: %v", err)
			}
			if p.Status != tt.status {
				t.Errorf("Status = %d, want %d", p.Status, tt.status)
			}
			if p.RequestID == "" {
				t.Error("RequestID is empty — support cannot trace this")
			}
			tt.check(t, p)
		})
	}
}

// The token must never appear in --debug output, no matter which header
// or body carried it. This test greps the whole captured buffer.
func TestTracerNeverLeaksToken(t *testing.T) {
	var buf bytes.Buffer
	f := newFakeAPI(t,
		step{status: 429, headers: map[string]string{"Retry-After": "0"}},
		step{status: 500},
		step{status: 200, body: `{"ok":true}`},
	)
	c, err := New(Options{
		Origin: f.URL, Token: testToken, AllowInsecure: true,
		Tracer: NewTracer(&buf, testToken),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Do(context.Background(), RequestOptions{Method: http.MethodGet, Path: "/v1/regions"}); err != nil {
		t.Fatalf("Do: %v", err)
	}

	out := buf.String()
	if out == "" {
		t.Fatal("tracer produced no output")
	}
	if strings.Contains(out, testToken) {
		t.Errorf("the raw token leaked into --debug output:\n%s", out)
	}
	// The public lookup prefix is expected and useful.
	if !strings.Contains(out, "sk_live_abcdefghijkl") {
		t.Errorf("trace should show the public key prefix so users can tell which key was used:\n%s", out)
	}
	if !strings.Contains(out, "redacted") {
		t.Errorf("trace should mark the redaction explicitly:\n%s", out)
	}
}

func TestRejectsPlaintextHTTPToRemoteHost(t *testing.T) {
	if _, err := New(Options{Origin: "http://api.example.com", Token: testToken}); err == nil {
		t.Error("expected plaintext http:// to a remote host to be refused")
	}
	if _, err := New(Options{Origin: "http://localhost:3001", Token: testToken}); err != nil {
		t.Errorf("localhost over http should be allowed for local development, got: %v", err)
	}
	if _, err := New(Options{Origin: "http://api.example.com", Token: testToken, AllowInsecure: true}); err != nil {
		t.Errorf("AllowInsecure should permit it, got: %v", err)
	}
}

func TestNormalizesOriginWithV1Suffix(t *testing.T) {
	c, err := New(Options{Origin: "https://api.cloud.flow.swiss/v1", Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Origin() != "https://api.cloud.flow.swiss" {
		t.Errorf("Origin() = %q, want the bare origin", c.Origin())
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	f := newFakeAPI(t, step{status: 503})
	c := testClient(t, f, testToken)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.Do(ctx, RequestOptions{Method: http.MethodGet, Path: "/v1/regions"}); err == nil {
		t.Error("expected an error from a cancelled context")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
