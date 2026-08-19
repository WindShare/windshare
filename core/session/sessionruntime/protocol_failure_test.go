package sessionruntime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestProtocolFailureConstructorsEnforceClosedCorrelationAndSettlement(t *testing.T) {
	spec := protocolFailureSpecForTest()
	received, err := NewReceivedAuthenticatedProtocolFailure(spec)
	if err != nil {
		t.Fatal(err)
	}
	if received.IsZero() ||
		received.RequestKind() != spec.RequestKind ||
		received.WireScope() != spec.WireScope ||
		received.WireCode() != spec.WireCode ||
		received.Retryable() ||
		received.ProtocolSessionID() != spec.ProtocolSessionID ||
		received.ProtocolOperationID() != spec.ProtocolOperationID {
		t.Fatalf("received failure = %+v", received)
	}
	if delay, present := received.RetryAfterMillis(); present || delay != 0 {
		t.Fatalf("permanent failure retry delay = %d, present=%v", delay, present)
	}
	if lane, present := received.Lane(); present || lane != (LaneIdentity{}) {
		t.Fatalf("received failure lane = %+v, present=%v", lane, present)
	}
	if received.Settlement().Kind() != ProtocolFailureSettlementReceivedAuthenticated {
		t.Fatalf("received settlement = %d", received.Settlement().Kind())
	}

	validResponses := []ProtocolFailureResponseSendSettlement{
		{Outcome: protocolsession.SendOutcomeUnknown},
		{Admitted: true, Settled: true, Outcome: protocolsession.SendOutcomeUnknown},
		{Settled: true, Outcome: protocolsession.SendOutcomeDropped},
		{Admitted: true, Settled: true, Outcome: protocolsession.SendOutcomeDelivered},
	}
	for _, settlement := range validResponses {
		failure, responseErr := NewResponseSendProtocolFailure(spec, settlement)
		if responseErr != nil || failure.IsZero() {
			t.Fatalf("valid response settlement %+v: failure=%+v error=%v", settlement, failure, responseErr)
		}
		got, present := failure.Settlement().ResponseSend()
		if !present || got != settlement {
			t.Fatalf("response settlement = %+v, present=%v, want %+v", got, present, settlement)
		}
	}

	invalidResponses := []ProtocolFailureResponseSendSettlement{
		{Admitted: true, Outcome: protocolsession.SendOutcomeDelivered},
		{Settled: true, Outcome: protocolsession.SendOutcomeDelivered},
		{Outcome: protocolsession.SendOutcomeDropped},
		{Outcome: protocolsession.SendOutcome(255)},
	}
	for _, settlement := range invalidResponses {
		if _, responseErr := NewResponseSendProtocolFailure(spec, settlement); responseErr == nil {
			t.Fatalf("invalid response settlement accepted: %+v", settlement)
		}
	}
}

func TestProtocolFailureRejectsInvalidReviewedFacts(t *testing.T) {
	tests := map[string]func(*ProtocolFailureSpec){
		"request kind": func(spec *ProtocolFailureSpec) {
			spec.RequestKind = protocolsession.MessageOperationError
		},
		"wire scope": func(spec *ProtocolFailureSpec) {
			spec.WireScope = ProtocolFailureScope(255)
		},
		"session identity": func(spec *ProtocolFailureSpec) {
			spec.ProtocolSessionID = protocolsession.ProtocolSessionID{}
		},
		"operation identity": func(spec *ProtocolFailureSpec) {
			spec.ProtocolOperationID = protocolsession.OperationID{}
		},
		"retry presence": func(spec *ProtocolFailureSpec) {
			spec.Retryable = true
		},
		"retry minimum": func(spec *ProtocolFailureSpec) {
			spec.Retryable, spec.HasRetryAfter = true, true
		},
		"retry maximum": func(spec *ProtocolFailureSpec) {
			spec.Retryable, spec.HasRetryAfter = true, true
			spec.RetryAfterMillis = 30_001
		},
		"invalid lane": func(spec *ProtocolFailureSpec) {
			spec.HasLane = true
		},
		"unclaimed lane": func(spec *ProtocolFailureSpec) {
			spec.Lane = LaneIdentity{ID: 1, Epoch: 0}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := protocolFailureSpecForTest()
			mutate(&spec)
			if _, err := NewReceivedAuthenticatedProtocolFailure(spec); err == nil {
				t.Fatalf("invalid protocol failure accepted: %+v", spec)
			}
		})
	}

	retryable := protocolFailureSpecForTest()
	retryable.Retryable = true
	retryable.HasRetryAfter = true
	retryable.RetryAfterMillis = 1
	if _, err := NewReceivedAuthenticatedProtocolFailure(retryable); err != nil {
		t.Fatalf("minimum retry delay rejected: %v", err)
	}
	retryable.RetryAfterMillis = 30_000
	if _, err := NewReceivedAuthenticatedProtocolFailure(retryable); err != nil {
		t.Fatalf("maximum retry delay rejected: %v", err)
	}
}

func TestProtocolFailureValueCannotRetainProviderTextOrBody(t *testing.T) {
	for _, value := range []any{ProtocolFailure{}, ProtocolFailureSpec{}} {
		valueType := reflect.TypeOf(value)
		for field := range valueType.Fields() {
			if field.Type.Kind() == reflect.String ||
				field.Type.Kind() == reflect.Slice ||
				field.Type.Kind() == reflect.Interface {
				t.Fatalf("%s admits open provider data through %s %s", valueType, field.Name, field.Type)
			}
		}
	}
}

func TestProtocolFailureTracingLeavesDisabledInboundHotPathUnbound(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	operationID := id16[protocolsession.OperationID](0x51)
	message, err := protocolsession.NewMessage(
		protocolsession.MessageOperationError,
		&operationID,
		[]byte{0xf6},
	)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := (laneInboundRouter{
		runtime:  runtime,
		identity: runtime.initial,
	}).prepareInboundRoute(context.Background(), message)
	if err != nil {
		t.Fatal(err)
	}
	if lane, present := inboundLane(binding.ctx); present {
		t.Fatalf("disabled trace bound inbound lane %+v", lane)
	}

	call := newOperationCall(
		operationID,
		protocolsession.MessageOpenRevisions,
		time.Time{},
		0,
		false,
		false,
	)
	if err := call.enqueue(operationResponse{message: message}); err != nil {
		t.Fatal(err)
	}
	if !call.traceFailure.IsZero() || call.traceCause != ProtocolOperationCauseNone {
		t.Fatalf("disabled call retained protocol failure: %+v", call.traceFailure)
	}
}

func protocolFailureSpecForTest() ProtocolFailureSpec {
	return ProtocolFailureSpec{
		RequestKind:         protocolsession.MessageOpenRevisions,
		WireScope:           ProtocolFailureRevision,
		WireCode:            0x3008,
		ProtocolSessionID:   id16[protocolsession.ProtocolSessionID](0x41),
		ProtocolOperationID: id16[protocolsession.OperationID](0x42),
	}
}
