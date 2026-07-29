// Package exitcode defines the CLI's process exit codes.
//
// These are a PUBLIC CONTRACT from v0.1.0 onward: customers branch on
// them in CI pipelines, so a value's meaning must never be reused or
// renumbered. New conditions get new numbers. The table is mirrored in
// docs/cli.md and asserted one-case-per-row in exitcode_test.go — if you
// change anything here, all three move together.
//
// Sysexits.h values (64-78) are deliberately avoided: they collide with
// what shells and supervisors already ascribe meaning to, and the BSD
// definitions do not map onto HTTP failure modes.
package exitcode

// Code is a process exit status.
type Code int

const (
	// OK is success.
	OK Code = 0

	// Unexpected is an internal CLI error: a bug, a panic recovered at
	// the top level, or a state we did not anticipate. It is never used
	// for a condition the API reported.
	Unexpected Code = 1

	// Usage is a bad invocation — unknown command, unknown flag, missing
	// or malformed argument, or a client-side type mismatch (asking for
	// `compute vm start` on a volume id). 2 matches Cobra's own default
	// for flag parse errors, so we adopt it rather than fight it.
	Usage Code = 2

	// Auth means the API rejected the credential (HTTP 401): no key, a
	// malformed key, a revoked key, or an expired one.
	Auth Code = 3

	// Forbidden means the credential was accepted but is not allowed to
	// do this (HTTP 403): a read-only key attempting a mutation, or a
	// service disabled for the tenant.
	Forbidden Code = 4

	// NotFound is HTTP 404 or 410. Note that a resource belonging to a
	// different tenant also lands here — the API returns 404 rather than
	// 403 so it never confirms existence across tenant boundaries.
	NotFound Code = 5

	// Conflict is HTTP 409: the resource is busy with another lifecycle
	// action, or an Idempotency-Key was replayed with a different body.
	Conflict Code = 6

	// Precondition is HTTP 412 — most commonly a quota that would be
	// exceeded, but also other precondition failures.
	Precondition Code = 7

	// Invalid is HTTP 400 or 422: the request was well-formed HTTP but
	// the API rejected its content.
	Invalid Code = 8

	// RateLimited means we exhausted our retry budget against HTTP 429.
	RateLimited Code = 9

	// Server means we exhausted our retry budget against a 5xx.
	Server Code = 10

	// Network means no HTTP response was obtained at all: DNS failure,
	// connection refused, TLS error, or timeout.
	Network Code = 11

	// WaitTimeout means --wait hit its deadline while the resource was
	// still transitioning. The operation itself may yet succeed; this is
	// explicitly not a failure of the mutation.
	WaitTimeout Code = 12

	// Config is a local configuration or credential problem: no profile,
	// an unreadable or over-permissive config file, or a token_command
	// that failed.
	Config Code = 13
)

// Int returns the code as the int that os.Exit wants.
func (c Code) Int() int { return int(c) }

// Description returns the one-line meaning, used by
// `cloudconsole help exit-codes` so the contract is discoverable from the
// binary itself and not only from the docs.
func (c Code) Description() string {
	switch c {
	case OK:
		return "success"
	case Unexpected:
		return "unexpected CLI error"
	case Usage:
		return "usage error (unknown command, bad flag or argument)"
	case Auth:
		return "authentication failed (HTTP 401)"
	case Forbidden:
		return "permission denied (HTTP 403: missing scope or disabled service)"
	case NotFound:
		return "not found (HTTP 404/410)"
	case Conflict:
		return "conflict (HTTP 409: resource busy, or idempotency key reuse)"
	case Precondition:
		return "precondition failed (HTTP 412, e.g. quota exceeded)"
	case Invalid:
		return "invalid request (HTTP 400/422)"
	case RateLimited:
		return "rate limited, retries exhausted (HTTP 429)"
	case Server:
		return "server error, retries exhausted (HTTP 5xx)"
	case Network:
		return "network error or timeout (no HTTP response)"
	case WaitTimeout:
		return "--wait deadline exceeded"
	case Config:
		return "configuration or credential problem"
	default:
		return "unknown"
	}
}

// All is every code in ascending order. `cloudconsole help exit-codes` renders
// it, and the test suite iterates it to guarantee every code has both a
// description and a covering case.
var All = []Code{
	OK, Unexpected, Usage, Auth, Forbidden, NotFound, Conflict,
	Precondition, Invalid, RateLimited, Server, Network, WaitTimeout, Config,
}

// FromHTTPStatus maps an HTTP status to its exit code. It is the single
// place that mapping lives; callers that have already classified an
// error (a wait timeout, a config problem) must not route through here.
//
// 402, 405, 415 and friends have no dedicated code by design: they mean
// the CLI sent something the API does not accept, which is Invalid from
// the user's point of view.
func FromHTTPStatus(status int) Code {
	switch {
	case status == 401:
		return Auth
	case status == 403:
		return Forbidden
	case status == 404, status == 410:
		return NotFound
	case status == 409:
		return Conflict
	case status == 412:
		return Precondition
	case status == 429:
		return RateLimited
	case status >= 500:
		return Server
	case status >= 400:
		return Invalid
	default:
		return OK
	}
}
