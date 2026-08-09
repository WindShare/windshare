package testscenario

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testrun"
)

func TestTraceCleanupFailuresEnterCleanupAndTerminalVerdicts(t *testing.T) {
	trace, events := newRecordedTrace(t)
	first := errors.New("first close failed")
	second := errors.New("second close failed")
	if err := trace.AddCleanup("first", func(context.Context) error { return first }); err != nil {
		t.Fatal(err)
	}
	if err := trace.AddCleanup("second", func(context.Context) error { return second }); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	err := trace.Finish()
	for _, want := range []error{first, second} {
		if !errors.Is(err, want) {
			t.Fatalf("Finish error %v omitted %v", err, want)
		}
	}
	terminal := assertTerminal(t, *events, testrun.OutcomeFailed, testrun.OutcomeFailed)
	if terminal.FunctionalOutcome != testrun.OutcomeSucceeded ||
		terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded ||
		terminal.LastMilestone != testrun.ScenarioLifecycleMilestone {
		t.Fatalf("cleanup-failure terminal context = %#v", terminal)
	}
	cleanup := findEvent(t, *events, testrun.CleanupMilestone, testrun.OutcomeFailed)
	var payload CleanupContext
	if err := json.Unmarshal(cleanup.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FailureCount != 2 {
		t.Fatalf("cleanup failure count = %d, want 2", payload.FailureCount)
	}
}

func TestCleanupDeadlinePreservesOpenProcessWaitAsLastMilestone(t *testing.T) {
	const processWaitMilestone testrun.Milestone = "process_wait"
	trace, err := newTrace(
		testOperation(t),
		"component",
		discardEventSink{},
		200*time.Millisecond,
		25*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trace.StartPhase(processWaitMilestone, nil); err != nil {
		t.Fatal(err)
	}

	remainingRan := make(chan struct{})
	if err := trace.AddCleanup("remaining owner", func(context.Context) error {
		close(remainingRan)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stalledEntered := make(chan struct{})
	releaseStalled := make(chan struct{})
	defer close(releaseStalled)
	if err := trace.AddCleanup("stalled owner", func(context.Context) error {
		close(stalledEntered)
		<-releaseStalled
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now()
	finishErr := trace.Finish()
	if !errors.Is(finishErr, ErrCleanupTimeout) || !errors.Is(finishErr, ErrPhaseActive) {
		t.Fatalf("Finish error = %v, want cleanup timeout and active phase", finishErr)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("cleanup lease budget delayed terminal transition by %s", elapsed)
	}
	select {
	case <-stalledEntered:
	default:
		t.Fatal("stalled cleanup owner was not invoked")
	}
	select {
	case <-remainingRan:
	default:
		t.Fatal("stalled cleanup owner prevented the remaining owner")
	}

	journal := trace.Events()
	terminal := assertTerminal(t, journal, testrun.OutcomeFailed, testrun.OutcomeFailed)
	if terminal.LastMilestone != processWaitMilestone ||
		terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded {
		t.Fatalf("cleanup-timeout terminal context = %#v", terminal)
	}
	cleanup := findEvent(t, journal, testrun.CleanupMilestone, testrun.OutcomeFailed)
	var cleanupContext CleanupContext
	if err := json.Unmarshal(cleanup.Payload, &cleanupContext); err != nil {
		t.Fatal(err)
	}
	if cleanupContext.FailureCount != 1 {
		t.Fatalf("cleanup failure count = %d, want 1", cleanupContext.FailureCount)
	}
}

func TestCleanupOwnerLeaseAllocationPreservesOwnerCapAndFairShare(t *testing.T) {
	tests := []struct {
		name            string
		ownerTimeout    time.Duration
		scenarioTimeout time.Duration
		ownerCount      int
		want            time.Duration
	}{
		{
			name:            "owner cap",
			ownerTimeout:    15 * time.Second,
			scenarioTimeout: 60 * time.Second,
			ownerCount:      2,
			want:            15 * time.Second,
		},
		{
			name:            "fair share",
			ownerTimeout:    200 * time.Millisecond,
			scenarioTimeout: 25 * time.Millisecond,
			ownerCount:      2,
			want:            25 * time.Millisecond / 2,
		},
		{
			name:            "minimum lease",
			ownerTimeout:    time.Second,
			scenarioTimeout: time.Nanosecond,
			ownerCount:      2,
			want:            time.Nanosecond,
		},
		{
			name:            "no owners",
			ownerTimeout:    15 * time.Second,
			scenarioTimeout: 60 * time.Second,
			ownerCount:      0,
			want:            15 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := allocateCleanupOwnerLease(
				test.ownerTimeout,
				test.scenarioTimeout,
				test.ownerCount,
			); got != test.want {
				t.Fatalf("cleanup owner lease = %s, want %s", got, test.want)
			}
		})
	}
}

func TestCleanupPanicIsolatedIntoTerminalVerdict(t *testing.T) {
	trace, _ := newRecordedTrace(t)
	if err := trace.AddCleanup("panicking owner", func(context.Context) error {
		panic("cleanup fault")
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); !errors.Is(err, ErrCleanupPanic) {
		t.Fatalf("Finish error = %v, want isolated cleanup panic", err)
	}
	assertTerminal(t, trace.Events(), testrun.OutcomeFailed, testrun.OutcomeFailed)
}

func TestCleanupErrorMethodCannotEscapeOwnerLease(t *testing.T) {
	trace := newDiscardTrace(t)
	remainingRan := false
	if err := trace.AddCleanup("remaining owner", func(context.Context) error {
		remainingRan = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	errorMethodEntered := make(chan struct{}, 1)
	releaseErrorMethod := make(chan struct{})
	defer close(releaseErrorMethod)
	want := &blockingCleanupError{entered: errorMethodEntered, release: releaseErrorMethod}
	if err := trace.AddCleanup("hostile error owner", func(context.Context) error {
		return want
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	finishResult := make(chan error, 1)
	go func() { finishResult <- trace.Finish() }()
	select {
	case err := <-finishResult:
		if !errors.Is(err, want) {
			t.Fatalf("Finish lost cleanup error identity: %T", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup Error method escaped the owner lease")
	}
	if !remainingRan {
		t.Fatal("hostile cleanup error prevented the remaining owner")
	}
	select {
	case <-errorMethodEntered:
		t.Fatal("lifecycle evaluated the cleanup Error method")
	default:
	}
	assertTerminal(t, trace.Events(), testrun.OutcomeFailed, testrun.OutcomeFailed)
}

type blockingCleanupError struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (err *blockingCleanupError) Error() string {
	err.entered <- struct{}{}
	<-err.release
	return "hostile cleanup error"
}
