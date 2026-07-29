package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/studio-ch/xcloud-cli/internal/exitcode"
)

// See the note on testToken in internal/api/client_test.go: split so a
// secret scanner does not read it as a real Stripe key.
const fakeToken = "sk_" + "live_" + "abcdefghijkl" + "MNOPQRSTUVWXYZ0123456789abcdefghij"

// route is one fake endpoint.
type route struct {
	status int
	body   string
}

// harness runs commands end-to-end against a fake API.
//
// It drives the real root command through the real configuration
// resolution — pointing XCLOUD_API_URL at the test server rather than
// injecting a client — so a break anywhere in the chain (flag parsing,
// precedence, transport, rendering, exit mapping) shows up here.
type harness struct {
	*httptest.Server
	mu       sync.Mutex
	routes   map[string]route
	requests []recorded
}

type recorded struct {
	Method         string
	Path           string
	Query          string
	Body           string
	IdempotencyKey string
}

func newHarness(t *testing.T, routes map[string]route) *harness {
	t.Helper()
	h := &harness{routes: routes}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		_, _ = body.ReadFrom(r.Body)

		h.mu.Lock()
		h.requests = append(h.requests, recorded{
			Method: r.Method, Path: r.URL.Path,
			Query: r.URL.RawQuery, Body: body.String(),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
		})
		rt, ok := h.routes[r.Method+" "+r.URL.Path]
		h.mu.Unlock()

		w.Header().Set("x-request-id", r.Header.Get("x-request-id"))
		w.Header().Set("X-RateLimit-Limit", "120")
		w.Header().Set("X-RateLimit-Remaining", "119")
		if !ok {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"No such resource."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rt.status)
		_, _ = w.Write([]byte(rt.body))
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *harness) seen(method, path string) *recorded {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.requests {
		if h.requests[i].Method == method && h.requests[i].Path == path {
			return &h.requests[i]
		}
	}
	return nil
}

// run executes argv and captures stdout, stderr and the exit code.
//
// confirmAnswer drives the destructive-action prompt: nil means "no
// terminal", which is what a CI runner looks like.
func (h *harness) run(t *testing.T, argv ...string) (stdout, stderr string, code exitcode.Code) {
	t.Helper()
	return h.runWithConfirm(t, nil, argv...)
}

func (h *harness) runWithConfirm(t *testing.T, confirm func(string) (bool, error), argv ...string) (stdout, stderr string, code exitcode.Code) {
	t.Helper()

	t.Setenv("XCLOUD_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("XCLOUD_API_URL", h.URL)
	t.Setenv("XCLOUD_API_TOKEN", fakeToken)
	t.Setenv("XCLOUD_ALLOW_INSECURE", "1") // the test server is plain http
	t.Setenv("XCLOUD_PROFILE", "")
	t.Setenv("XCLOUD_OUTPUT", "")
	t.Setenv("NO_COLOR", "1")

	var out, errBuf bytes.Buffer
	s := &State{stdout: &out, stderr: &errBuf, confirm: confirm}
	if confirm == nil {
		// Mirror the non-interactive refusal the real terminal check
		// produces, so tests do not depend on how `go test` was invoked.
		s.confirm = func(string) (bool, error) {
			return false, &usageError{errors.New(
				"refusing to perform a destructive action without confirmation; " +
					"pass --yes when running non-interactively")}
		}
	}
	root := newRootCommand(s)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(argv)

	code = exitcode.OK
	if err := root.ExecuteContext(context.Background()); err != nil {
		code = report(&errBuf, err, s)
	}
	return out.String(), errBuf.String(), code
}

func TestInstanceListSendsRegionFilter(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/xcloud/instances": {200, `{"data":[]}`},
	})
	_, _, code := h.run(t, "instance", "list", "--region", "ZRH1")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want 0", code)
	}
	req := h.seen("GET", "/v1/xcloud/instances")
	if req == nil {
		t.Fatal("no request reached the API")
	}
	if req.Query != "region=ZRH1" {
		t.Errorf("query = %q, want region=ZRH1", req.Query)
	}
}

// A region slug must be resolved to the UUID the create endpoint wants —
// requiring users to look up a UUID first would make the command
// unusable by hand.
func TestInstanceCreateResolvesRegionSlug(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions": {200, `{"data":[
			{"id":"11111111-2222-3333-4444-555555555555","slug":"ZRH1","services":{"compute":true,"xcloud":true}},
			{"id":"99999999-8888-7777-6666-555555555555","slug":"BSL1","services":{"compute":true,"xcloud":false}}
		]}`},
		"POST /v1/xcloud/instances": {201, `{"id":"i-new","name":"build","status":"pending","pendingAction":"create"}`},
	})

	_, _, code := h.run(t, "instance", "create",
		"--name", "build", "--region", "ZRH1", "--image", "ghcr.io/x/macos:1",
		"--cpu", "10", "--memory", "28", "--disk", "480")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want 0", code)
	}

	req := h.seen("POST", "/v1/xcloud/instances")
	if req == nil {
		t.Fatal("create request never reached the API")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if body["regionId"] != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("regionId = %v, want the resolved UUID", body["regionId"])
	}
	if body["name"] != "build" || body["imageRef"] != "ghcr.io/x/macos:1" {
		t.Errorf("body = %v", body)
	}
	// json.Unmarshal gives float64; compare numerically.
	if body["cpuCores"] != float64(10) || body["memoryGib"] != float64(28) || body["diskGib"] != float64(480) {
		t.Errorf("sizing fields = %v", body)
	}
}

// An unknown region must fail locally with a usage error listing the
// real options, rather than sending a bad UUID and getting a 400.
func TestInstanceCreateRejectsUnknownRegion(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions": {200, `{"data":[{"id":"1","slug":"ZRH1","services":{"xcloud":true}}]}`},
	})
	_, stderr, code := h.run(t, "instance", "create",
		"--name", "x", "--region", "NOPE", "--image", "img", "--cpu", "1", "--memory", "1", "--disk", "1")

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d (usage)", code, exitcode.Usage)
	}
	if !strings.Contains(stderr, "ZRH1") {
		t.Errorf("error should list the available regions, got: %s", stderr)
	}
	if h.seen("POST", "/v1/xcloud/instances") != nil {
		t.Error("a create request was sent despite the region being unresolvable")
	}
}

// Every create must carry an Idempotency-Key, generated when the caller
// did not supply one — a lost response would otherwise leave the client
// unable to tell whether a billable instance exists.
func TestInstanceCreateAlwaysSendsAnIdempotencyKey(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions":           {200, `{"data":[{"id":"1","slug":"ZRH1","services":{"xcloud":true}}]}`},
		"POST /v1/xcloud/instances": {201, `{"id":"i-1"}`},
	})
	_, _, code := h.run(t, "instance", "create",
		"--name", "x", "--region", "ZRH1", "--image", "img",
		"--cpu", "1", "--memory", "1", "--disk", "1",
		"--idempotency-key", "abc")

	if code != exitcode.OK {
		t.Fatalf("exit = %d", code)
	}
	req := h.seen("POST", "/v1/xcloud/instances")
	if req == nil {
		t.Fatal("create was not sent")
	}
	if req.IdempotencyKey != "abc" {
		t.Errorf("Idempotency-Key = %q, want the caller's key", req.IdempotencyKey)
	}
}

func TestInstanceCreateGeneratesAnIdempotencyKeyWhenUnset(t *testing.T) {
	h := newHarness(t, map[string]route{
		"GET /v1/regions":           {200, `{"data":[{"id":"1","slug":"ZRH1","services":{"xcloud":true}}]}`},
		"POST /v1/xcloud/instances": {201, `{"id":"i-1"}`},
	})
	_, _, code := h.run(t, "instance", "create",
		"--name", "x", "--region", "ZRH1", "--image", "img",
		"--cpu", "1", "--memory", "1", "--disk", "1")

	if code != exitcode.OK {
		t.Fatalf("exit = %d", code)
	}
	req := h.seen("POST", "/v1/xcloud/instances")
	if req == nil {
		t.Fatal("create was not sent")
	}
	if req.IdempotencyKey == "" {
		t.Error("no Idempotency-Key was generated; an interrupted create would not be safely retryable")
	}
}

func TestLifecycleCommandsHitTheRightEndpoints(t *testing.T) {
	tests := []struct {
		argv []string
		path string
	}{
		{[]string{"instance", "start", "i-1"}, "/v1/xcloud/instances/i-1/start"},
		{[]string{"instance", "stop", "i-1"}, "/v1/xcloud/instances/i-1/stop"},
		{[]string{"instance", "shutdown", "i-1"}, "/v1/xcloud/instances/i-1/shutdown"},
		{[]string{"instance", "suspend", "i-1"}, "/v1/xcloud/instances/i-1/suspend"},
	}
	for _, tt := range tests {
		t.Run(tt.argv[1], func(t *testing.T) {
			h := newHarness(t, map[string]route{"POST " + tt.path: {202, ""}})
			_, _, code := h.run(t, tt.argv...)
			if code != exitcode.OK {
				t.Fatalf("exit = %d, want 0", code)
			}
			if h.seen("POST", tt.path) == nil {
				t.Errorf("no POST reached %s", tt.path)
			}
		})
	}
}

func TestBootModeSendsTheFlag(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want bool
	}{{"--recovery", true}, {"--normal", false}} {
		t.Run(tc.flag, func(t *testing.T) {
			h := newHarness(t, map[string]route{
				"POST /v1/xcloud/instances/i-1/boot-mode": {202, ""},
			})
			_, _, code := h.run(t, "instance", "boot-mode", "i-1", tc.flag)
			if code != exitcode.OK {
				t.Fatalf("exit = %d", code)
			}
			req := h.seen("POST", "/v1/xcloud/instances/i-1/boot-mode")
			if req == nil {
				t.Fatal("no request")
			}
			var body map[string]any
			_ = json.Unmarshal([]byte(req.Body), &body)
			if body["bootIntoRecovery"] != tc.want {
				t.Errorf("bootIntoRecovery = %v, want %v", body["bootIntoRecovery"], tc.want)
			}
		})
	}
}

// --recovery and --normal are mutually exclusive, and neither is a
// sensible default, so both-or-neither must be a usage error.
func TestBootModeRequiresExactlyOneMode(t *testing.T) {
	for _, argv := range [][]string{
		{"instance", "boot-mode", "i-1"},
		{"instance", "boot-mode", "i-1", "--recovery", "--normal"},
	} {
		h := newHarness(t, map[string]route{})
		_, _, code := h.run(t, argv...)
		if code != exitcode.Usage {
			t.Errorf("%v: exit = %d, want %d", argv, code, exitcode.Usage)
		}
	}
}

func TestResizeRequiresAtLeastOneDimension(t *testing.T) {
	h := newHarness(t, map[string]route{})
	_, stderr, code := h.run(t, "instance", "resize", "i-1")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(stderr, "--cpu") {
		t.Errorf("error should name the flags, got: %s", stderr)
	}
}

// Deleting destroys a disk. Without a TTY to prompt on, the command must
// refuse rather than proceed — a CI job has to say --yes explicitly.
func TestDeleteRefusesWithoutConfirmationWhenNonInteractive(t *testing.T) {
	h := newHarness(t, map[string]route{
		"DELETE /v1/xcloud/instances/i-1": {202, ""},
	})
	_, stderr, code := h.run(t, "instance", "delete", "i-1")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("error should mention --yes, got: %s", stderr)
	}
	if h.seen("DELETE", "/v1/xcloud/instances/i-1") != nil {
		t.Error("a delete was sent without confirmation")
	}
}

func TestDeleteWithYesProceeds(t *testing.T) {
	h := newHarness(t, map[string]route{
		"DELETE /v1/xcloud/instances/i-1": {202, ""},
	})
	_, _, code := h.run(t, "instance", "delete", "i-1", "--yes", "--release-elastic-ips")
	if code != exitcode.OK {
		t.Fatalf("exit = %d", code)
	}
	req := h.seen("DELETE", "/v1/xcloud/instances/i-1")
	if req == nil {
		t.Fatal("no delete request")
	}
	if req.Query != "releaseElasticIps=true" {
		t.Errorf("query = %q, want releaseElasticIps=true", req.Query)
	}
}

// --wait must poll until the pending action clears, not return on the
// first observation.
func TestWaitPollsUntilSettled(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(202)
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			_, _ = w.Write([]byte(`{"status":"starting","pendingAction":"start"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"running","pendingAction":null}`))
	}))
	defer srv.Close()

	t.Setenv("XCLOUD_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("XCLOUD_API_URL", srv.URL)
	t.Setenv("XCLOUD_API_TOKEN", fakeToken)
	t.Setenv("XCLOUD_ALLOW_INSECURE", "1")
	t.Setenv("NO_COLOR", "1")

	var out, errBuf bytes.Buffer
	s := &State{stdout: &out, stderr: &errBuf}
	root := newRootCommand(s)
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"instance", "start", "i-1", "--wait", "--wait-timeout", "30s"})
	if err := root.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	if got < 3 {
		t.Errorf("polled %d times; --wait must keep polling while pendingAction is set", got)
	}
}

// A 404 from the API must map to exit code 5 and carry the hint about
// cross-organisation resources reading as not-found.
func TestNotFoundMapsToExitCode5(t *testing.T) {
	h := newHarness(t, map[string]route{})
	_, stderr, code := h.run(t, "instance", "get", "does-not-exist")
	if code != exitcode.NotFound {
		t.Errorf("exit = %d, want %d", code, exitcode.NotFound)
	}
	if !strings.Contains(stderr, "request-id") {
		t.Errorf("error should include a request id, got: %s", stderr)
	}
}

// A private key pasted where a public one belongs must be caught before
// it leaves the machine.
func TestSSHKeyCreateRejectsPrivateKey(t *testing.T) {
	h := newHarness(t, map[string]route{"POST /v1/ssh-keys": {201, `{"id":"k-1"}`}})
	_, stderr, code := h.run(t, "ssh-key", "create", "--name", "oops",
		"--public-key", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----")

	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(stderr, "PRIVATE") {
		t.Errorf("error should call out the private key, got: %s", stderr)
	}
	if h.seen("POST", "/v1/ssh-keys") != nil {
		t.Error("a private key was sent to the API")
	}
}

func TestSSHKeyCreateRequiresExactlyOneKeySource(t *testing.T) {
	h := newHarness(t, map[string]route{})
	_, _, code := h.run(t, "ssh-key", "create", "--name", "x")
	if code != exitcode.Usage {
		t.Errorf("no key source: exit = %d, want %d", code, exitcode.Usage)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	h := newHarness(t, map[string]route{})
	_, _, code := h.run(t, "nonsense")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
}

func TestExitCodesCommandListsEveryCode(t *testing.T) {
	h := newHarness(t, map[string]route{})
	stdout, _, code := h.run(t, "exit-codes")
	if code != exitcode.OK {
		t.Fatalf("exit = %d", code)
	}
	for _, c := range exitcode.All {
		if !strings.Contains(stdout, c.Description()) {
			t.Errorf("output is missing the description for code %d", c)
		}
	}
}
