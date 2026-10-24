package throughput

import (
	"time"

	"github.com/windshare/windshare/core/transfer/lanescheduling"
)

type trial struct {
	baseline rates
	paths    []PathIdentity
	sample   *sampleTrial
}
type sampleTrial struct {
	path    PathIdentity
	initial lanescheduling.SamplingEvidence
	started time.Time
	window  window
}

func (c *Controller) advanceTrial(now time.Time, o Observation, busy bool) Decision {
	t := c.trial
	if t.sample == nil {
		return c.awaitTrialPath(now, o)
	}
	s := t.sample
	var target *PathObservation
	var others []PathIdentity
	for i := range o.Paths {
		path := &o.Paths[i]
		if path.Identity == s.path {
			target = path
		} else {
			others = append(others, path.Identity)
		}
	}
	d := Decision{Action: Retire, Path: s.path}
	if target == nil {
		d.Action, d.Reason = Abandon, "trial-path-replaced"
		return c.endTrial(now, o, d, ObservationWindow)
	}
	if !samePaths(t.paths, others) {
		d.Reason = "trial-topology-changed"
		return c.endTrial(now, o, d, ObservationWindow)
	}
	work, valid := workSince(target.Work, s.initial)
	d.Work = work
	if !valid || o.UsefulBytes < s.window.useful || o.OutputBytes < s.window.output {
		d.Reason = "trial-observation-reset"
		return c.endTrial(now, o, d, ObservationWindow)
	}
	if !busy {
		d.Reason = "trial-demand-ended"
		return c.endTrial(now, o, d, ObservationWindow)
	}
	if work.OfferedBytes == 0 {
		s.window = windowAt(now, o)
	}
	if now.Sub(s.window.at) >= ObservationWindow {
		if decision := c.evaluateTrial(now, o, work); decision.Action != Hold {
			return decision
		}
	}
	if now.Sub(s.started) >= lanescheduling.SampleLifetime {
		d.Reason = "insufficient-scheduling-evidence"
		return c.endTrial(now, o, d, ObservationWindow)
	}
	return Decision{Action: Hold}
}

func (c *Controller) evaluateTrial(now time.Time, o Observation, work lanescheduling.SamplingEvidence) Decision {
	t := c.trial
	r := ratesAt(now, o, t.sample.window)
	d := r.decision(Retire, "consumer-pressure")
	d.Path, d.Work = t.sample.path, work
	if r.output < r.useful*MinimumOutputRatio && o.CapacityWaitingBytes > 0 {
		return c.endTrial(now, o, d, ObservationWindow)
	}
	if !representative(work) {
		return Decision{Action: Hold}
	}
	if r.useful >= t.baseline.useful*MinimumUsefulGain && r.output >= t.baseline.output*MinimumOutputGain {
		d.Action, d.Reason = Keep, "aggregate-progress-improved"
		return c.endTrial(now, o, d, 0)
	}
	d.Reason = "no-aggregate-gain"
	return c.endTrial(now, o, d, FailureBackoff)
}

func (c *Controller) awaitTrialPath(now time.Time, o Observation) Decision {
	t := c.trial
	for _, path := range o.Paths {
		if !path.Optional {
			continue
		}
		existed := false
		for _, old := range t.paths {
			if old.PathID == path.Identity.PathID {
				existed = true
				break
			}
		}
		if existed {
			continue
		}
		t.sample = &sampleTrial{path: path.Identity, initial: path.Work, started: now, window: windowAt(now, o)}
		return Decision{Action: Sample, Reason: "path-sampling-started", Path: path.Identity, BaselineUsefulRate: t.baseline.useful, BaselineOutputRate: t.baseline.output}
	}
	if o.Pending {
		return Decision{Action: Hold}
	}
	return c.endTrial(now, o, Decision{Action: Abandon, Reason: "connection-not-admitted"}, FailureBackoff)
}

func representative(w lanescheduling.SamplingEvidence) bool {
	// Multiple settlements avoid judging startup from one block; byte coverage
	// also allows fast paths to establish evidence without artificial delay.
	return w.CompletedAttempts+w.FailedAttempts+w.SupersededAttempts >= minimumSettledAttempts &&
		(w.BusyMilliseconds >= minimumBusyMilliseconds || w.CompletedBytes >= lanescheduling.SampleByteBudget)
}

func workSince(w, initial lanescheduling.SamplingEvidence) (lanescheduling.SamplingEvidence, bool) {
	if w.OfferedBytes < initial.OfferedBytes || w.SettledBytes < initial.SettledBytes ||
		w.CompletedBytes < initial.CompletedBytes || w.CompletedAttempts < initial.CompletedAttempts ||
		w.FailedAttempts < initial.FailedAttempts || w.SupersededAttempts < initial.SupersededAttempts ||
		w.CancelledAttempts < initial.CancelledAttempts || w.BusyMilliseconds < initial.BusyMilliseconds {
		return lanescheduling.SamplingEvidence{}, false
	}
	return lanescheduling.SamplingEvidence{
		OfferedBytes: w.OfferedBytes - initial.OfferedBytes, SettledBytes: w.SettledBytes - initial.SettledBytes,
		CompletedBytes: w.CompletedBytes - initial.CompletedBytes, CompletedAttempts: w.CompletedAttempts - initial.CompletedAttempts,
		FailedAttempts: w.FailedAttempts - initial.FailedAttempts, SupersededAttempts: w.SupersededAttempts - initial.SupersededAttempts,
		CancelledAttempts: w.CancelledAttempts - initial.CancelledAttempts, BusyMilliseconds: w.BusyMilliseconds - initial.BusyMilliseconds,
	}, true
}
