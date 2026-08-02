package relayv2_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
)

type unboundedExchangeFailure struct {
	reason string
}

func (failure unboundedExchangeFailure) Error() string { return "synthetic frame exchange failure" }

func (failure unboundedExchangeFailure) FailureReason() string { return failure.reason }

func TestObserveRelayFrameExchangeRecordsFailureBeforeReturning(t *testing.T) {
	trace, events := newRecordedRelayTrace(t)
	cause := errors.New("sender accept failed")
	err := observeRelayFrameExchange(
		trace,
		relayFrameExchangeContext{ReceiverToSenderBytes: 3, SenderToReceiverBytes: 5},
		func() error { return newRelayFrameExchangeError(senderAcceptFailureReason, cause) },
	)
	if !errors.Is(err, cause) {
		t.Fatalf("observed exchange error = %v, want %v", err, cause)
	}
	assertFrameExchangePair(t, *events, testrun.OutcomeFailed, string(senderAcceptFailureReason))
	if err := trace.Finish(); !errors.Is(err, testscenario.ErrIncomplete) {
		t.Fatalf("failed exchange scenario Finish = %v, want ErrIncomplete", err)
	}
}

func TestObserveRelayFrameExchangeRecordsSuccessExactlyOnce(t *testing.T) {
	trace, events := newRecordedRelayTrace(t)
	if err := observeRelayFrameExchange(
		trace,
		relayFrameExchangeContext{ReceiverToSenderBytes: 7, SenderToReceiverBytes: 11},
		func() error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	assertFrameExchangePair(t, *events, testrun.OutcomeSucceeded, "")
	if err := trace.MarkFunctionalSuccess(); err != nil {
		t.Fatal(err)
	}
	if err := trace.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestObserveRelayFrameExchangeBoundsForeignFailureReason(t *testing.T) {
	trace, events := newRecordedRelayTrace(t)
	err := observeRelayFrameExchange(
		trace,
		relayFrameExchangeContext{},
		func() error {
			return unboundedExchangeFailure{
				reason: strings.Repeat("x", testscenario.MaximumFailureReasonBytes+1),
			}
		},
	)
	if err == nil {
		t.Fatal("synthetic exchange failure was accepted")
	}
	assertFrameExchangePair(t, *events, testrun.OutcomeFailed, unexpectedExchangeFailureReason)
	_ = trace.Finish()
}

func newRecordedRelayTrace(t *testing.T) (*testscenario.Trace, *[]testrun.Event) {
	t.Helper()
	operation, err := testrun.NewOperation("run-1", "operation-1", "relay/frame-exchange")
	if err != nil {
		t.Fatal(err)
	}
	events := make([]testrun.Event, 0, 8)
	trace, err := testscenario.New(
		operation,
		"integration_relayv2_test",
		testrun.EventSinkFunc(func(event testrun.Event) error {
			events = append(events, event)
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return trace, &events
}

func assertFrameExchangePair(
	t *testing.T,
	events []testrun.Event,
	wantTerminal testrun.Outcome,
	wantReason string,
) {
	t.Helper()
	phaseEvents := make([]testrun.Event, 0, 2)
	for _, event := range events {
		if event.Milestone == string(frameExchangeMilestone) {
			phaseEvents = append(phaseEvents, event)
		}
	}
	if len(phaseEvents) != 2 || phaseEvents[0].Outcome != string(testrun.OutcomeStarted) ||
		phaseEvents[1].Outcome != string(wantTerminal) {
		t.Fatalf("frame exchange events = %#v", phaseEvents)
	}
	if wantTerminal != testrun.OutcomeFailed {
		return
	}
	var failure testscenario.FailureContext
	if err := json.Unmarshal(phaseEvents[1].Payload, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Reason != wantReason {
		t.Fatalf("frame exchange failure reason = %q, want %q", failure.Reason, wantReason)
	}
}
