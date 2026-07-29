package api

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"net/http"
	"time"
)

// Retry policy constants. Deliberately conservative: this is a CLI, and
// a user staring at a prompt would rather see a clear error in two
// seconds than a silent thirty-second stall.
const (
	maxAttempts      = 4
	backoffBase      = 500 * time.Millisecond
	backoffCap       = 8 * time.Second
	maxRateLimitHits = 6

	// Below this many remaining tokens the client self-paces instead of
	// racing to the 429. The bucket refills at 2/s for an API key, so
	// pacing costs almost nothing and avoids a retry storm.
	throttleThreshold = 10
	throttleInterval  = 500 * time.Millisecond
)

// retryDecision says what the transport should do with a response.
type retryDecision int

const (
	stop retryDecision = iota
	retryNow
	retryAfterHeader
)

// shouldRetry implements the policy table.
//
// The subtle entry is 429. The rate limiter is mounted globally on
// /v1/* BEFORE every route in apps/api/src/app.ts, so a 429 provably
// never reached a handler and no side effect occurred. That makes a 429
// safe to retry even for a POST that creates a billable resource — which
// is the opposite of the usual advice, and the reason this is spelled
// out rather than folded into a generic "retry 5xx and 429" rule.
//
// Everything else follows the ordinary rule: only retry a mutation when
// the server will deduplicate it, i.e. when we sent an Idempotency-Key
// to a path that honours one.
func shouldRetry(method string, status int, idempotent bool, attempt int) retryDecision {
	if attempt >= maxAttempts {
		return stop
	}

	if status == http.StatusTooManyRequests {
		return retryAfterHeader
	}

	safe := isSafeMethod(method) || idempotent
	if !safe {
		return stop
	}

	switch status {
	case http.StatusRequestTimeout, // 408
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return retryNow
	}
	return stop
}

// shouldRetryTransportError covers the case where no response was
// obtained at all (dial failure, connection reset, EOF mid-header). The
// same safety rule applies: without a response we cannot know whether
// the server processed the request, so an unguarded mutation must not
// be replayed.
func shouldRetryTransportError(method string, idempotent bool, attempt int) bool {
	if attempt >= maxAttempts {
		return false
	}
	return isSafeMethod(method) || idempotent
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// backoff returns the sleep before the given attempt using full jitter:
// sleep = rand(0, min(cap, base * 2^attempt)).
//
// Full jitter rather than exponential-plus-jitter because several CI
// runners sharing one tenant's token will otherwise re-collide in step
// after each 429 — the whole point is to spread them out.
func backoff(attempt int) time.Duration {
	exp := float64(backoffBase) * math.Pow(2, float64(attempt))
	if exp > float64(backoffCap) {
		exp = float64(backoffCap)
	}
	return time.Duration(randFloat() * exp)
}

// randFloat returns a uniform [0,1). crypto/rand keeps this package free
// of a math/rand seeding decision; the cost is irrelevant next to a
// network round-trip.
func randFloat() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Cannot read randomness: degrade to the deterministic full
		// backoff rather than to zero, which would be a busy loop.
		return 1.0
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(1<<53)
}

// retryAfterDelay decides how long to wait on a 429. The server's
// Retry-After is authoritative when present, but we never sleep for less
// than the jittered backoff, so a Retry-After: 0 cannot turn into a hot
// loop.
func retryAfterDelay(header time.Duration, attempt int) time.Duration {
	jittered := backoff(attempt)
	if header > jittered {
		return header
	}
	return jittered
}
