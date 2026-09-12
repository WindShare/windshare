package clievent

import (
	"errors"
	"testing"
	"time"
)

func protocolTestContext(t *testing.T) ProtocolObservationContext {
	t.Helper()
	session, _ := NewProtocolSessionID(bytes16(31))
	operation, _ := NewProtocolOperationID(bytes16(32))
	return ProtocolObservationContext{Command: CommandShare, ObservedAt: time.Unix(1, 2), Role: ProtocolRoleSender, ProtocolSession: session, ProtocolOperation: operation, RequestKind: ProtocolMessageOpenRevisions}
}
func TestProtocolObservationFactsAndImmutableResult(t *testing.T) {
	context := protocolTestContext(t)
	lane, _ := NewLaneIdentity(2, 0)
	attempt, err := NewSendAttemptSnapshot(SendAttemptSpec{AttemptSequence: 1, Lane: lane, PolicyAdmitted: true, Outcome: ProtocolSendUnknown, End: SendAttemptEndWaitingEnded})
	if err != nil {
		t.Fatal(err)
	}
	attempts := []SendAttemptSnapshot{attempt}
	result, err := NewResponseSendResult(ResponseSendResultSpec{Started: true, Evidence: ResponseSendEvidenceUncertain, End: ResponseSendEndCallerCanceled, Cleanup: SendCleanupRouteReleased, Attempts: attempts, PendingAttemptSequence: 1, HasPendingAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	attempts[0] = SendAttemptSnapshot{}
	captured, ok := result.Attempt(0)
	if !ok || captured != attempt || result.AttemptCount() != 1 {
		t.Fatal("caller mutation changed retained evidence")
	}
	if _, ok := result.Attempt(-1); ok {
		t.Fatal("negative attempt index")
	}
	if _, ok := result.Attempt(1); ok {
		t.Fatal("out of bounds attempt index")
	}
	content, err := NewProtocolErrorContent(ProtocolErrorContentSpec{WireScope: ProtocolErrorRevision, WireCode: 0x3008, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 125})
	if err != nil {
		t.Fatal(err)
	}
	returned, err := NewResponseSendReturnedObserved(context, 99, ProtocolMessageOperationError, content, result)
	if err != nil {
		t.Fatal(err)
	}
	fact := returned.Fact().(ResponseSendReturnedFact)
	if !returned.ObservedAt().Equal(context.ObservedAt) || fact.ResponseSequence() != 99 || fact.Result().Evidence() != ResponseSendEvidenceUncertain || fact.Content() != content {
		t.Fatal("response fact lost source facts")
	}
	pending, ok := fact.Result().PendingAttemptSequence()
	if !ok || pending != 1 {
		t.Fatal("pending link missing")
	}
	notStarted, err := NewResponseSendResult(ResponseSendResultSpec{Evidence: ResponseSendEvidenceDefinitelyNotSent, End: ResponseSendEndRouteUnavailable})
	if err != nil {
		t.Fatal(err)
	}
	setup, err := NewResponseSendNotStartedObserved(context, 100, ProtocolMessageOperationError, content, notStarted)
	if err != nil {
		t.Fatal(err)
	}
	if setup.Fact().(ResponseSendNotStartedFact).Result().AttemptCount() != 0 {
		t.Fatal("invented attempt for failed setup")
	}
	settled, err := NewSendAttemptSnapshot(SendAttemptSpec{AttemptSequence: 1, Lane: lane, PolicyAdmitted: true, Settled: true, Outcome: ProtocolSendTransportConfirmed, TransportDisposition: SendAccepted, HasTransportDisposition: true, End: SendAttemptEndSettled})
	if err != nil {
		t.Fatal(err)
	}
	late, err := NewSendAttemptSettledObserved(context, 99, ProtocolMessageOperationError, settled)
	if err != nil {
		t.Fatal(err)
	}
	if late.Fact().(SendAttemptSettledFact).Attempt().Lane() != lane || fact.Result().Evidence() != ResponseSendEvidenceUncertain {
		t.Fatal("late settlement mutated earlier snapshot")
	}
	received, err := NewReceivedProtocolErrorObserved(context, content, lane)
	if err != nil {
		t.Fatal(err)
	}
	if received.Fact().(ReceivedProtocolErrorFact).Lane() != lane {
		t.Fatal("received lane missing")
	}
	decisionID, _ := NewCapacityDecisionID("capacity-1")
	decision, _ := NewSenderCapacityDecision(decisionID)
	decisionEvent, err := NewSenderContentDecisionObserved(context, decision, lane, true)
	if err != nil {
		t.Fatal(err)
	}
	if decisionEvent.Fact().(SenderContentDecisionFact).Decision() != decision {
		t.Fatal("decision changed")
	}
	for _, event := range []ProtocolObservationObserved{returned, setup, late, received, decisionEvent} {
		if err := event.Accept(&exhaustiveVisitor{}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(event.Accept(nil), ErrInvalidEvent) {
			t.Fatal("nil visitor accepted")
		}
	}
}
func TestProtocolObservationRejectsUninitializedAndFormatViolations(t *testing.T) {
	context := protocolTestContext(t)
	if _, err := NewResponseSendReturnedObserved(context, 1, ProtocolMessageOperationError, ProtocolErrorContent{}, ResponseSendResult{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal(err)
	}
	if err := (ProtocolObservationObserved{}).Accept(&exhaustiveVisitor{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal(err)
	}
	for _, spec := range []ProtocolErrorContentSpec{
		{WireScope: 255}, {WireScope: ProtocolErrorRevision, Retryable: true}, {WireScope: ProtocolErrorRevision, RetryAfterMillis: 1}, {WireScope: ProtocolErrorRevision, Retryable: true, HasRetryAfter: true, RetryAfterMillis: 30001},
	} {
		if _, err := NewProtocolErrorContent(spec); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("invalid content accepted: %+v", spec)
		}
	}
	for _, mutate := range []func(*ProtocolObservationContext){func(c *ProtocolObservationContext) { c.ObservedAt = time.Time{} }, func(c *ProtocolObservationContext) { c.Role = 255 }, func(c *ProtocolObservationContext) { c.ProtocolSession = ProtocolSessionID{} }, func(c *ProtocolObservationContext) { c.RequestKind = ProtocolMessageBlockFragment }} {
		invalid := context
		mutate(&invalid)
		if err := validateProtocolContext(invalid); !errors.Is(err, ErrInvalidEvent) {
			t.Fatal("invalid context accepted")
		}
	}
}
