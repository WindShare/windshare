package clievent

import (
	"errors"
	"testing"
)

func TestProtocolOperationEventRetainsOnlyBoundedDiagnosticFacts(t *testing.T) {
	session, _ := NewProtocolSessionID(bytes16(31))
	operation, _ := NewProtocolOperationID(bytes16(32))
	lane, _ := NewLaneIdentity(2, 1)
	event, err := NewProtocolOperationObserved(ProtocolOperationSpec{
		Command: CommandGet, Role: ProtocolRoleReceiver,
		Stage:           ProtocolOperationReceiverFailed,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind: ProtocolMessageReleaseLease,
		Lane:        lane, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome:             ProtocolSendDelivered,
		DeadlineRemainingMillis: 30_000, HasDeadline: true,
		OperationElapsedMillis: 30_000,
		UsableLanesAtSelection: 2, UsableLanesAtSettlement: 2,
		Cause: ProtocolOperationCauseDeadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if event.Command() != CommandGet || event.Level() != LevelDebug ||
		event.Role() != ProtocolRoleReceiver || event.Stage() != ProtocolOperationReceiverFailed ||
		event.ProtocolSessionID() != session || event.ProtocolOperationID() != operation ||
		event.RequestKind() != ProtocolMessageReleaseLease || event.Cause() != ProtocolOperationCauseDeadline ||
		event.ResponseCount() != 0 || event.OperationElapsedMillis() != 30_000 ||
		event.UsableLanesAtSelection() != 2 || event.UsableLanesAtSettlement() != 2 {
		t.Fatalf("protocol operation event = %#v", event)
	}
	if got, ok := event.Lane(); !ok || got != lane {
		t.Fatalf("lane = %#v, %v", got, ok)
	}
	if _, ok := event.ResponseKind(); ok {
		t.Fatal("failed wait unexpectedly retained a response kind")
	}
	if outcome, settled, admitted, ok := event.Send(); !ok || outcome != ProtocolSendDelivered || !settled || !admitted {
		t.Fatalf("send = %v settled=%v admitted=%v present=%v", outcome, settled, admitted, ok)
	}
	if deadline, ok := event.DeadlineRemainingMillis(); !ok || deadline != 30_000 {
		t.Fatalf("deadline = %d, %v", deadline, ok)
	}
}

func TestProtocolOperationEventRejectsContradictoryLifecycleFacts(t *testing.T) {
	session, _ := NewProtocolSessionID(bytes16(41))
	operation, _ := NewProtocolOperationID(bytes16(42))
	valid := ProtocolOperationSpec{
		Command: CommandGet, Role: ProtocolRoleReceiver,
		Stage:           ProtocolOperationReceiverFailed,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind: ProtocolMessageReleaseLease,
		Cause:       ProtocolOperationCauseDeadline,
	}
	tests := []struct {
		name   string
		mutate func(*ProtocolOperationSpec)
	}{
		{"sender command for receiver", func(spec *ProtocolOperationSpec) { spec.Command = CommandShare }},
		{"response flag without kind", func(spec *ProtocolOperationSpec) { spec.HasResponse = true }},
		{"response kind without flag", func(spec *ProtocolOperationSpec) { spec.ResponseKind = ProtocolMessageOperationComplete }},
		{"lane flag without identity", func(spec *ProtocolOperationSpec) { spec.HasLane = true }},
		{"send fact without send", func(spec *ProtocolOperationSpec) { spec.SendSettled = true }},
		{"deadline value without deadline", func(spec *ProtocolOperationSpec) { spec.DeadlineRemainingMillis = 1 }},
		{"non request kind", func(spec *ProtocolOperationSpec) { spec.RequestKind = ProtocolMessageOperationComplete }},
		{"failed stage without cause", func(spec *ProtocolOperationSpec) { spec.Cause = ProtocolOperationCauseNone }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			test.mutate(&spec)
			if _, err := NewProtocolOperationObserved(spec); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid protocol operation error = %v", err)
			}
		})
	}
}

func TestProtocolOperationEventCarriesTypedProtocolFailure(t *testing.T) {
	session, _ := NewProtocolSessionID(bytes16(51))
	operation, _ := NewProtocolOperationID(bytes16(52))
	lane, _ := NewLaneIdentity(7, 0)
	failure, err := NewResponseSendProtocolFailure(
		ProtocolFailureSpec{
			RequestKind:       ProtocolMessageRequestBlocks,
			WireScope:         ProtocolFailureRevision,
			WireCode:          0x3008,
			Retryable:         true,
			RetryAfterMillis:  30_000,
			HasRetryAfter:     true,
			ProtocolSession:   session,
			ProtocolOperation: operation,
			Lane:              lane,
			HasLane:           true,
		},
		ProtocolFailureResponseSendSettlement{
			Admitted: true, Settled: true, Outcome: ProtocolSendDelivered,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := NewProtocolOperationObserved(ProtocolOperationSpec{
		Command: CommandShare, Role: ProtocolRoleSender,
		Stage:           ProtocolOperationSenderResponseSettled,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind:  ProtocolMessageRequestBlocks,
		ResponseKind: ProtocolMessageOperationError, HasResponse: true,
		Lane: lane, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome: ProtocolSendDelivered, Failure: failure,
		Cause: ProtocolOperationCauseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	projected, ok := event.Failure()
	if !ok || projected.RequestKind() != ProtocolMessageRequestBlocks ||
		projected.WireScope() != ProtocolFailureRevision || projected.WireCode() != 0x3008 ||
		!projected.Retryable() || projected.ProtocolSessionID() != session ||
		projected.ProtocolOperationID() != operation {
		t.Fatalf("protocol failure = %#v, present=%v", projected, ok)
	}
	if retryAfter, present := projected.RetryAfterMillis(); !present || retryAfter != 30_000 {
		t.Fatalf("retry after = %d, present=%v", retryAfter, present)
	}
	if projectedLane, present := projected.Lane(); !present || projectedLane != lane {
		t.Fatalf("failure lane = %#v, present=%v", projectedLane, present)
	}
	response, present := projected.Settlement().ResponseSend()
	if !present || !response.Admitted || !response.Settled || response.Outcome != ProtocolSendDelivered {
		t.Fatalf("response settlement = %#v, present=%v", response, present)
	}
	contradictory := event.spec
	contradictory.SendOutcome = ProtocolSendDropped
	if _, err := NewProtocolOperationObserved(contradictory); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("contradictory outer settlement error = %v", err)
	}
}

func TestProtocolFailureRejectsInvalidRetrySettlementAndOuterCorrelation(t *testing.T) {
	session, _ := NewProtocolSessionID(bytes16(61))
	operation, _ := NewProtocolOperationID(bytes16(62))
	otherOperation, _ := NewProtocolOperationID(bytes16(63))
	lane, _ := NewLaneIdentity(3, 1)
	base := ProtocolFailureSpec{
		RequestKind: ProtocolMessageReleaseLease,
		WireScope:   ProtocolFailureRevision, WireCode: 0xffff,
		Retryable: true, RetryAfterMillis: 1, HasRetryAfter: true,
		ProtocolSession: session, ProtocolOperation: operation,
		Lane: lane, HasLane: true,
	}

	for _, test := range []struct {
		name   string
		mutate func(*ProtocolFailureSpec)
	}{
		{"retry without value", func(spec *ProtocolFailureSpec) { spec.HasRetryAfter = false }},
		{"zero retry", func(spec *ProtocolFailureSpec) { spec.RetryAfterMillis = 0 }},
		{"oversized retry", func(spec *ProtocolFailureSpec) { spec.RetryAfterMillis = 30_001 }},
		{"unknown scope", func(spec *ProtocolFailureSpec) { spec.WireScope = 255 }},
		{"hidden lane", func(spec *ProtocolFailureSpec) { spec.HasLane = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := base
			test.mutate(&spec)
			if _, err := NewReceivedAuthenticatedProtocolFailure(spec); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("invalid failure error = %v", err)
			}
		})
	}
	if _, err := NewResponseSendProtocolFailure(
		base,
		ProtocolFailureResponseSendSettlement{Outcome: ProtocolSendDelivered},
	); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unsettled delivery error = %v", err)
	}

	failure, err := NewReceivedAuthenticatedProtocolFailure(base)
	if err != nil {
		t.Fatal(err)
	}
	outer := ProtocolOperationSpec{
		Command: CommandGet, Role: ProtocolRoleReceiver,
		Stage:           ProtocolOperationReceiverFailed,
		ProtocolSession: session, ProtocolOperation: otherOperation,
		RequestKind:  ProtocolMessageReleaseLease,
		ResponseKind: ProtocolMessageOperationError, HasResponse: true,
		Lane: lane, HasLane: true, Failure: failure,
		Cause: ProtocolOperationCauseProtocolFailure,
	}
	if _, err := NewProtocolOperationObserved(outer); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("mismatched operation correlation error = %v", err)
	}
}
