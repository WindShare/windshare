// Package throughput decides optional growth from aggregate application progress.
// It owns no connection, timer, byte buffer or recovery allowance.
package throughput

import "time"

const (
	SampleInterval     = time.Second
	ObservationWindow  = 3 * time.Second
	SettlingWindow     = time.Second
	Cooldown           = 15 * time.Second
	IdleRetention      = 10 * time.Second
	MinimumUsefulGain  = 1.10
	MinimumOutputGain  = 1.05
	MinimumOutputRatio = 0.65
)

type Observation struct {
	InFlightBytes                                      uint64
	QueuedBytes, UnfinishedBytes, CapacityWaitingBytes uint64
	NetworkWaitingBytes                                uint64
	UsefulBytes, OutputBytes                           uint64
	RemainingBytes                                     uint64
	RemainingKnown                                     bool
	Paths, OptionalPaths                               int
	Pending                                            bool
}

type Action string

const (
	Hold    Action = "hold"
	Explore Action = "explore"
	Retire  Action = "retire"
	Keep    Action = "keep"
)

type Decision struct {
	Action                                 Action
	Reason                                 string
	UsefulRate, OutputRate                 float64
	BaselineUsefulRate, BaselineOutputRate float64
}
type window struct {
	at             time.Time
	useful, output uint64
}
type trial struct {
	paths                  int
	usefulRate, outputRate float64
	admittedAt             time.Time
}

type Controller struct {
	window                              window
	trial                               *trial
	eligibleSince, idleSince, notBefore time.Time
	lastPaths                           int
}

// Step uses the caller's monotonic clock, so a receiver can join its own timer
// lifetime and tests can cover congestion and churn without wall-clock waits.
func (c *Controller) Step(now time.Time, o Observation, maximumPaths int) Decision {
	d := Decision{Action: Hold}
	reset := func() { c.window = window{now, o.UsefulBytes, o.OutputBytes}; c.lastPaths = o.Paths }
	// Balanced authentication/output rates can also mean a full consumer window.
	// Only route-eligible work waiting on the network justifies another path.
	// A bounded producer can fill its entire window without leaving requests in
	// the queue. Discovery exposes future work without allocating larger buffers
	// or creating a private connection backlog.
	windowHasMoreWork := o.QueuedBytes == 0 && o.InFlightBytes > 0 && o.RemainingBytes > o.InFlightBytes
	busy := (o.NetworkWaitingBytes > 0 || windowHasMoreWork) && o.UnfinishedBytes > 0 && o.Paths > 0
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
		if o.OptionalPaths > 0 && !o.Pending && now.Sub(c.idleSince) >= IdleRetention {
			c.trial = nil
			c.notBefore = now.Add(Cooldown)
			c.idleSince = now
			reset()
			return Decision{Action: Retire, Reason: "no-network-demand"}
		}
	}
	if c.window.at.IsZero() || o.UsefulBytes < c.window.useful || o.OutputBytes < c.window.output {
		reset()
		return d
	}
	if c.awaitStablePaths(now, o) {
		return d
	}
	elapsed := now.Sub(c.window.at)
	if elapsed < ObservationWindow {
		return d
	}
	d.UsefulRate = float64(o.UsefulBytes-c.window.useful) / elapsed.Seconds()
	d.OutputRate = float64(o.OutputBytes-c.window.output) / elapsed.Seconds()
	reset()
	if c.trial != nil {
		t := c.trial
		d.BaselineUsefulRate, d.BaselineOutputRate = t.usefulRate, t.outputRate
		c.trial = nil
		c.notBefore = now.Add(Cooldown)
		if d.UsefulRate >= t.usefulRate*MinimumUsefulGain && d.OutputRate >= t.outputRate*MinimumOutputGain {
			d.Action, d.Reason = Keep, "aggregate-progress-improved"
		} else {
			d.Action, d.Reason = Retire, "no-aggregate-gain"
		}
		return d
	}
	outputLimited := d.OutputRate < d.UsefulRate*MinimumOutputRatio
	if o.OptionalPaths > 0 && outputLimited && o.CapacityWaitingBytes > 0 && !o.Pending {
		c.notBefore = now.Add(Cooldown)
		d.Action, d.Reason = Retire, "consumer-pressure"
		return d
	}
	if !busy || c.eligibleSince.IsZero() || now.Sub(c.eligibleSince) < ObservationWindow ||
		now.Before(c.notBefore) || o.Pending || o.Paths >= maximumPaths || d.UsefulRate <= 0 || d.OutputRate <= 0 || outputLimited {
		return d
	}
	// A known tail must have enough work to observe an added connection. Unknown
	// discovery is eligible; it never waits for a complete directory scan.
	if o.RemainingKnown && float64(o.RemainingBytes) < d.UsefulRate*(ObservationWindow+SettlingWindow).Seconds() {
		return d
	}
	c.trial = &trial{paths: o.Paths, usefulRate: d.UsefulRate, outputRate: d.OutputRate}
	d.Action, d.Reason = Explore, "sustained-network-demand"
	return d
}

func (c *Controller) resetWindow(now time.Time, o Observation) {
	c.window = window{now, o.UsefulBytes, o.OutputBytes}
	c.lastPaths = o.Paths
}

func (c *Controller) awaitStablePaths(now time.Time, o Observation) bool {
	if c.trial == nil {
		if o.Paths != c.lastPaths {
			c.resetWindow(now, o)
			return true
		}
		return false
	}
	t := c.trial
	if o.Paths <= t.paths {
		if !o.Pending {
			c.trial = nil
			c.notBefore = now.Add(Cooldown)
			c.resetWindow(now, o)
		}
		return true
	}
	if t.admittedAt.IsZero() {
		t.admittedAt = now
		c.resetWindow(now, o)
		return true
	}
	if now.Sub(t.admittedAt) < SettlingWindow {
		c.resetWindow(now, o)
		return true
	}
	return false
}
