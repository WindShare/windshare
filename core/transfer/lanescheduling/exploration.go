package lanescheduling

import "time"

const (
	ProbeInterval      = 5 * time.Second
	HedgeCheckInterval = 100 * time.Millisecond
	HedgeDelayFactor   = 2
	MaximumSupplements = 2
)

type Purpose string

const (
	Content Purpose = "content"
	Probe   Purpose = "probe"
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
	if e.active >= MaximumSupplements {
		return false
	}
	if purpose == Probe {
		if e.probes != 0 || (!e.lastProbe.IsZero() && now.Sub(e.lastProbe) < ProbeInterval) {
			return false
		}
		e.probes++
		e.lastProbe = now
	}
	e.active++
	return true
}

func (e *Exploration) Release(purpose Purpose) {
	e.active--
	if purpose == Probe {
		e.probes--
	}
}

func ProbeDue(p *Performance, now time.Time) bool {
	return p.PendingBytes == 0 && (p.LastAttempt.IsZero() || now.Sub(p.LastAttempt) >= ProbeInterval)
}

func RescueDue(elapsed, primaryEstimate, alternativeEstimate time.Duration) bool {
	return elapsed >= max(HedgeCheckInterval, HedgeDelayFactor*min(primaryEstimate, alternativeEstimate))
}
