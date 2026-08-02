//go:build windows

package windowsjob

import (
	"errors"
	"strings"
	"testing"
	"time"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

func TestPendingLauncherTransitionPreservesOwnedLifecycleTrigger(t *testing.T) {
	eventResult := launcherEventResult{event: launcherEvent{
		SchemaVersion: launcherEventSchema,
		Type:          launcherEventRootStarted,
		PID:           41,
		ProcessHandle: 73,
	}}
	tests := []struct {
		name     string
		trigger  lifecycleTrigger
		validate func(*testing.T, pendingLauncherTransition)
	}{
		{
			name: "stop",
			trigger: controlLifecycleTrigger(controlResult{
				reason: ownerprotocol.TerminationStop,
			}),
			validate: func(t *testing.T, transition pendingLauncherTransition) {
				t.Helper()
				if transition.trigger.kind != lifecycleTriggerControl ||
					transition.trigger.control.reason != ownerprotocol.TerminationStop {
					t.Fatalf("stop transition = %#v", transition)
				}
			},
		},
		{
			name:    "deadline",
			trigger: deadlineLifecycleTrigger(),
			validate: func(t *testing.T, transition pendingLauncherTransition) {
				t.Helper()
				if transition.trigger.kind != lifecycleTriggerDeadline {
					t.Fatalf("deadline transition = %#v", transition)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expiration := make(chan struct{})
			deadline := controlledLauncherHandoffDeadline(expiration)
			events := make(chan launcherEventResult, 1)
			events <- eventResult
			transition, err := awaitPendingLauncherTransition(events, deadline, test.trigger)
			if err != nil {
				t.Fatal(err)
			}
			if transition.eventResult != eventResult {
				t.Fatalf("launcher event = %#v, want %#v", transition.eventResult, eventResult)
			}
			test.validate(t, transition)
		})
	}
}

func TestLifecycleTriggerRejectsAmbiguousRepresentations(t *testing.T) {
	cause := errors.New("launcher authority failed")
	tests := []lifecycleTrigger{
		{kind: lifecycleTriggerNone, control: controlResult{reason: ownerprotocol.TerminationStop}},
		{kind: lifecycleTriggerControl},
		{kind: lifecycleTriggerControl, control: controlResult{err: cause}},
		{kind: lifecycleTriggerControl, control: controlResult{reason: ownerprotocol.TerminationStop, err: cause}},
		{kind: lifecycleTriggerControl, control: controlResult{reason: "foreign"}},
		{kind: lifecycleTriggerDeadline, control: controlResult{reason: ownerprotocol.TerminationStop}},
		{kind: lifecycleTriggerKind(255)},
	}
	for _, trigger := range tests {
		if err := trigger.validate(); err == nil {
			t.Fatalf("ambiguous lifecycle trigger was accepted: %#v", trigger)
		}
	}
}

func TestLifecycleTriggerAcceptsOnlyCanonicalControlReasons(t *testing.T) {
	for _, reason := range []string{
		ownerprotocol.TerminationStop,
		ownerprotocol.TerminationParentLost,
		ownerprotocol.TerminationDeadline,
	} {
		if err := controlLifecycleTrigger(controlResult{reason: reason}).validate(); err != nil {
			t.Fatalf("canonical control reason %q: %v", reason, err)
		}
	}
}

func TestInitialRootLifecycleTriggerPreservesOuterCausality(t *testing.T) {
	tests := []struct {
		name         string
		initial      func() lifecycleTrigger
		liveControl  controlResult
		wantReason   string
		wantTimedOut bool
		wantExitCode func(terminationExitCodes) uint32
	}{
		{
			name: "stop over simultaneous deadline",
			initial: func() lifecycleTrigger {
				return controlLifecycleTrigger(controlResult{reason: ownerprotocol.TerminationStop})
			},
			liveControl:  controlResult{reason: ownerprotocol.TerminationParentLost},
			wantReason:   ownerprotocol.TerminationStop,
			wantExitCode: func(codes terminationExitCodes) uint32 { return codes.parent },
		},
		{
			name:         "deadline over simultaneous stop",
			initial:      deadlineLifecycleTrigger,
			liveControl:  controlResult{reason: ownerprotocol.TerminationStop},
			wantReason:   ownerprotocol.TerminationDeadline,
			wantTimedOut: true,
			wantExitCode: func(codes terminationExitCodes) uint32 { return codes.deadline },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			codes := mustTerminationExitCodes(t, testIdentity)
			job := newRootExitRaceJob(codes)
			request := windowsIntegrationRequest(t, "echo", 5_000)
			controls := make(chan controlResult, 1)
			controls <- test.liveControl
			requestDeadline := make(chan time.Time, 1)
			requestDeadline <- time.Now()
			monitor := rootTreeMonitor{
				controls:          controls,
				deadline:          requestDeadline,
				terminationReason: ownerprotocol.TerminationNatural,
				initialTrigger:    test.initial(),
			}
			defer monitor.close()

			if err := monitor.awaitEvent(job, fixedRootAuthority(1), request, nil); err != nil {
				t.Fatal(err)
			}
			if len(job.terminationCodes) != 1 || job.terminationCodes[0] != test.wantExitCode(codes) {
				t.Fatalf("termination codes = %#v", job.terminationCodes)
			}
			if monitor.pendingIntervention == nil ||
				monitor.pendingIntervention.reason != test.wantReason ||
				monitor.pendingIntervention.timedOut != test.wantTimedOut {
				t.Fatalf("initial intervention = %#v", monitor.pendingIntervention)
			}
			if monitor.initialTrigger.kind != lifecycleTriggerNone {
				t.Fatalf("initial trigger was not consumed: %#v", monitor.initialTrigger)
			}
		})
	}
}

func TestPendingLauncherEventWinsSimultaneousAbsoluteDeadline(t *testing.T) {
	expiration := make(chan struct{})
	close(expiration)
	deadline := controlledLauncherHandoffDeadline(expiration)
	want := launcherEventResult{event: launcherEvent{Type: launcherEventRootStarted, PID: 17}}
	events := make(chan launcherEventResult, 1)
	events <- want

	got, err := awaitPendingLauncherEvent(events, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("launcher event = %#v, want %#v", got, want)
	}
}

func TestAbsoluteHandoffExpiryRemainsVisibleAfterEventPrecedence(t *testing.T) {
	expiration := make(chan struct{})
	close(expiration)
	deadline := controlledLauncherHandoffDeadline(expiration)
	events := make(chan launcherEventResult, 1)
	events <- launcherEventResult{event: launcherEvent{Type: launcherEventRootStarted, PID: 23}}
	if _, err := awaitPendingLauncherEvent(events, deadline); err != nil {
		t.Fatalf("accept boundary event: %v", err)
	}

	observation := awaitTrustedLauncherExit(make(chan error), deadline)
	if observation.err == nil || !strings.Contains(observation.err.Error(), launcherExitHandoffTimeout) {
		t.Fatalf("post-event launcher exit observation = %#v", observation)
	}
}

func TestAbsoluteHandoffExpiryRemainsVisibleAfterExitPrecedence(t *testing.T) {
	expiration := make(chan struct{})
	close(expiration)
	deadline := controlledLauncherHandoffDeadline(expiration)
	wait := make(chan error, 1)
	wait <- nil
	observation := awaitTrustedLauncherExit(wait, deadline)
	if observation.err != nil || observation.waitErr != nil {
		t.Fatalf("boundary launcher exit observation = %#v", observation)
	}
	membership := &fixedLauncherMembership{processIDs: []uint32{31}}

	err := waitForProcessMembershipRelease(membership, 31, 1, deadline)
	if err == nil || !strings.Contains(err.Error(), "remained in the Job process list") {
		t.Fatalf("post-exit membership fence error = %v", err)
	}
	if membership.queries != 2 {
		t.Fatalf("membership queries = %d, want boundary recheck", membership.queries)
	}
}

func TestPendingLauncherTransitionFailsOnlyAfterAbsoluteDeadline(t *testing.T) {
	expiration := make(chan struct{})
	deadline := controlledLauncherHandoffDeadline(expiration)
	events := make(chan launcherEventResult)
	result := make(chan error, 1)
	go func() {
		_, err := awaitPendingLauncherTransition(
			events,
			deadline,
			controlLifecycleTrigger(controlResult{reason: ownerprotocol.TerminationStop}),
		)
		result <- err
	}()
	want := launcherEventResult{event: launcherEvent{Type: launcherEventRootStarted, PID: 47}}
	select {
	case events <- want:
	case <-time.After(time.Second):
		t.Fatal("pending transition did not retain launcher event authority")
	}
	if err := <-result; err != nil {
		t.Fatalf("event before explicit expiry: %v", err)
	}

	expired := make(chan struct{})
	close(expired)
	_, err := awaitPendingLauncherTransition(
		make(chan launcherEventResult),
		controlledLauncherHandoffDeadline(expired),
		deadlineLifecycleTrigger(),
	)
	if err == nil || !strings.Contains(err.Error(), launcherEventHandoffTimeout) {
		t.Fatalf("expired transition error = %v", err)
	}
}

func controlledLauncherHandoffDeadline(expiration <-chan struct{}) *launcherHandoffDeadline {
	return &launcherHandoffDeadline{
		expiration: expiration,
		stopTimer:  func() bool { return true },
	}
}

type fixedLauncherMembership struct {
	processIDs []uint32
	queries    int
}

func (membership *fixedLauncherMembership) activeProcessIDs(int) ([]uint32, error) {
	membership.queries++
	return membership.processIDs, nil
}
