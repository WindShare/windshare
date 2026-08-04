package linuxsubreaper

import (
	"fmt"
	"time"

	"github.com/windshare/windshare/internal/processowner"
)

const lifecyclePollInterval = 10 * time.Millisecond

type trigger struct {
	reason string
	err    error
}

type rootObserver func() (bool, error)
type controlObserver func() (trigger, bool, error)
type lifecyclePause func(time.Duration) error

type lifecycleDecision struct {
	reason      string
	controlErr  error
	rootSettled bool
}

// awaitInitialOutcome gives an already-observable kernel exit precedence over
// queued control and the deadline. Reading each source in causal order avoids
// reporting whichever notification Go happened to schedule first.
func awaitInitialOutcome(
	deadline time.Time,
	observeRoot rootObserver,
	observeControl controlObserver,
	now func() time.Time,
	pause lifecyclePause,
) lifecycleDecision {
	for {
		settled, err := observeRoot()
		if err != nil {
			return lifecycleDecision{
				reason:     processowner.ReasonStop,
				controlErr: fmt.Errorf("observe Linux target lifecycle: phase=initial: %w", err),
			}
		}
		if settled {
			return lifecycleDecision{reason: processowner.ReasonNatural, rootSettled: true}
		}
		requested, observed, err := observeControl()
		if err != nil {
			return lifecycleDecision{
				reason:     processowner.ReasonStop,
				controlErr: fmt.Errorf("observe Linux process control: phase=initial: %w", err),
			}
		}
		if observed {
			return controlDecision(requested)
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return lifecycleDecision{reason: processowner.ReasonDeadline}
		}
		if remaining > lifecyclePollInterval {
			remaining = lifecyclePollInterval
		}
		if err := pause(remaining); err != nil {
			return lifecycleDecision{
				reason:     processowner.ReasonStop,
				controlErr: fmt.Errorf("wait for Linux lifecycle event: phase=initial: %w", err),
			}
		}
	}
}

func controlDecision(requested trigger) lifecycleDecision {
	return lifecycleDecision{reason: requested.reason, controlErr: requested.err}
}

type stableEmptyTracker struct {
	observing bool
	since     time.Time
}

// observe measures stable emptiness in elapsed time instead of successful poll
// count. Scheduler stalls therefore extend the evidence window instead of
// manufacturing a cleanup failure while the process inventory remains empty.
func (tracker *stableEmptyTracker) observe(empty bool, observedAt time.Time, minimum time.Duration) bool {
	if !empty {
		tracker.observing = false
		tracker.since = time.Time{}
		return false
	}
	if !tracker.observing {
		tracker.observing = true
		tracker.since = observedAt
	}
	return observedAt.Sub(tracker.since) >= minimum
}

func (tracker stableEmptyTracker) elapsed(observedAt time.Time) time.Duration {
	if !tracker.observing {
		return 0
	}
	elapsed := observedAt.Sub(tracker.since)
	if elapsed < 0 {
		return 0
	}
	return elapsed
}
