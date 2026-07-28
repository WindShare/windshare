package protocolsession

import (
	"context"
	"errors"
	"testing"
)

func TestRouterContextCapabilitiesRemainBoundToExactOperation(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
	if err != nil {
		t.Fatal(err)
	}
	operationID := testOperationID(243)
	request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
	admission, err := table.AdmitOutbound(
		DirectionReceiverToSender,
		request,
		OutboundOperationPermit{},
	)
	if err != nil || admission.Generation.IsZero() || admission.Operation.IsZero() {
		t.Fatalf("request admission=%+v error=%v", admission, err)
	}
	defer admission.pin.release()

	generationContext := WithOperationGeneration(routerMissingContext(), admission.Generation)
	if generation, ok := OperationGenerationFromContext(generationContext, operationID); !ok || !generation.Same(admission.Generation) {
		t.Fatalf("generation context=%+v ok=%v", generation, ok)
	}
	if _, ok := OperationGenerationFromContext(routerMissingContext(), operationID); ok {
		t.Fatal("nil context exposed a generation")
	}
	if _, ok := OperationGenerationFromContext(generationContext, OperationID{}); ok {
		t.Fatal("zero operation identity exposed a generation")
	}
	if _, ok := OperationGenerationFromContext(generationContext, testOperationID(244)); ok {
		t.Fatal("generation crossed operation identity")
	}

	permitContext := WithOutboundOperationPermit(routerMissingContext(), admission.Operation)
	if permit, ok := OutboundOperationPermitFromContext(permitContext, operationID); !ok || permit.IsZero() || permit.operationID != operationID {
		t.Fatalf("permit context=%+v ok=%v", permit, ok)
	}
	if _, ok := OutboundOperationPermitFromContext(routerMissingContext(), operationID); ok {
		t.Fatal("nil context exposed a permit")
	}
	if _, ok := OutboundOperationPermitFromContext(permitContext, OperationID{}); ok {
		t.Fatal("zero operation identity exposed a permit")
	}
	if _, ok := OutboundOperationPermitFromContext(
		context.WithValue(context.Background(), outboundOperationPermitContextKey{}, "foreign"),
		operationID,
	); ok {
		t.Fatal("foreign context value became an operation permit")
	}
	if _, ok := OutboundOperationPermitFromContext(permitContext, testOperationID(245)); ok {
		t.Fatal("permit crossed operation identity")
	}
}

func TestRetainedMessageContextSeparatesValuesFromCancellation(t *testing.T) {
	type contextKey string
	const (
		messageKey  contextKey = "message"
		lifetimeKey contextKey = "lifetime"
	)

	messageContext, cancelMessage := context.WithCancel(
		context.WithValue(context.Background(), messageKey, "authenticated"),
	)
	cancelMessage()
	lifetime, cancelLifetime := context.WithCancel(
		context.WithValue(context.Background(), lifetimeKey, "service"),
	)
	retained := RetainMessageContext(lifetime, messageContext)
	if retained.Value(messageKey) != "authenticated" || retained.Value(lifetimeKey) != "service" ||
		retained.Err() != nil {
		t.Fatalf("retained context values message=%v lifetime=%v error=%v",
			retained.Value(messageKey), retained.Value(lifetimeKey), retained.Err())
	}
	cancelLifetime()
	if !errors.Is(retained.Err(), context.Canceled) {
		t.Fatalf("retained context ignored service cancellation: %v", retained.Err())
	}

	nilLifetime := RetainMessageContext(routerMissingContext(), context.WithValue(context.Background(), messageKey, "value"))
	if nilLifetime.Value(messageKey) != "value" || nilLifetime.Done() != nil {
		t.Fatalf("nil-lifetime context value=%v done=%v", nilLifetime.Value(messageKey), nilLifetime.Done())
	}
	direct := retainedMessageContext{
		Context: context.WithValue(context.Background(), lifetimeKey, "fallback"),
	}
	if direct.Value(lifetimeKey) != "fallback" {
		t.Fatalf("retained context fallback=%v", direct.Value(lifetimeKey))
	}
}

// routerMissingContext keeps intentional nil-capability coverage distinct from
// accidental nil Context arguments in ordinary tests.
func routerMissingContext() context.Context { return nil }

func TestRoleRouterReplayTerminalAndClosedStateContracts(t *testing.T) {
	t.Run("exact outbound replay", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 4, MaxTombstones: 4}, nil)
		router, _ := NewRoleRouter(RoleReceiver, table)
		operationID := testOperationID(246)
		request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
		admission, err := router.AdmitOutbound(request, OutboundOperationPermit{})
		if err != nil || admission.Replay.IsZero() {
			t.Fatalf("request admission=%+v error=%v", admission, err)
		}
		replayed, err := router.AcceptOutboundReplay(request, admission.Replay)
		if err != nil || replayed.Disposition != OperationDeliver ||
			!replayed.Generation.Same(admission.Generation) {
			t.Fatalf("request replay=%+v error=%v", replayed, err)
		}
		replayed.pin.release()
		admission.pin.release()
	})

	t.Run("authenticated terminal bypasses backlog", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouter(RoleReceiver, table)
		terminal := mustMessage(t, MessageSessionTerminal, nil, map[uint64]any{0: uint64(1)})
		disposition, err := router.RouteInbound(context.Background(), terminal)
		if err != nil || disposition != OperationSessionTerminal || len(router.control) != 0 || len(router.data) != 0 {
			t.Fatalf("terminal disposition=%d error=%v control=%d data=%d",
				disposition, err, len(router.control), len(router.data))
		}
	})

	t.Run("closed router rejects every authority mutation", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouter(RoleSender, table)
		operationID := testOperationID(247)
		request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
		router.Close()
		router.Close()
		if err := router.RegisterHandler(MessageListChildren, MessageHandlerFunc(func(context.Context, Message) error {
			return nil
		})); !errors.Is(err, ErrSessionTerminated) {
			t.Fatalf("register after close error=%v", err)
		}
		if err := router.Dispatch(context.Background(), RouteEvent{message: request, hasMessage: true}); !errors.Is(err, ErrSessionTerminated) {
			t.Fatalf("dispatch after close error=%v", err)
		}
		if disposition, err := router.RouteInbound(context.Background(), request); disposition != OperationDrop || !errors.Is(err, ErrSessionTerminated) {
			t.Fatalf("route after close disposition=%d error=%v", disposition, err)
		}
		if bound, err := router.RouteAuthenticatedOperationViolation(
			context.Background(),
			request,
			AuthenticatedOperationViolation{code: AuthenticatedOperationViolationMalformedFailure},
		); bound || !errors.Is(err, ErrSessionTerminated) {
			t.Fatalf("violation after close bound=%v error=%v", bound, err)
		}
		var nilRouter *RoleRouter
		nilRouter.Close()
		if bound, err := nilRouter.RouteAuthenticatedOperationViolation(
			context.Background(), request, AuthenticatedOperationViolation{},
		); bound || !errors.Is(err, ErrNilRuntimeDependency) {
			t.Fatalf("nil-router violation bound=%v error=%v", bound, err)
		}
	})

	t.Run("unmatched authenticated violation is isolated", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouter(RoleSender, table)
		operationID := testOperationID(248)
		unmatched := mustMessage(t, MessageOperationError, &operationID, map[uint64]any{0: uint64(1)})
		bound, err := router.RouteAuthenticatedOperationViolation(
			context.Background(),
			unmatched,
			AuthenticatedOperationViolation{code: AuthenticatedOperationViolationMalformedFailure},
		)
		if err != nil || bound {
			t.Fatalf("unmatched violation bound=%v error=%v", bound, err)
		}
	})
}

func TestRoleRouterOverflowPreservesAuthenticatedOrderAndFailureScope(t *testing.T) {
	t.Run("final cannot overtake admitted data", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouterWithLimits(
			RoleReceiver,
			table,
			RouterLimits{ControlFrames: 1, DataFrames: 1},
		)
		generation := OperationGeneration{authority: &operationAuthority{}}
		router.pendingData[generation] = 1
		router.data <- RouteEvent{}
		deferred, err := router.enqueueFinalAfterData(RouteEvent{generation: generation})
		if deferred || !errors.Is(err, ErrRouterDataFull) {
			t.Fatalf("full final queue deferred=%v error=%v", deferred, err)
		}
	})

	t.Run("invalid overflow generation remains isolated", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouter(RoleReceiver, table)
		if disposition, err := router.handleDataOverflow(testOperationID(249), OperationGeneration{}); disposition != OperationDrop || err == nil {
			t.Fatalf("invalid overflow disposition=%d error=%v", disposition, err)
		}
	})

	t.Run("overflow error cannot displace control backlog", func(t *testing.T) {
		table, _ := NewOperationTable(OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil)
		router, _ := NewRoleRouterWithLimits(
			RoleReceiver,
			table,
			RouterLimits{ControlFrames: 1, DataFrames: 1},
		)
		operationID := testOperationID(250)
		request := mustMessage(t, MessageListChildren, &operationID, map[uint64]any{0: uint64(1)})
		admission, err := table.AdmitOutbound(DirectionReceiverToSender, request, OutboundOperationPermit{})
		if err != nil {
			t.Fatal(err)
		}
		admission.pin.release()
		router.control <- RouteEvent{message: request, hasMessage: true}
		disposition, err := router.handleDataOverflow(operationID, admission.Generation)
		if disposition != OperationDrop || !errors.Is(err, ErrRouterControlFull) || len(router.control) != 1 {
			t.Fatalf("overflow disposition=%d error=%v control=%d", disposition, err, len(router.control))
		}
	})
}
