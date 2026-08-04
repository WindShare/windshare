package linuxsubreaper

import (
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner"
)

func TestAwaitInitialOutcomeUsesCausalPrecedence(t *testing.T) {
	deadline := time.Unix(1, 0)
	controlErr := errors.New("control failed")
	tests := []struct {
		name            string
		settled         bool
		observationErr  error
		control         *trigger
		wantReason      string
		wantRootSettled bool
		wantError       error
	}{
		{
			name: "kernel exit precedes queued control and deadline", settled: true,
			control:    &trigger{reason: processowner.ReasonStop, err: controlErr},
			wantReason: processowner.ReasonNatural, wantRootSettled: true,
		},
		{
			name:       "queued control precedes deadline",
			control:    &trigger{reason: processowner.ReasonStop, err: controlErr},
			wantReason: processowner.ReasonStop, wantError: controlErr,
		},
		{name: "deadline", wantReason: processowner.ReasonDeadline},
		{
			name:           "kernel observation failure stops an unclassified target",
			observationErr: errors.New("waitid failed"),
			control:        &trigger{reason: processowner.ReasonStop, err: controlErr},
			wantReason:     processowner.ReasonStop,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := awaitInitialOutcome(
				deadline,
				func() (bool, error) { return test.settled, test.observationErr },
				func() (trigger, bool, error) {
					if test.control == nil {
						return trigger{}, false, nil
					}
					return *test.control, true, nil
				},
				func() time.Time { return deadline },
				func(time.Duration) error { return nil },
			)
			if decision.reason != test.wantReason || decision.rootSettled != test.wantRootSettled {
				t.Fatalf("decision = {reason:%q root_settled:%t error:%q}",
					decision.reason, decision.rootSettled, diagnosticText(decision.controlErr))
			}
			if test.observationErr != nil {
				if decision.controlErr == nil || !errors.Is(decision.controlErr, test.observationErr) {
					t.Fatalf("observation decision error = %q", diagnosticText(decision.controlErr))
				}
			} else if !errors.Is(decision.controlErr, test.wantError) {
				t.Fatalf("decision error = %q, want %q",
					diagnosticText(decision.controlErr), diagnosticText(test.wantError))
			}
		})
	}
}

func TestAwaitInitialOutcomeReportsControlObservationFailure(t *testing.T) {
	controlErr := errors.New("poll control failed")
	decision := awaitInitialOutcome(
		time.Now().Add(time.Second),
		func() (bool, error) { return false, nil },
		func() (trigger, bool, error) { return trigger{}, false, controlErr },
		time.Now,
		func(time.Duration) error { return nil },
	)
	if decision.reason != processowner.ReasonStop || decision.rootSettled ||
		!errors.Is(decision.controlErr, controlErr) {
		t.Fatalf("decision = {reason:%q root_settled:%t error:%q}",
			decision.reason, decision.rootSettled, diagnosticText(decision.controlErr))
	}
}

func TestAwaitInitialOutcomeReobservesSourcesAfterWaiting(t *testing.T) {
	deadline := time.Unix(2, 0)
	controlReady := false
	waits := 0
	decision := awaitInitialOutcome(
		deadline,
		func() (bool, error) { return false, nil },
		func() (trigger, bool, error) {
			return trigger{reason: processowner.ReasonInterrupt}, controlReady, nil
		},
		func() time.Time { return deadline.Add(-time.Second) },
		func(maximum time.Duration) error {
			waits++
			if maximum != lifecyclePollInterval {
				t.Fatalf("lifecycle pause = %s", maximum)
			}
			controlReady = true
			return nil
		},
	)
	if waits != 1 || decision.reason != processowner.ReasonInterrupt || decision.rootSettled ||
		decision.controlErr != nil {
		t.Fatalf("decision = {reason:%q root_settled:%t error:%q waits:%d}",
			decision.reason, decision.rootSettled, diagnosticText(decision.controlErr), waits)
	}
}

func TestAwaitInitialOutcomeReportsWaitFailure(t *testing.T) {
	waitErr := errors.New("ppoll failed")
	decision := awaitInitialOutcome(
		time.Now().Add(time.Second),
		func() (bool, error) { return false, nil },
		func() (trigger, bool, error) { return trigger{}, false, nil },
		time.Now,
		func(time.Duration) error { return waitErr },
	)
	if decision.reason != processowner.ReasonStop || !errors.Is(decision.controlErr, waitErr) {
		t.Fatalf("decision = {reason:%q root_settled:%t error:%q}",
			decision.reason, decision.rootSettled, diagnosticText(decision.controlErr))
	}
}

func TestStableEmptyTrackerUsesElapsedEvidenceAndResets(t *testing.T) {
	minimum := 40 * time.Millisecond
	started := time.Unix(1, 0)
	tracker := stableEmptyTracker{}
	if tracker.observe(true, started, minimum) {
		t.Fatal("one empty observation satisfied a nonzero evidence window")
	}
	if !tracker.observe(true, started.Add(250*time.Millisecond), minimum) {
		t.Fatal("scheduler delay did not count toward an uninterrupted empty window")
	}
	if elapsed := tracker.elapsed(started.Add(250 * time.Millisecond)); elapsed != 250*time.Millisecond {
		t.Fatalf("stable empty elapsed = %s", elapsed)
	}
	if tracker.observe(false, started.Add(251*time.Millisecond), minimum) ||
		tracker.elapsed(started.Add(time.Second)) != 0 {
		t.Fatal("a non-empty observation did not reset stable-empty evidence")
	}
	if tracker.observe(true, started.Add(time.Second), minimum) {
		t.Fatal("post-reset empty observation reused stale evidence")
	}
}

func TestStableEmptyTrackerAcceptsAnImmediateEvidencePolicy(t *testing.T) {
	tracker := stableEmptyTracker{}
	started := time.Unix(1, 0)
	if !tracker.observe(true, started, 0) {
		t.Fatal("zero evidence window did not accept an empty observation")
	}
	if elapsed := tracker.elapsed(started.Add(-time.Nanosecond)); elapsed != 0 {
		t.Fatalf("backward clock elapsed = %s", elapsed)
	}
}

func diagnosticText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
