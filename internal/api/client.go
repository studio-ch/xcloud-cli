package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/studio-ch/cloudconsole-cli/internal/buildinfo"
)

// Client is the CLI's HTTP client against the Cloud Console API.
//
// It owns exactly one credential and one origin — an API key is bound
// server-side to a single tenant (the x-tenant-id header is ignored for
// api_key actors), so there is no tenant switching to model here.
type Client struct {
	origin string
	token  string
	http   *http.Client
	tracer *Tracer

	// throttle serialises the self-pacing sleep so concurrent requests
	// from one process cannot collectively blow through the bucket.
	throttleMu   sync.Mutex
	throttleNext time.Time
}

// Options configures a Client.
type Options struct {
	Origin  string // e.g. https://api.cloud.flow.swiss (no /v1)
	Token   string
	Timeout time.Duration // per attempt; 0 means the default
	Tracer  *Tracer       // nil disables tracing
	// AllowInsecure permits a plaintext http:// origin to a non-loopback
	// host. Off by default: a bearer key must not travel in the clear.
	AllowInsecure bool
}

const defaultTimeout = 30 * time.Second

// New builds a Client, validating the origin up front so a typo in a
// profile surfaces immediately rather than as a confusing dial error.
func New(o Options) (*Client, error) {
	origin := strings.TrimRight(o.Origin, "/")
	origin = strings.TrimSuffix(origin, "/v1")
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL %q: %w", o.Origin, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("invalid API URL %q: expected an http:// or https:// origin", o.Origin)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid API URL %q: no host", o.Origin)
	}
	if u.Scheme == "http" && !o.AllowInsecure && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf(
			"refusing to send an API key over plaintext http:// to %s — "+
				"use https://, or set CLOUDCONSOLE_ALLOW_INSECURE=1 if you really mean it",
			u.Host)
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return &Client{
		origin: origin,
		token:  o.Token,
		tracer: o.Tracer,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: timeout,
			},
		},
	}, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Origin returns the configured API origin (without /v1).
func (c *Client) Origin() string { return c.origin }

// Response is a completed API call.
//
// Body is retained verbatim. That is deliberate and load-bearing: the
// CLI's `--output json` contract is to emit the server's payload
// unchanged, so that every jq recipe transfers 1:1 between curl and
// cloudconsole. Decoding into a Go struct and re-encoding would silently drop
// any field the committed spec snapshot does not yet model.
type Response struct {
	Status    int
	Body      []byte
	Header    http.Header
	RequestID string
	RateLimit RateLimit
	Attempts  int
}

// RequestOptions are the per-call knobs.
type RequestOptions struct {
	Method string
	// Path is API-relative and must start with /v1.
	Path  string
	Query url.Values
	Body  []byte
	// ContentType defaults to application/json when Body is non-empty.
	ContentType string
	// IdempotencyKey is sent only if the target path actually has the
	// idempotency middleware mounted. Callers should not pre-filter;
	// Do() reports what it did through the returned Response.
	IdempotencyKey string
}

// Do performs a request with the retry policy applied.
//
// A non-2xx response is returned as a *Problem error, not as a Response
// with a status — so callers cannot accidentally treat a 403 as success
// by forgetting to check.
func (c *Client) Do(ctx context.Context, o RequestOptions) (*Response, error) {
	target := c.origin + o.Path
	if len(o.Query) > 0 {
		target += "?" + o.Query.Encode()
	}

	sendIdemKey := o.IdempotencyKey != "" && SupportsIdempotencyKey(o.Path)
	idempotent := sendIdemKey

	var rateLimitHits int
	for attempt := 0; ; attempt++ {
		c.waitForThrottle(ctx)

		reqID := newRequestID()
		req, err := http.NewRequestWithContext(ctx, o.Method, target, bodyReader(o.Body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		req.Header.Set("User-Agent", buildinfo.UserAgent())
		req.Header.Set("Accept", "application/json")
		// Generated client-side rather than read off the response. This
		// way a support-quotable id exists even when the request times
		// out, TLS fails, or a proxy returns a body we cannot parse —
		// exactly the cases where the user most needs one. The API
		// echoes a supplied x-request-id verbatim.
		req.Header.Set("x-request-id", reqID)
		if len(o.Body) > 0 {
			ct := o.ContentType
			if ct == "" {
				ct = "application/json"
			}
			req.Header.Set("Content-Type", ct)
		}
		if sendIdemKey {
			req.Header.Set("Idempotency-Key", o.IdempotencyKey)
		}

		c.tracer.request(req, attempt+1)
		started := time.Now()
		resp, err := c.http.Do(req)
		if err != nil {
			c.tracer.transportError(reqID, err, time.Since(started))
			if shouldRetryTransportError(o.Method, idempotent, attempt+1) && ctx.Err() == nil {
				d := backoff(attempt)
				c.tracer.retrying(d, attempt+2)
				if sleepCtx(ctx, d) {
					continue
				}
			}
			return nil, &TransportError{RequestID: reqID, Method: o.Method, URL: target, Err: err, Attempts: attempt + 1}
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			c.tracer.transportError(reqID, readErr, time.Since(started))
			if shouldRetryTransportError(o.Method, idempotent, attempt+1) && ctx.Err() == nil {
				d := backoff(attempt)
				c.tracer.retrying(d, attempt+2)
				if sleepCtx(ctx, d) {
					continue
				}
			}
			return nil, &TransportError{RequestID: reqID, Method: o.Method, URL: target, Err: readErr, Attempts: attempt + 1}
		}

		rl := parseRateLimit(resp.Header)
		c.tracer.response(reqID, resp, rl, time.Since(started))
		c.noteRateLimit(rl)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &Response{
				Status:    resp.StatusCode,
				Body:      body,
				Header:    resp.Header,
				RequestID: firstNonEmpty(resp.Header.Get("x-request-id"), reqID),
				RateLimit: rl,
				Attempts:  attempt + 1,
			}, nil
		}

		problem := parseProblem(resp, body, o.Method, target, attempt+1)
		if problem.RequestID == "" {
			problem.RequestID = reqID
		}

		switch shouldRetry(o.Method, resp.StatusCode, idempotent, attempt+1) {
		case retryAfterHeader:
			rateLimitHits++
			// A 429 costs an attempt from a separate, larger budget: a
			// shared CI runner legitimately hits the 2/s refill, and
			// failing after four tries there would be needlessly brittle.
			if rateLimitHits > maxRateLimitHits || ctx.Err() != nil {
				return nil, problem
			}
			attempt-- // does not consume the general retry budget
			d := retryAfterDelay(problem.RetryAfter, rateLimitHits)
			c.tracer.retrying(d, rateLimitHits+1)
			if !sleepCtx(ctx, d) {
				return nil, problem
			}
		case retryNow:
			if ctx.Err() != nil {
				return nil, problem
			}
			d := backoff(attempt)
			c.tracer.retrying(d, attempt+2)
			if !sleepCtx(ctx, d) {
				return nil, problem
			}
		default:
			return nil, problem
		}
	}
}

// noteRateLimit self-paces once the bucket runs low, rather than racing
// into a 429 and paying the retry cost.
func (c *Client) noteRateLimit(rl RateLimit) {
	if rl.Remaining < 0 || rl.Remaining >= throttleThreshold {
		return
	}
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	next := time.Now().Add(throttleInterval)
	if next.After(c.throttleNext) {
		c.throttleNext = next
	}
}

func (c *Client) waitForThrottle(ctx context.Context) {
	c.throttleMu.Lock()
	wait := time.Until(c.throttleNext)
	c.throttleMu.Unlock()
	if wait > 0 {
		sleepCtx(ctx, wait)
	}
}

// sleepCtx sleeps unless the context is cancelled first. Returns true if
// the full duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func bodyReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// newRequestID returns a random 128-bit hex id. Not a UUID: the API
// treats it as an opaque string, and this avoids a dependency for
// sixteen bytes of randomness.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cloudconsole-cli-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// TransportError is returned when no HTTP response was obtained.
type TransportError struct {
	RequestID string
	Method    string
	URL       string
	Attempts  int
	Err       error
}

func (e *TransportError) Error() string {
	host := e.URL
	if u, err := url.Parse(e.URL); err == nil && u.Host != "" {
		host = u.Host
	}
	return fmt.Sprintf("could not reach %s: %v", host, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// IsTimeout reports whether the failure was a deadline rather than a
// refusal, so the caller can suggest --timeout instead of "check your
// network".
func (e *TransportError) IsTimeout() bool {
	var terr interface{ Timeout() bool }
	return errors.As(e.Err, &terr) && terr.Timeout()
}
