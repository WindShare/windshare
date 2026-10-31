package throughput

import "time"

type inconclusiveRetry struct {
	paths []PathIdentity
	delay time.Duration
}

func (c *Controller) rearmExploration(now time.Time, o Observation) {
	if c.inconclusive == nil {
		return
	}
	idle := !c.idleSince.IsZero() && now.Sub(c.idleSince) >= IdleRetention
	reset := !c.window.at.IsZero() && (o.UsefulBytes < c.window.useful || o.OutputBytes < c.window.output)
	if idle || reset || !samePaths(c.inconclusive.paths, identities(o.Paths)) {
		c.inconclusive = nil
		c.notBefore = now
	}
}

func (c *Controller) endInconclusiveTrial(now time.Time, o Observation, d Decision) Decision {
	delay := FailureBackoff
	if c.inconclusive != nil {
		delay = min(c.inconclusive.delay*2, maximumInconclusiveBackoff)
	}
	// Remember the baseline, not the optional path we are about to remove.
	retry := &inconclusiveRetry{paths: c.trial.paths, delay: delay}
	d = c.endTrial(now, o, d, delay)
	c.inconclusive = retry
	return d
}
