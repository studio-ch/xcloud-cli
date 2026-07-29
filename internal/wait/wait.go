// Package wait polls a resource until an operation finishes.
//
// Nearly every mutation in the API is asynchronous: the endpoint returns
// 202, sets a row-level `pendingAction`, and a worker does the real
// work. There is no completion callback and — for the Xcloud stack — no
// event stream, so the only way to know an operation finished is to read
// the resource back until `pendingAction` clears and the status settles.
//
// A second action attempted while one is pending returns 409, which is
// why `--wait` is not merely a convenience: without it, scripting two
// operations in a row is a race.
package wait

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// Outcome is how a wait finished.
type Outcome int

const (
	// Succeeded means the resource reached its expected terminal state.
	Succeeded Outcome = iota
	// Failed means the resource reached a terminal error state.
	Failed
	// Gone means the resource no longer exists — success for a delete.
	Gone
)

// State is one observation of a resource.
type State struct {
	Status        string
	PendingAction string
	// LastError is surfaced when the wait ends in Failed; the API sets
	// it on the row, and it is usually the only explanation available.
	LastError string
	// Exists is false when the fetch returned 404.
	Exists bool
}

// Settled reports whether the worker has finished with this row.
func (s State) Settled() bool { return s.PendingAction == "" }

// Predicate decides whether an observed state ends the wait.
type Predicate func(State) (Outcome, bool)

// Options configures a wait.
type Options struct {
	Timeout time.Duration
	// Fetch reads the current state. A 404 must be reported as
	// State{Exists: false}, not as an error, so a delete can complete.
	Fetch func(context.Context) (State, error)
	// Progress is called on every observation, for a status line. It is
	// always written to stderr by the caller, never stdout.
	Progress func(State, time.Duration)
}

// DefaultTimeout is generous on purpose: a macOS guest's graceful
// shutdown alone is budgeted at roughly ten minutes server-side, and a
// first-boot provision can exceed that.
const DefaultTimeout = 15 * time.Minute

// TimeoutError reports that the deadline passed while an operation was
// still in flight. It is explicitly not a failure of the operation.
type TimeoutError struct {
	Elapsed time.Duration
	Last    State
}

func (e *TimeoutError) Error() string {
	if e.Last.PendingAction != "" {
		return fmt.Sprintf("timed out after %s; the resource is still %s (status %s)",
			e.Elapsed.Round(time.Second), e.Last.PendingAction, e.Last.Status)
	}
	return fmt.Sprintf("timed out after %s (status %s)", e.Elapsed.Round(time.Second), e.Last.Status)
}

// FailedError reports that the resource reached an error state.
type FailedError struct {
	Last State
}

func (e *FailedError) Error() string {
	if e.Last.LastError != "" {
		return fmt.Sprintf("the operation failed: %s", e.Last.LastError)
	}
	return fmt.Sprintf("the operation failed (status %s)", e.Last.Status)
}

// For polls until the predicate settles, the deadline passes, or the
// context is cancelled.
//
// The backoff ramps 1s → 2s → 3s → 5s and then holds, with ±20% jitter.
// Holding at 5s rather than growing further is deliberate: an API key's
// bucket refills at 2 requests per second, so a 5s interval costs 0.1
// req/s and several concurrent waits still fit comfortably — while a
// longer interval would add pointless latency to short operations.
func For(ctx context.Context, opts Options, predicate Predicate) (State, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()
	var last State

	for attempt := 0; ; attempt++ {
		state, err := opts.Fetch(ctx)
		if err != nil {
			// A cancelled or expired context during the fetch is the
			// deadline, not a transport failure.
			if ctx.Err() != nil {
				return last, &TimeoutError{Elapsed: time.Since(started), Last: last}
			}
			return last, err
		}
		last = state

		if opts.Progress != nil {
			opts.Progress(state, time.Since(started))
		}

		if outcome, done := predicate(state); done {
			switch outcome {
			case Failed:
				return state, &FailedError{Last: state}
			default:
				return state, nil
			}
		}

		select {
		case <-time.After(interval(attempt)):
		case <-ctx.Done():
			return last, &TimeoutError{Elapsed: time.Since(started), Last: last}
		}
	}
}

func interval(attempt int) time.Duration {
	base := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		5 * time.Second,
	}
	d := base[len(base)-1]
	if attempt < len(base) {
		d = base[attempt]
	}
	// ±20% jitter, so concurrent waits from one CI job do not lock step.
	jitter := time.Duration(rand.Float64()*0.4*float64(d)) - time.Duration(0.2*float64(d))
	return d + jitter
}
