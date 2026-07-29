package wait

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scripted returns a Fetch that walks a fixed sequence of observations.
func scripted(states ...State) func(context.Context) (State, error) {
	i := 0
	return func(context.Context) (State, error) {
		s := states[i]
		if i < len(states)-1 {
			i++
		}
		return s, nil
	}
}

func TestWaitsUntilPendingActionClears(t *testing.T) {
	fetch := scripted(
		State{Exists: true, Status: StatusPending, PendingAction: "create"},
		State{Exists: true, Status: StatusProvisioning, PendingAction: "create"},
		State{Exists: true, Status: StatusRunning, PendingAction: "create"},
		State{Exists: true, Status: StatusRunning},
	)
	got, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceRunning())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got.Status != StatusRunning || !got.Settled() {
		t.Errorf("final state = %+v, want a settled running instance", got)
	}
}

// A row can carry its final status while the pending action is still
// set. Succeeding on the status alone would return before the worker is
// finished, and the very next command would hit a 409.
func TestDoesNotSucceedWhileActionStillPending(t *testing.T) {
	calls := 0
	fetch := func(context.Context) (State, error) {
		calls++
		if calls < 3 {
			return State{Exists: true, Status: StatusRunning, PendingAction: "start"}, nil
		}
		return State{Exists: true, Status: StatusRunning}, nil
	}
	if _, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceRunning()); err != nil {
		t.Fatalf("For: %v", err)
	}
	if calls < 3 {
		t.Errorf("returned after %d polls; must keep polling while pendingAction is set", calls)
	}
}

func TestErrorStatusFailsImmediately(t *testing.T) {
	fetch := scripted(State{Exists: true, Status: StatusError, LastError: "no suitable node for VM"})
	_, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceRunning())

	var failed *FailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v, want a *FailedError", err)
	}
	// The server-side reason is usually the only explanation the user
	// gets; losing it would make the failure undiagnosable.
	if failed.Last.LastError != "no suitable node for VM" {
		t.Errorf("lastError was not surfaced: %+v", failed.Last)
	}
}

func TestDeleteSucceedsOnNotFound(t *testing.T) {
	fetch := scripted(
		State{Exists: true, Status: StatusDeleting, PendingAction: "delete"},
		State{Exists: false},
	)
	if _, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceGone()); err != nil {
		t.Fatalf("delete wait should succeed on a 404, got: %v", err)
	}
}

func TestSuspendWaitsForSuspended(t *testing.T) {
	fetch := scripted(
		State{Exists: true, Status: StatusSuspending, PendingAction: "suspend"},
		State{Exists: true, Status: StatusSuspended},
	)
	got, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceSuspended())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if got.Status != StatusSuspended {
		t.Errorf("status = %q, want suspended", got.Status)
	}
}

// A resize leaves the instance in whichever steady state it started
// from, so the predicate must accept both.
func TestSettledAcceptsRunningOrStopped(t *testing.T) {
	for _, final := range []string{StatusRunning, StatusStopped} {
		fetch := scripted(
			State{Exists: true, Status: StatusResizing, PendingAction: "resize"},
			State{Exists: true, Status: final},
		)
		if _, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceSettled()); err != nil {
			t.Errorf("final=%s: %v", final, err)
		}
	}
}

func TestTimeoutReportsTheStateItGaveUpOn(t *testing.T) {
	fetch := scripted(State{Exists: true, Status: StatusProvisioning, PendingAction: "create"})
	_, err := For(context.Background(), Options{Timeout: 1500 * time.Millisecond, Fetch: fetch}, InstanceRunning())

	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v, want a *TimeoutError", err)
	}
	if timeout.Last.PendingAction != "create" {
		t.Errorf("timeout should report the last observed state, got %+v", timeout.Last)
	}
	// The message must say the operation may still be running — a
	// timeout is not a failed mutation.
	if timeout.Error() == "" {
		t.Error("timeout error has no message")
	}
}

func TestContextCancellationEndsTheWait(t *testing.T) {
	fetch := scripted(State{Exists: true, Status: StatusProvisioning, PendingAction: "create"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := For(ctx, Options{Timeout: time.Minute, Fetch: fetch}, InstanceRunning()); err == nil {
		t.Error("expected an error after cancellation")
	}
}

// A resource vanishing under a non-delete operation must end the wait
// rather than poll a 404 until the deadline.
func TestVanishedResourceEndsNonDeleteWait(t *testing.T) {
	fetch := scripted(State{Exists: false})
	if _, err := For(context.Background(), Options{Timeout: 30 * time.Second, Fetch: fetch}, InstanceRunning()); err != nil {
		t.Fatalf("a vanished resource should end the wait, got: %v", err)
	}
}

func TestIsTransient(t *testing.T) {
	transient := []string{
		StatusPending, StatusProvisioning, StatusStopping, StatusStarting,
		StatusDeleting, StatusResizing, StatusSuspending, StatusOffloading,
	}
	settled := []string{
		StatusRunning, StatusStopped, StatusSuspended, StatusOffloaded, StatusError,
	}
	for _, s := range transient {
		if !IsTransient(s) {
			t.Errorf("IsTransient(%q) = false, want true", s)
		}
	}
	for _, s := range settled {
		if IsTransient(s) {
			t.Errorf("IsTransient(%q) = true, want false", s)
		}
	}
}

func TestProgressIsCalledForEveryObservation(t *testing.T) {
	fetch := scripted(
		State{Exists: true, Status: StatusPending, PendingAction: "create"},
		State{Exists: true, Status: StatusRunning},
	)
	seen := 0
	_, err := For(context.Background(), Options{
		Timeout:  30 * time.Second,
		Fetch:    fetch,
		Progress: func(State, time.Duration) { seen++ },
	}, InstanceRunning())
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if seen != 2 {
		t.Errorf("Progress called %d times, want 2", seen)
	}
}

func TestIntervalStaysWithinBounds(t *testing.T) {
	// The cap matters: an API key's bucket refills at 2 req/s, and the
	// poll interval is what keeps a long wait from consuming it.
	for attempt := 0; attempt < 20; attempt++ {
		d := interval(attempt)
		if d < 700*time.Millisecond || d > 6500*time.Millisecond {
			t.Errorf("interval(%d) = %v, outside the expected 0.8s..6s band", attempt, d)
		}
	}
}
