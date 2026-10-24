package lanescheduling

import "time"

const (
	ProbeInterval      = 5 * time.Second
	MinimumRescueDelay = 10 * time.Millisecond
	HedgeDelayFactor   = 2
	MaximumSupplements = 2
)

type Purpose string

const (
	Content Purpose = "content"
	Probe   Purpose = "probe"
	Sample  Purpose = "sample"
	Rescue  Purpose = "rescue"
)

// Exploration grants at most one supplemental attempt per demand (enforced by
// the caller), and bounds concurrent duplicates across the entire lane set.
// Probes consume a separate time budget; idle paths need no content to stay open.
type Exploration struct {
	active    int
	probes    int
	lastProbe time.Time
}

func (e *Exploration) Acquire(purpose Purpose, now time.Time) bool {
	if !e.Available(purpose, now) {
		return false
	}
	if purpose == Probe || purpose == Sample {
		e.probes++
		e.lastProbe = now
	}
	e.active++
	return true
}

func (e *Exploration) Available(purpose Purpose, now time.Time) bool {
	if e.active >= MaximumSupplements {
		return false
	}
	if purpose == Probe || purpose == Sample {
		if e.probes != 0 || (purpose == Probe && !e.lastProbe.IsZero() && now.Sub(e.lastProbe) < ProbeInterval) {
			return false
		}
	}
	return true
}

func (e *Exploration) NextProbe() time.Time { return e.lastProbe.Add(ProbeInterval) }

func (e *Exploration) Release(purpose Purpose) {
	e.active--
	if purpose == Probe || purpose == Sample {
		e.probes--
	}
}

func ProbeDue(p *Performance, now time.Time) bool {
	return p.PendingBytes == 0 && (p.LastAttempt.IsZero() || now.Sub(p.LastAttempt) >= ProbeInterval)
}

func RescueDue(elapsed, primaryEstimate, alternativeEstimate time.Duration) bool {
	if elapsed < MinimumRescueDelay {
		return false
	}
	remaining := max(0, primaryEstimate-elapsed)
	// Expected queue residence is not a stall. Duplicate only when the other
	// lane can beat the remaining work, or the original estimate is overdue.
	return remaining > HedgeDelayFactor*alternativeEstimate || elapsed >= HedgeDelayFactor*primaryEstimate
}
