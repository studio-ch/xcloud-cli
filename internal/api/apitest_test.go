package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// step is one scripted response from the fake API.
type step struct {
	status  int
	body    string
	headers map[string]string
	// hang closes the connection without replying, to exercise the
	// transport-error path.
	hang bool
}

// fakeAPI is an httptest server that replays a script and records what
// it was sent. It asserts the invariants the real middleware relies on,
// so a regression in header handling fails here rather than in
// production.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	steps    []step
	idx      int
}

func newFakeAPI(t *testing.T, steps ...step) *fakeAPI {
	t.Helper()
	f := &fakeAPI{steps: steps}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Clone(r.Context()))
		var s step
		if f.idx < len(f.steps) {
			s = f.steps[f.idx]
			f.idx++
		} else if len(f.steps) > 0 {
			s = f.steps[len(f.steps)-1] // repeat the last step forever
		}
		f.mu.Unlock()

		if s.hang {
			// Hijack and close so the client sees an EOF rather than a
			// response.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
					return
				}
			}
		}

		// Every real response carries these; the client reads them for
		// pacing and for support-quotable correlation.
		w.Header().Set("x-request-id", r.Header.Get("x-request-id"))
		w.Header().Set("X-RateLimit-Limit", "120")
		w.Header().Set("X-RateLimit-Remaining", "119")
		w.Header().Set("X-RateLimit-Reset", "1750000000")
		for k, v := range s.headers {
			w.Header().Set(k, v)
		}
		if s.status == 0 {
			s.status = 200
		}
		if s.body != "" && w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(s.status)
		if s.body != "" {
			_, _ = w.Write([]byte(s.body))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeAPI) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// Problem bodies copied verbatim from the API sources, so the decoder is
// tested against what the server actually emits rather than against our
// idea of it.
const (
	// apps/api/src/middleware/scope.ts
	bodyReadOnlyScope = `{"type":"about:blank","title":"Forbidden","status":403,"detail":"API key is read-only; this endpoint requires the write:resources scope."}`
	// apps/api/src/middleware/require-service.ts
	bodyServiceDisabled = `{"type":"about:blank","title":"Forbidden","status":403,"detail":"The xcloud service is not enabled for this tenant.","code":"service_disabled","service":"xcloud"}`
	// apps/api/src/services/quotas.ts (quotaErrorToProblem)
	bodyQuotaExceeded = `{"type":"https://studio-cp.dev/errors/quota-exceeded","title":"Quota Exceeded","status":412,"detail":"Quota xcloud_instances_total exceeded.","quota":{"key":"xcloud_instances_total","limit":25,"usage":25,"requested":1}}`
	// apps/api/src/middleware/auth.ts
	bodyKeyExpired = `{"type":"about:blank","title":"Unauthorized","status":401,"detail":"api key expired"}`
	bodyKeyInvalid = `{"type":"about:blank","title":"Unauthorized","status":401,"detail":"invalid api key"}`
	// apps/api/src/routes/v1/resources.ts
	bodyBusy = `{"type":"about:blank","title":"Conflict","status":409,"detail":"Resource is busy with another lifecycle action"}`
	// apps/api/src/middleware/idempotency.ts
	bodyIdempotencyConflict = `{"type":"about:blank","title":"Conflict","status":409,"detail":"Idempotency key was already used with a different request body."}`
	// apps/api/src/routes/v1/me.ts — the 403 an api_key gets on /v1/me
	bodyNoUserContext = `{"type":"about:blank","title":"Forbidden","status":403,"detail":"API keys have no user context."}`
)

func testClient(t *testing.T, f *fakeAPI, token string) *Client {
	t.Helper()
	c, err := New(Options{Origin: f.URL, Token: token, AllowInsecure: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}
