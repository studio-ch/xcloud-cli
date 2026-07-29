package wait

// Terminal-state predicates for the Xcloud compute stack.
//
// The status vocabulary is defined in
// packages/shared/src/types/xcloud-instance.ts. It differs materially
// from the classic Incus stack's (which has no suspended, offloaded or
// resizing), which is why the two stacks get separate predicates rather
// than a shared abstraction that would be wrong for both.

// Xcloud instance statuses.
const (
	StatusPending      = "pending"
	StatusProvisioning = "provisioning"
	StatusRunning      = "running"
	StatusStopping     = "stopping"
	StatusStopped      = "stopped"
	StatusStarting     = "starting"
	StatusDeleting     = "deleting"
	StatusResizing     = "resizing"
	StatusSuspending   = "suspending"
	StatusSuspended    = "suspended"
	StatusOffloading   = "offloading"
	StatusOffloaded    = "offloaded"
	StatusError        = "error"
)

// settledIn returns a predicate that succeeds once the worker is done
// with the row and the status is one of `want`.
//
// Both conditions matter. `pendingAction == ""` alone is not enough: the
// row is written with its new status before the pending action clears in
// some paths, and a status check alone would race. Requiring both means
// the observation is of a genuinely settled row.
func settledIn(want ...string) Predicate {
	set := make(map[string]bool, len(want))
	for _, w := range want {
		set[w] = true
	}
	return func(s State) (Outcome, bool) {
		if !s.Exists {
			// The row vanished under a non-delete operation. Treat it as
			// terminal rather than polling a 404 until the deadline.
			return Gone, true
		}
		if s.Status == StatusError {
			return Failed, true
		}
		if !s.Settled() {
			return Succeeded, false
		}
		if set[s.Status] {
			return Succeeded, true
		}
		return Succeeded, false
	}
}

// InstanceRunning waits for create, start and resume.
//
// `offloaded` counts as terminal-but-unexpected rather than success: it
// is a worker-driven idle-parking state, not something the user asked
// for, and reporting "started" for a parked VM would be a lie. The
// caller sees the real status and can start it again.
func InstanceRunning() Predicate { return settledIn(StatusRunning) }

// InstanceStopped waits for stop and shutdown.
func InstanceStopped() Predicate { return settledIn(StatusStopped) }

// InstanceSuspended waits for suspend-to-disk.
func InstanceSuspended() Predicate { return settledIn(StatusSuspended) }

// InstanceSettled waits for an operation that returns the instance to
// whichever steady state it was in — resize and boot-mode changes keep a
// stopped instance stopped and a running one running.
func InstanceSettled() Predicate {
	return settledIn(StatusRunning, StatusStopped, StatusSuspended, StatusOffloaded)
}

// InstanceGone waits for a delete. Success is a 404: the row is reaped
// once the upstream VM is torn down, so its absence is the completion
// signal.
func InstanceGone() Predicate {
	return func(s State) (Outcome, bool) {
		if !s.Exists {
			return Gone, true
		}
		if s.Status == StatusError {
			return Failed, true
		}
		return Succeeded, false
	}
}

// IsTransient reports whether a status means "something is happening".
// Used to decide whether waiting is worthwhile at all.
func IsTransient(status string) bool {
	switch status {
	case StatusPending, StatusProvisioning, StatusStopping, StatusStarting,
		StatusDeleting, StatusResizing, StatusSuspending, StatusOffloading:
		return true
	}
	return false
}
