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

func TestProtocolOperationEventExposesBoundedOperationErrorClassification(t *testing.T) {
	session, _ := NewProtocolSessionID(bytes16(51))
	operation, _ := NewProtocolOperationID(bytes16(52))
	event, err := NewProtocolOperationObserved(ProtocolOperationSpec{
		Command: CommandShare, Role: ProtocolRoleSender,
		Stage:           ProtocolOperationSenderResponseSettled,
		ProtocolSession: session, ProtocolOperation: operation,
		RequestKind:  ProtocolMessageRequestBlocks,
		ResponseKind: ProtocolMessageOperationError, HasResponse: true,
		OperationErrorScope: ProtocolOperationErrorRevision,
		OperationErrorCode:  0x3008, HasOperationError: true,
		Cause: ProtocolOperationCauseNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	scope, code, retryable, ok := event.OperationError()
	if !ok || scope != ProtocolOperationErrorRevision || code != 0x3008 || retryable {
		t.Fatalf("operation error = scope=%d code=%x retryable=%v present=%v", scope, code, retryable, ok)
	}
}
