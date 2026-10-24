// Package throughput decides optional growth from aggregate application progress.
// It owns no connection, timer, byte buffer or recovery allowance.
package throughput

import "time"

type Controller struct {
	window                              window
	trial                               *trial
	eligibleSince, idleSince, notBefore time.Time
	paths                               []PathIdentity
}

// Step consumes scheduling facts for exact path generations. The caller owns
// connection creation and sampling grants, keeping this policy deterministic.
func (c *Controller) Step(now time.Time, o Observation, maximumPaths int) Decision {
	windowHasMoreWork := o.QueuedBytes == 0 && o.InFlightBytes > 0 && o.RemainingBytes > o.InFlightBytes
	busy := (o.NetworkWaitingBytes > 0 || windowHasMoreWork) && o.UnfinishedBytes > 0 && len(o.Paths) > 0
	if busy {
		c.idleSince = time.Time{}
		if c.eligibleSince.IsZero() {
			c.eligibleSince = now
		}
	} else {
		c.eligibleSince = time.Time{}
		if c.idleSince.IsZero() {
			c.idleSince = now
		}
	}
	if c.trial != nil {
		return c.advanceTrial(now, o, busy)
	}
	optional := newestOptional(o.Paths)
	if !busy && optional != nil && !o.Pending && now.Sub(c.idleSince) >= IdleRetention {
		c.notBefore = now.Add(ObservationWindow)
		c.reset(now, o)
		return Decision{Action: Retire, Reason: "no-network-demand", Path: optional.Identity}
	}
	if c.window.at.IsZero() || o.UsefulBytes < c.window.useful || o.OutputBytes < c.window.output ||
		!samePaths(c.paths, identities(o.Paths)) {
		c.reset(now, o)
		return Decision{Action: Hold}
	}
	if now.Sub(c.window.at) < ObservationWindow {
		return Decision{Action: Hold}
	}
	r := ratesAt(now, o, c.window)
	c.reset(now, o)
	outputLimited := r.output < r.useful*MinimumOutputRatio
	if optional != nil && outputLimited && o.CapacityWaitingBytes > 0 && !o.Pending {
		c.notBefore = now.Add(ObservationWindow)
		d := r.decision(Retire, "consumer-pressure")
		d.Path = optional.Identity
		return d
	}
	if !busy || c.eligibleSince.IsZero() || now.Sub(c.eligibleSince) < ObservationWindow ||
		now.Before(c.notBefore) || o.Pending || len(o.Paths) >= maximumPaths ||
		r.useful <= 0 || r.output <= 0 || outputLimited {
		return Decision{Action: Hold}
	}
	// Unknown progressive discovery never requires a complete directory scan.
	if o.RemainingKnown && float64(o.RemainingBytes) < r.useful*(ObservationWindow+SampleInterval).Seconds() {
		return Decision{Action: Hold}
	}
	c.trial = &trial{baseline: r, paths: c.paths}
	return r.decision(Explore, "sustained-network-demand")
}

func (c *Controller) reset(now time.Time, o Observation) {
	c.window = windowAt(now, o)
	c.paths = identities(o.Paths)
}

func (c *Controller) endTrial(now time.Time, o Observation, d Decision, delay time.Duration) Decision {
	d.BaselineUsefulRate, d.BaselineOutputRate = c.trial.baseline.useful, c.trial.baseline.output
	c.trial = nil
	c.notBefore = now.Add(delay)
	c.reset(now, o)
	return d
}
