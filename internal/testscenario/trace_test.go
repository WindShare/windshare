package testscenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testrun"
)

const testPhaseMilestone testrun.Milestone = "frame_exchange"

type fatalCapture struct {
	message string
}

func (*fatalCapture) Helper() {}

func (capture *fatalCapture) Fatalf(format string, arguments ...any) {
	capture.message = fmt.Sprintf(format, arguments...)
}

type lifecycleCapture struct {
	fatalMessage string
	errors       []string
	cleanups     []func()
}

func (*lifecycleCapture) Helper() {}

func (capture *lifecycleCapture) Fatalf(format string, arguments ...any) {
	capture.fatalMessage = fmt.Sprintf(format, arguments...)
}

func (capture *lifecycleCapture) Errorf(format string, arguments ...any) {
	capture.errors = append(capture.errors, fmt.Sprintf(format, arguments...))
}

func (capture *lifecycleCapture) Cleanup(cleanup func()) {
	capture.cleanups = append(capture.cleanups, cleanup)
}

func (capture *lifecycleCapture) runCleanups() {
	for index := range slices.Backward(capture.cleanups) {
		capture.cleanups[index]()
	}
}

func TestTraceFinishIsIdempotentAndFreezesState(t *testing.T) {
	trace, events := newRecordedTrace(t)
	phase, err := trace.StartPhase(testPhaseMilestone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := phase.Succeed(nil); err != nil {
		t.Fatal(err)
	}
	var cleanupOrder []string
	if err := trace.AddCleanup("first", func(context.Context) error {
		cleanupOrder = append(cleanupOrder, "first")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.AddCleanup("second", func(context.Context) error {
		cleanupOrder = append(cleanupOrder, "second")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
	eventCount := len(*events)
	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(*events) != eventCount {
		t.Fatalf("idempotent Finish appended events: before=%d after=%d", eventCount, len(*events))
	}
	if len(cleanupOrder) != 2 || cleanupOrder[0] != "second" || cleanupOrder[1] != "first" {
		t.Fatalf("cleanup order = %v, want [second first]", cleanupOrder)
	}
	if !trace.Finished() {
		t.Fatal("trace did not publish its finished boundary")
	}
	terminal := assertTerminal(t, *events, testrun.OutcomeSucceeded, testrun.OutcomeSucceeded)
	if terminal.FunctionalOutcome != testrun.OutcomeSucceeded ||
		terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded || terminal.LastMilestone != testPhaseMilestone {
		t.Fatalf("successful terminal context = %#v", terminal)
	}

	assertRetired := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrRetired) {
			t.Fatalf("%s after Finish = %v, want ErrRetired", name, err)
		}
	}
	assertRetired("cleanup registration", trace.AddCleanup("late", func(context.Context) error { return nil }))
	capture := &fatalCapture{}
	trace.RequireCleanup(capture, "late-required", func(context.Context) error { return nil })
	if !strings.Contains(capture.message, ErrRetired.Error()) {
		t.Fatalf("RequireCleanup after Finish did not reject retirement: %q", capture.message)
	}
	assertRetired("record", trace.Record(testPhaseMilestone, testrun.OutcomeSucceeded, nil))
	_, err = trace.StartPhase(testPhaseMilestone, nil)
	assertRetired("phase start", err)
	assertRetired("phase settlement", phase.Fail("late_failure"))
	assertRetired("functional success", trace.MarkFunctionalSuccess())
}

func TestMarkFunctionalSuccessSealsEveryMutationBeforeFinish(t *testing.T) {
	trace, _ := newRecordedTrace(t)
	phase, err := trace.StartPhase(testPhaseMilestone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := phase.Succeed(nil); err != nil {
		t.Fatal(err)
	}
	cleanupCalled := false
	if err := trace.AddCleanup("owned", func(context.Context) error {
		cleanupCalled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if trace.Finished() {
		t.Fatal("functional seal incorrectly reported the cleanup boundary as finished")
	}

	assertRetired := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrRetired) {
			t.Fatalf("%s after functional seal = %v, want ErrRetired", name, err)
		}
	}
	assertRetired("cleanup registration", trace.AddCleanup("late", func(context.Context) error { return nil }))
	capture := &fatalCapture{}
	trace.RequireCleanup(capture, "late-required", func(context.Context) error { return nil })
	if !strings.Contains(capture.message, ErrRetired.Error()) {
		t.Fatalf("RequireCleanup after functional seal did not reject retirement: %q", capture.message)
	}
	assertRetired("point record", trace.Record(testPhaseMilestone, testrun.OutcomeSucceeded, nil))
	_, err = trace.StartPhase("late_phase", nil)
	assertRetired("phase start", err)
	assertRetired("phase settlement", phase.Fail("late_failure"))
	assertRetired("second functional seal", trace.MarkFunctionalSuccess())

	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
	if !cleanupCalled {
		t.Fatal("functional seal discarded an already-owned cleanup")
	}
}

func TestMarkFunctionalSuccessLinearizesAgainstConcurrentPhaseStart(t *testing.T) {
	const attempts = 256
	type startResult struct {
		phase *Phase
		err   error
	}
	for attempt := range attempts {
		trace, events := newRecordedTrace(t)
		start := make(chan struct{})
		started := make(chan startResult, 1)
		marked := make(chan error, 1)
		go func() {
			<-start
			phase, err := trace.StartPhase(testPhaseMilestone, nil)
			started <- startResult{phase: phase, err: err}
		}()
		go func() {
			<-start
			marked <- trace.MarkFunctionalSuccess()
		}()
		close(start)
		startResult := <-started
		markErr := <-marked

		switch {
		case markErr == nil:
			if startResult.phase != nil || !errors.Is(startResult.err, ErrRetired) {
				t.Fatalf(
					"attempt %d accepted phase after seal: phase=%p err=%v",
					attempt,
					startResult.phase,
					startResult.err,
				)
			}
			if err := trace.Finish(); err != nil {
				t.Fatalf("attempt %d successful seal Finish: %v", attempt, err)
			}
			assertTerminal(t, *events, testrun.OutcomeSucceeded, testrun.OutcomeSucceeded)
		case startResult.err == nil:
			if startResult.phase == nil || !errors.Is(markErr, ErrPhaseActive) {
				t.Fatalf(
					"attempt %d phase-first result: phase=%p startErr=%v markErr=%v",
					attempt,
					startResult.phase,
					startResult.err,
					markErr,
				)
			}
			finishErr := trace.Finish()
			if !errors.Is(finishErr, ErrPhaseActive) || !errors.Is(finishErr, ErrIncomplete) {
				t.Fatalf("attempt %d phase-first Finish = %v", attempt, finishErr)
			}
			assertTerminal(t, *events, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
		default:
			t.Fatalf(
				"attempt %d produced non-linearizable results: startErr=%v markErr=%v",
				attempt,
				startResult.err,
				markErr,
			)
		}
	}
}

func TestMarkFunctionalSuccessLinearizesAgainstConcurrentPointMutations(t *testing.T) {
	const attempts = 128
	mutations := []struct {
		name        string
		ownsCleanup bool
		build       func(*bool) func(*Trace) error
	}{
		{
			name:        "cleanup registration",
			ownsCleanup: true,
			build: func(cleanupCalled *bool) func(*Trace) error {
				return func(trace *Trace) error {
					return trace.AddCleanup("concurrent", func(context.Context) error {
						*cleanupCalled = true
						return nil
					})
				}
			},
		},
		{
			name: "point record",
			build: func(*bool) func(*Trace) error {
				return func(trace *Trace) error {
					return trace.Record("concurrent_point", testrun.OutcomeSucceeded, nil)
				}
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			for attempt := range attempts {
				trace, _ := newRecordedTrace(t)
				cleanupCalled := false
				mutate := mutation.build(&cleanupCalled)
				start := make(chan struct{})
				mutationResult := make(chan error, 1)
				markResult := make(chan error, 1)
				go func() {
					<-start
					mutationResult <- mutate(trace)
				}()
				go func() {
					<-start
					markResult <- trace.MarkFunctionalSuccess()
				}()
				close(start)
				mutationErr := <-mutationResult
				if markErr := <-markResult; markErr != nil {
					t.Fatalf("attempt %d functional seal: %v", attempt, markErr)
				}
				if mutationErr != nil && !errors.Is(mutationErr, ErrRetired) {
					t.Fatalf("attempt %d mutation error = %v", attempt, mutationErr)
				}
				if err := trace.Finish(); err != nil {
					t.Fatalf("attempt %d Finish: %v", attempt, err)
				}
				if cleanupCalled != (mutationErr == nil && mutation.ownsCleanup) {
					t.Fatalf(
						"attempt %d cleanupCalled=%v mutationErr=%v",
						attempt,
						cleanupCalled,
						mutationErr,
					)
				}
			}
		})
	}
}

func TestMarkFunctionalSuccessLinearizesAgainstConcurrentPhaseSettlement(t *testing.T) {
	const attempts = 256
	for attempt := range attempts {
		trace, events := newRecordedTrace(t)
		phase, err := trace.StartPhase(testPhaseMilestone, nil)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		settled := make(chan error, 1)
		marked := make(chan error, 1)
		go func() {
			<-start
			settled <- phase.Succeed(nil)
		}()
		go func() {
			<-start
			marked <- trace.MarkFunctionalSuccess()
		}()
		close(start)
		settleErr := <-settled
		markErr := <-marked
		if settleErr != nil {
			t.Fatalf("attempt %d phase settlement: %v", attempt, settleErr)
		}

		if markErr == nil {
			if err := trace.Finish(); err != nil {
				t.Fatalf("attempt %d sealed Finish: %v", attempt, err)
			}
			assertTerminal(t, *events, testrun.OutcomeSucceeded, testrun.OutcomeSucceeded)
			continue
		}
		if !errors.Is(markErr, ErrPhaseActive) {
			t.Fatalf("attempt %d functional seal error = %v", attempt, markErr)
		}
		if err := trace.Finish(); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("attempt %d unsealed Finish = %v, want ErrIncomplete", attempt, err)
		}
		assertTerminal(t, *events, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	}
}

func TestFinishRevalidatesEverySealedPhase(t *testing.T) {
	trace, events := newRecordedTrace(t)
	phase, err := trace.StartPhase(testPhaseMilestone, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := phase.Succeed(nil); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}

	// This white-box mutation models an internal regression after sealing. Finish
	// must derive its verdict from phase state rather than trust the cached seal.
	trace.mu.Lock()
	phase.succeeded = false
	trace.mu.Unlock()
	if err := trace.Finish(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Finish accepted an inconsistent sealed phase: %v", err)
	}
	assertTerminal(t, *events, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
}

func TestTraceRetainsSinkFailureAndFinishesOnce(t *testing.T) {
	operation := testOperation(t)
	want := errors.New("event sink failed")
	var mu sync.Mutex
	var events []testrun.Event
	writes := 0
	sink := testrun.EventSinkFunc(func(event testrun.Event) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
		writes++
		if writes == 1 {
			return nil
		}
		return want
	})
	trace, err := New(operation, "component", sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); !errors.Is(err, want) {
		t.Fatalf("Finish error = %v, want sink failure", err)
	}
	mu.Lock()
	firstWrites := writes
	mu.Unlock()
	if err := trace.Finish(); !errors.Is(err, want) {
		t.Fatalf("second Finish error = %v, want sink failure", err)
	}
	mu.Lock()
	secondWrites := writes
	mu.Unlock()
	if secondWrites != firstWrites {
		t.Fatalf("second Finish retried sink writes: before=%d after=%d", firstWrites, secondWrites)
	}
	terminal := assertTerminal(t, trace.Events(), testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	if terminal.FunctionalOutcome != testrun.OutcomeSucceeded ||
		terminal.PriorDeliveryOutcome != testrun.OutcomeFailed ||
		terminal.LastMilestone != testrun.ScenarioLifecycleMilestone {
		t.Fatalf("sink-failure terminal context = %#v", terminal)
	}
}

func TestNewReturnsLifecycleAuthorityWhenStartedSinkFails(t *testing.T) {
	want := errors.New("scenario started sink failed")
	var events []testrun.Event
	trace, err := New(
		testOperation(t),
		"component",
		testrun.EventSinkFunc(func(event testrun.Event) error {
			events = append(events, event)
			if event.Milestone == string(testrun.ScenarioLifecycleMilestone) &&
				event.Outcome == string(testrun.OutcomeStarted) {
				return want
			}
			return nil
		}),
	)
	if trace == nil {
		t.Fatal("New discarded lifecycle authority after a sink error")
	}
	if !errors.Is(err, want) {
		t.Fatalf("New error = %v, want sink failure", err)
	}
	finishErr := trace.Finish()
	if !errors.Is(finishErr, want) || !errors.Is(finishErr, ErrIncomplete) {
		t.Fatalf("Finish error = %v, want sink failure and ErrIncomplete", finishErr)
	}
	terminal := assertTerminal(t, trace.Events(), testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	if terminal.PriorDeliveryOutcome != testrun.OutcomeFailed ||
		terminal.LastMilestone != testrun.ScenarioLifecycleMilestone {
		t.Fatalf("started-sink-failure terminal context = %#v", terminal)
	}
}

func TestStartOwnsTerminalBeforeSurfacingStartedSinkFailure(t *testing.T) {
	want := errors.New("started event was persisted but not acknowledged")
	var events []testrun.Event
	capture := &lifecycleCapture{}
	trace := Start(
		capture,
		testOperation(t),
		"component",
		testrun.EventSinkFunc(func(event testrun.Event) error {
			events = append(events, event)
			if event.Milestone == string(testrun.ScenarioLifecycleMilestone) &&
				event.Outcome == string(testrun.OutcomeStarted) {
				return want
			}
			return nil
		}),
	)
	if trace == nil || len(capture.cleanups) != 1 ||
		!strings.Contains(capture.fatalMessage, "test event delivery failed") {
		t.Fatalf(
			"Start result: trace=%p cleanups=%d fatal=%q",
			trace,
			len(capture.cleanups),
			capture.fatalMessage,
		)
	}
	capture.runCleanups()
	if len(capture.errors) != 1 ||
		!strings.Contains(capture.errors[0], "test event delivery failed") {
		t.Fatalf("cleanup errors = %v, want retained sink failure", capture.errors)
	}
	terminal := assertTerminal(t, trace.Events(), testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	if terminal.PriorDeliveryOutcome != testrun.OutcomeFailed {
		t.Fatalf("terminal prior delivery outcome = %q", terminal.PriorDeliveryOutcome)
	}
}

func TestStartPhaseSinkErrorRetainsAttemptForMatchedFailure(t *testing.T) {
	operation := testOperation(t)
	want := errors.New("phase started sink failed")
	var events []testrun.Event
	sink := testrun.EventSinkFunc(func(event testrun.Event) error {
		// A sink can persist an event before reporting a delivery error. Retaining the
		// phase is therefore necessary to prevent an externally visible orphan start.
		events = append(events, event)
		if event.Milestone == string(testPhaseMilestone) &&
			event.Outcome == string(testrun.OutcomeStarted) {
			return want
		}
		return nil
	})
	trace, err := New(operation, "component", sink)
	if err != nil {
		t.Fatal(err)
	}
	phase, err := trace.StartPhase(testPhaseMilestone, nil)
	if phase == nil {
		t.Fatal("StartPhase discarded settlement authority after a sink error")
	}
	if !errors.Is(err, want) {
		t.Fatalf("StartPhase error = %v, want sink failure", err)
	}
	finishErr := trace.Finish()
	for _, expected := range []error{want, ErrPhaseActive, ErrIncomplete} {
		if !errors.Is(finishErr, expected) {
			t.Fatalf("Finish error %v omitted %v", finishErr, expected)
		}
	}

	journal := trace.Events()
	phaseEvents := eventsForMilestone(journal, testPhaseMilestone)
	if len(phaseEvents) != 2 || phaseEvents[0].Outcome != string(testrun.OutcomeStarted) ||
		phaseEvents[1].Outcome != string(testrun.OutcomeFailed) {
		t.Fatalf("phase events = %#v, want started then failed", phaseEvents)
	}
	var failure FailureContext
	if err := json.Unmarshal(phaseEvents[1].Payload, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Reason != InterruptedFailureReason {
		t.Fatalf("interrupted reason = %q", failure.Reason)
	}
	if err := phase.Succeed(nil); !errors.Is(err, ErrRetired) {
		t.Fatalf("phase settlement after Finish = %v, want ErrRetired", err)
	}
	terminal := assertTerminal(t, journal, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	if terminal.PriorDeliveryOutcome != testrun.OutcomeFailed || terminal.LastMilestone != testPhaseMilestone {
		t.Fatalf("phase-start sink failure terminal context = %#v", terminal)
	}
}

func TestRecordRejectsScenarioOwnedMilestones(t *testing.T) {
	trace, events := newRecordedTrace(t)
	before := len(*events)
	for _, milestone := range []testrun.Milestone{
		testrun.ScenarioLifecycleMilestone,
		testrun.CleanupMilestone,
	} {
		if err := trace.Record(milestone, testrun.OutcomeSucceeded, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Record(%q) error = %v, want ErrInvalid", milestone, err)
		}
	}
	if len(*events) != before {
		t.Fatalf("reserved Record calls appended events: before=%d after=%d", before, len(*events))
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRejectsStartedOutcomeWithoutPublishing(t *testing.T) {
	trace, events := newRecordedTrace(t)
	before := len(*events)
	for _, outcome := range []testrun.Outcome{testrun.OutcomeStarted, "unknown"} {
		if err := trace.Record(testPhaseMilestone, outcome, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("point record outcome %q error = %v, want ErrInvalid", outcome, err)
		}
	}
	if len(*events) != before {
		t.Fatalf("undecided point records appended events: before=%d after=%d", before, len(*events))
	}
	if err := trace.Record("point_observation", testrun.OutcomeSucceeded, nil); err != nil {
		t.Fatalf("decided point record: %v", err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestPhasePairsStartedWithExactlyOneBoundedFailure(t *testing.T) {
	trace, events := newRecordedTrace(t)
	phase, err := trace.StartPhase(testPhaseMilestone, map[string]int{"bytes": 7})
	if err != nil {
		t.Fatal(err)
	}
	if err := phase.Fail("receiver_send_failed"); err != nil {
		t.Fatal(err)
	}
	if err := phase.Succeed(nil); !errors.Is(err, ErrPhaseSettled) {
		t.Fatalf("second phase settlement = %v, want ErrPhaseSettled", err)
	}
	if err := trace.MarkFunctionalSuccess(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("failed phase allowed functional success: %v", err)
	}
	if err := trace.Finish(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Finish error = %v, want ErrIncomplete", err)
	}

	phaseEvents := eventsForMilestone(*events, testPhaseMilestone)
	if len(phaseEvents) != 2 || phaseEvents[0].Outcome != string(testrun.OutcomeStarted) ||
		phaseEvents[1].Outcome != string(testrun.OutcomeFailed) {
		t.Fatalf("phase events = %#v, want started then failed", phaseEvents)
	}
	var failure FailureContext
	if err := json.Unmarshal(phaseEvents[1].Payload, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Reason != "receiver_send_failed" {
		t.Fatalf("phase failure reason = %q", failure.Reason)
	}
}

func TestTerminalReportsOpenTimeoutPhaseAsLastMilestone(t *testing.T) {
	trace, events := newRecordedTrace(t)
	const timeoutMilestone testrun.Milestone = "receiver_wait_timeout"
	if _, err := trace.StartPhase(timeoutMilestone, nil); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); !errors.Is(err, ErrPhaseActive) || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("Finish error = %v, want active phase and incomplete verdict", err)
	}
	phaseEvents := eventsForMilestone(*events, timeoutMilestone)
	if len(phaseEvents) != 2 || phaseEvents[1].Outcome != string(testrun.OutcomeFailed) {
		t.Fatalf("interrupted phase events = %#v", phaseEvents)
	}
	var failure FailureContext
	if err := json.Unmarshal(phaseEvents[1].Payload, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Reason != InterruptedFailureReason {
		t.Fatalf("interrupted reason = %q", failure.Reason)
	}
	terminal := assertTerminal(t, *events, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
	if terminal.LastMilestone != timeoutMilestone || terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded {
		t.Fatalf("open-timeout terminal context = %#v", terminal)
	}
}

func TestPhaseRejectsUnboundedOrFreeFormFailureReasons(t *testing.T) {
	for _, reason := range []string{"", "contains spaces", strings.Repeat("a", MaximumFailureReasonBytes+1)} {
		trace, _ := newRecordedTrace(t)
		phase, err := trace.StartPhase(testPhaseMilestone, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := phase.Fail(reason); !errors.Is(err, ErrFailureReason) {
			t.Fatalf("reason %q error = %v, want ErrFailureReason", reason, err)
		}
		if err := trace.Finish(); !errors.Is(err, ErrPhaseActive) {
			t.Fatalf("invalid reason settled phase: %v", err)
		}
	}
}

func TestStalledSinkCannotPreventCleanupOrAuthoritativeTerminal(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	var enteredOnce sync.Once
	startedAt := time.Now()
	trace, err := New(
		testOperation(t),
		"component",
		testrun.EventSinkFunc(func(testrun.Event) error {
			enteredOnce.Do(func() { close(entered) })
			<-release
			return nil
		}),
	)
	if trace == nil || !errors.Is(err, testrun.ErrEventDeliveryTimeout) {
		t.Fatalf("New result: trace=%p error=%v, want bounded delivery timeout", trace, err)
	}
	if elapsed := time.Since(startedAt); elapsed > testrun.EventSinkCallTimeout+time.Second {
		t.Fatalf("stalled started-event delivery took %s", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("stalled sink was not invoked")
	}

	cleanupRan := false
	if err := trace.AddCleanup("remaining owner", func(context.Context) error {
		cleanupRan = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	finishStartedAt := time.Now()
	finishErr := trace.Finish()
	if !errors.Is(finishErr, testrun.ErrEventDeliveryTimeout) {
		t.Fatalf("Finish error = %v, want retained delivery timeout", finishErr)
	}
	if elapsed := time.Since(finishStartedAt); elapsed > time.Second {
		t.Fatalf("latched stalled sink delayed terminal transition by %s", elapsed)
	}
	if !cleanupRan {
		t.Fatal("stalled sink prevented cleanup")
	}
	terminal := assertTerminal(
		t,
		trace.Events(),
		testrun.OutcomeFailed,
		testrun.OutcomeSucceeded,
	)
	if terminal.FunctionalOutcome != testrun.OutcomeSucceeded ||
		terminal.PriorEvidenceOutcome != testrun.OutcomeSucceeded ||
		terminal.PriorDeliveryOutcome != testrun.OutcomeFailed {
		t.Fatalf("stalled-sink terminal context = %#v", terminal)
	}
}

func TestPayloadPreparationFailureDoesNotCommitContradictoryPhaseState(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		trace := newDiscardTrace(t)
		const milestone testrun.Milestone = "payload_start"
		phase, err := trace.StartPhase(milestone, panickingTracePayload{})
		if phase != nil || !errors.Is(err, testrun.ErrEventPayloadPanic) {
			t.Fatalf("StartPhase result: phase=%p error=%v", phase, err)
		}
		if err := trace.MarkFunctionalSuccess(); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("payload preparation failure allowed success: %v", err)
		}
		if err := trace.Finish(); !errors.Is(err, testrun.ErrEventPayloadPanic) {
			t.Fatalf("Finish omitted payload preparation failure: %v", err)
		}
		journal := trace.Events()
		if events := eventsForMilestone(journal, milestone); len(events) != 0 {
			t.Fatalf("failed phase preparation reached journal: %#v", events)
		}
		terminal := assertTerminal(t, journal, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
		if terminal.LastMilestone != testrun.ScenarioLifecycleMilestone ||
			terminal.PriorEvidenceOutcome != testrun.OutcomeFailed ||
			terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded {
			t.Fatalf("phase-preparation terminal = %#v", terminal)
		}
	})

	t.Run("settlement", func(t *testing.T) {
		trace := newDiscardTrace(t)
		const milestone testrun.Milestone = "payload_settlement"
		phase, err := trace.StartPhase(milestone, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := phase.Succeed(panickingTracePayload{}); !errors.Is(err, testrun.ErrEventPayloadPanic) {
			t.Fatalf("settlement payload error = %v", err)
		}
		if err := trace.MarkFunctionalSuccess(); !errors.Is(err, ErrIncomplete) {
			t.Fatalf("missing settlement allowed success: %v", err)
		}
		if err := trace.Finish(); !errors.Is(err, ErrPhaseActive) {
			t.Fatalf("Finish error = %v, want active phase", err)
		}
		journal := trace.Events()
		phaseEvents := eventsForMilestone(journal, milestone)
		if len(phaseEvents) != 2 || phaseEvents[0].Outcome != string(testrun.OutcomeStarted) ||
			phaseEvents[1].Outcome != string(testrun.OutcomeFailed) {
			t.Fatalf("settlement preparation journal = %#v", phaseEvents)
		}
		terminal := assertTerminal(t, journal, testrun.OutcomeFailed, testrun.OutcomeSucceeded)
		if terminal.LastMilestone != milestone ||
			terminal.PriorEvidenceOutcome != testrun.OutcomeFailed ||
			terminal.PriorDeliveryOutcome != testrun.OutcomeSucceeded {
			t.Fatalf("settlement-preparation terminal = %#v", terminal)
		}
	})
}

func TestTraceBoundsCleanupPhaseAndEvidenceGrowth(t *testing.T) {
	t.Run("cleanup owners", func(t *testing.T) {
		trace := newDiscardTrace(t)
		if err := trace.AddCleanup(
			strings.Repeat("x", MaximumCleanupOwnerNameBytes+1),
			func(context.Context) error { return nil },
		); !errors.Is(err, ErrInvalid) {
			t.Fatalf("oversized cleanup name error = %v, want ErrInvalid", err)
		}
		for range MaximumCleanupOwners {
			if err := trace.AddCleanup("bounded owner", func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
		if err := trace.AddCleanup("overflow owner", func(context.Context) error { return nil }); !errors.Is(err, ErrCapacity) {
			t.Fatalf("overflow cleanup error = %v, want ErrCapacity", err)
		}
		if err := trace.MarkFunctionalSuccess(); err != nil {
			t.Fatal(err)
		}
		if err := trace.Finish(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("phases", func(t *testing.T) {
		trace := newDiscardTrace(t)
		for range MaximumPhasesPerScenario {
			phase, err := trace.StartPhase(testPhaseMilestone, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := phase.Succeed(nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := trace.StartPhase(testPhaseMilestone, nil); !errors.Is(err, ErrCapacity) {
			t.Fatalf("overflow phase error = %v, want ErrCapacity", err)
		}
		if err := trace.MarkFunctionalSuccess(); err != nil {
			t.Fatal(err)
		}
		if err := trace.Finish(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("evidence events", func(t *testing.T) {
		trace := newDiscardTrace(t)
		for range MaximumScenarioEvidenceEvents {
			if err := trace.Record("point_observation", testrun.OutcomeSucceeded, nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := trace.Record("point_observation", testrun.OutcomeSucceeded, nil); !errors.Is(err, ErrCapacity) {
			t.Fatalf("overflow evidence error = %v, want ErrCapacity", err)
		}
		if err := trace.MarkFunctionalSuccess(); err != nil {
			t.Fatal(err)
		}
		if err := trace.Finish(); err != nil {
			t.Fatal(err)
		}
		wantEvents := 1 + MaximumScenarioEvidenceEvents + terminalEventReserve
		if events := trace.Events(); len(events) != wantEvents {
			t.Fatalf("authoritative events = %d, want %d", len(events), wantEvents)
		}
	})
}

type discardEventSink struct{}

func (discardEventSink) WriteEvent(testrun.Event) error { return nil }

type panickingTracePayload struct{}

func (panickingTracePayload) MarshalJSON() ([]byte, error) { panic("payload fault") }

func newDiscardTrace(t *testing.T) *Trace {
	t.Helper()
	trace, err := New(testOperation(t), "component", discardEventSink{})
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func newRecordedTrace(t *testing.T) (*Trace, *[]testrun.Event) {
	t.Helper()
	var mu sync.Mutex
	events := make([]testrun.Event, 0, 8)
	trace, err := New(
		testOperation(t),
		"component",
		testrun.EventSinkFunc(func(event testrun.Event) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, event)
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return trace, &events
}

func testOperation(t *testing.T) testrun.Operation {
	t.Helper()
	operation, err := testrun.NewOperation("run-1", "operation-1", "scenario")
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func eventsForMilestone(events []testrun.Event, milestone testrun.Milestone) []testrun.Event {
	result := make([]testrun.Event, 0, 2)
	for _, event := range events {
		if event.Milestone == string(milestone) {
			result = append(result, event)
		}
	}
	return result
}

func findEvent(
	t *testing.T,
	events []testrun.Event,
	milestone testrun.Milestone,
	outcome testrun.Outcome,
) testrun.Event {
	t.Helper()
	for _, event := range events {
		if event.Milestone == string(milestone) && event.Outcome == string(outcome) {
			return event
		}
	}
	t.Fatalf("event %s/%s not found in %#v", milestone, outcome, events)
	return testrun.Event{}
}

func assertTerminal(
	t *testing.T,
	events []testrun.Event,
	wantTerminal testrun.Outcome,
	wantCleanup testrun.Outcome,
) TerminalContext {
	t.Helper()
	cleanupEvents := eventsForMilestone(events, testrun.CleanupMilestone)
	if len(cleanupEvents) != 2 || cleanupEvents[0].Outcome != string(testrun.OutcomeStarted) ||
		cleanupEvents[1].Outcome != string(wantCleanup) {
		t.Fatalf("cleanup events = %#v", cleanupEvents)
	}
	lifecycleEvents := eventsForMilestone(events, testrun.ScenarioLifecycleMilestone)
	if len(lifecycleEvents) != 2 || lifecycleEvents[0].Outcome != string(testrun.OutcomeStarted) {
		t.Fatalf("scenario lifecycle events = %#v", lifecycleEvents)
	}
	terminal := lifecycleEvents[1]
	if terminal.Milestone != string(testrun.ScenarioLifecycleMilestone) ||
		terminal.Outcome != string(wantTerminal) {
		t.Fatalf("scenario terminal = %#v", terminal)
	}
	final := events[len(events)-1]
	if final.Milestone != terminal.Milestone || final.Outcome != terminal.Outcome {
		t.Fatalf("scenario terminal is not the final event: terminal=%#v final=%#v", terminal, final)
	}
	var payload TerminalContext
	if err := json.Unmarshal(terminal.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CleanupOutcome != wantCleanup {
		t.Fatalf("terminal cleanup outcome = %q, want %q", payload.CleanupOutcome, wantCleanup)
	}
	if payload.PriorEvidenceOutcome != testrun.OutcomeSucceeded &&
		payload.PriorEvidenceOutcome != testrun.OutcomeFailed {
		t.Fatalf("terminal prior evidence outcome = %q", payload.PriorEvidenceOutcome)
	}
	if payload.PriorDeliveryOutcome != testrun.OutcomeSucceeded &&
		payload.PriorDeliveryOutcome != testrun.OutcomeFailed {
		t.Fatalf("terminal prior delivery outcome = %q", payload.PriorDeliveryOutcome)
	}
	if err := testrun.ValidateMilestone(payload.LastMilestone); err != nil {
		t.Fatalf("terminal last milestone = %q: %v", payload.LastMilestone, err)
	}
	return payload
}
