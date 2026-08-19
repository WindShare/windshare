package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type protocolTraceRecorder struct {
	mu     sync.Mutex
	events []ProtocolOperationTrace
}

func (recorder *protocolTraceRecorder) TraceProtocolOperation(event ProtocolOperationTrace) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
}

func (recorder *protocolTraceRecorder) snapshot() []ProtocolOperationTrace {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]ProtocolOperationTrace(nil), recorder.events...)
}

func TestProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline(t *testing.T) {
	synctest.Test(t, testProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline)
}

func testProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x63}, protocolsession.IdentityBytes)))
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	call, err := rpc.begin(ctx, protocolsession.MessageReleaseLease, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.await(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release lease wait error = %v", err)
	}
	_ = rpc.cancelAndEnd(call, contentflow.CancelReasonTimeout)

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("protocol trace events = %d, want 1: %+v", len(events), events)
	}
	event := events[0]
	if event.Stage != ProtocolOperationReceiverFailed ||
		event.Cause != ProtocolOperationCauseDeadline ||
		event.RequestKind != protocolsession.MessageReleaseLease ||
		event.OperationID != call.id || event.ProtocolSessionID != runtime.sessionID ||
		!event.HasLane || event.Lane != runtime.initial ||
		!event.HasSend || !event.SendSettled || !event.SendAdmitted ||
		event.SendOutcome != protocolsession.SendOutcomeDelivered ||
		event.HasResponse || event.ResponseCount != 0 ||
		!event.HasDeadline || event.DeadlineRemainingMillis != 30_000 ||
		event.OperationElapsedMillis != 30_000 ||
		event.UsableLanesAtSelection != 1 || event.UsableLanesAtSettlement != 1 {
		t.Fatalf("release lease protocol trace = %+v", event)
	}
}

func TestSenderProtocolOperationTraceCorrelatesRequestAndResponse(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	operationID := id16[protocolsession.OperationID](0x71)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageReleaseLease, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x72}, ed25519.SeedSize))
	outbound := senderOutbound{runtime: runtime, privateKey: privateKey}
	responseBody, err := contentflow.EncodeOperationComplete(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.RegisterHandler(
		protocolsession.MessageReleaseLease,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, message protocolsession.Message) error {
			id, ok := message.OperationID()
			if !ok {
				return ErrOperationMissing
			}
			_, sendErr := outbound.SendControl(
				ctx, protocolsession.MessageOperationComplete, id, responseBody,
			)
			return sendErr
		}),
	); err != nil {
		t.Fatal(err)
	}
	inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
	if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("route release lease: disposition=%d error=%v", disposition, err)
	}
	queued, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("sender protocol trace events = %d, want 2: %+v", len(events), events)
	}
	received, responded := events[0], events[1]
	if received.Stage != ProtocolOperationSenderRequestReceived ||
		received.OperationID != operationID || received.RequestKind != protocolsession.MessageReleaseLease ||
		!received.HasLane || received.Lane != runtime.initial || received.Cause != ProtocolOperationCauseNone {
		t.Fatalf("sender request trace = %+v", received)
	}
	if responded.Stage != ProtocolOperationSenderResponseSettled ||
		responded.OperationID != operationID || responded.RequestKind != protocolsession.MessageReleaseLease ||
		!responded.HasResponse || responded.ResponseKind != protocolsession.MessageOperationComplete ||
		!responded.HasSend || !responded.SendSettled || !responded.SendAdmitted ||
		responded.SendOutcome != protocolsession.SendOutcomeDelivered ||
		!responded.HasLane || responded.Lane != runtime.initial ||
		responded.Cause != ProtocolOperationCauseNone {
		t.Fatalf("sender response trace = %+v", responded)
	}
}

func TestSenderProtocolOperationTraceCapturesFailureSendSettlement(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	selected, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- selected.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	operationID := id16[protocolsession.OperationID](0x76)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageOpenRevisions,
		&operationID,
		[]byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x77}, ed25519.SeedSize))
	outbound := senderOutbound{runtime: runtime, privateKey: privateKey}
	failureBody, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope:      protocolsession.OperationScopeRevision,
		Code:       0x3008,
		Retryable:  true,
		RetryAfter: 2 * time.Second,
		Message:    "provider-only revision detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, message protocolsession.Message) error {
			id, ok := message.OperationID()
			if !ok {
				return ErrOperationMissing
			}
			_, sendErr := outbound.SendControl(
				ctx,
				protocolsession.MessageOperationError,
				id,
				failureBody,
			)
			return sendErr
		}),
	); err != nil {
		t.Fatal(err)
	}
	inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
	if disposition, routeErr := inbound.RouteInbound(context.Background(), request); routeErr != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("route open revisions: disposition=%d error=%v", disposition, routeErr)
	}
	queued, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("sender protocol trace events = %d, want 2: %+v", len(events), events)
	}
	event := events[1]
	failure := event.Failure
	if event.Stage != ProtocolOperationSenderResponseSettled ||
		event.RequestKind != protocolsession.MessageOpenRevisions ||
		event.ResponseKind != protocolsession.MessageOperationError ||
		failure.IsZero() ||
		failure.RequestKind() != protocolsession.MessageOpenRevisions ||
		failure.WireScope() != ProtocolFailureRevision ||
		failure.WireCode() != 0x3008 ||
		!failure.Retryable() ||
		failure.ProtocolSessionID() != runtime.sessionID ||
		failure.ProtocolOperationID() != operationID {
		t.Fatalf("sender failure trace = %+v", event)
	}
	if retryAfter, present := failure.RetryAfterMillis(); !present || retryAfter != 2_000 {
		t.Fatalf("sender failure retry after = %d, present=%v", retryAfter, present)
	}
	if lane, present := failure.Lane(); !present || lane != runtime.initial {
		t.Fatalf("sender failure lane = %+v, present=%v", lane, present)
	}
	response, present := failure.Settlement().ResponseSend()
	if !present || !response.Admitted || !response.Settled ||
		response.Outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("sender failure settlement = %+v, present=%v", response, present)
	}
}

func TestProtocolOperationTraceSuppressesSuccessfulTransferHotPath(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	operationID := id16[protocolsession.OperationID](0x78)

	for _, event := range []ProtocolOperationTrace{
		{
			Stage: ProtocolOperationReceiverCompleted, OperationID: operationID,
			RequestKind:  protocolsession.MessageRequestBlocks,
			ResponseKind: protocolsession.MessageOperationComplete, HasResponse: true,
			HasSend: true, SendSettled: true, SendAdmitted: true,
			SendOutcome: protocolsession.SendOutcomeDelivered,
		},
		{
			Stage: ProtocolOperationSenderRequestReceived, OperationID: operationID,
			RequestKind: protocolsession.MessageRequestBlocks,
		},
		{
			Stage: ProtocolOperationSenderResponseSettled, OperationID: operationID,
			RequestKind:  protocolsession.MessageListChildren,
			ResponseKind: protocolsession.MessageScanProgress, HasResponse: true,
			HasSend: true, SendSettled: true, SendAdmitted: true,
			SendOutcome: protocolsession.SendOutcomeDelivered,
		},
	} {
		runtime.traceProtocolOperation(event)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("successful hot-path protocol events were retained: %+v", events)
	}

	runtime.traceProtocolOperation(ProtocolOperationTrace{
		Stage: ProtocolOperationReceiverFailed, OperationID: operationID,
		RequestKind: protocolsession.MessageRequestBlocks,
		Cause:       ProtocolOperationCauseDeadline,
	})
	if events := recorder.snapshot(); len(events) != 1 || events[0].Cause != ProtocolOperationCauseDeadline {
		t.Fatalf("exceptional hot-path protocol event was lost: %+v", events)
	}
}

func TestProtocolFailureForResponseSendRetainsReviewedFactsAndSettlement(t *testing.T) {
	body, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope:      protocolsession.OperationScopeRevision,
		Code:       0x3008,
		Retryable:  true,
		RetryAfter: protocolsession.MaxOperationFailureRetryAfter,
		Message:    "provider text must not enter the trace",
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := id16[protocolsession.ProtocolSessionID](0x7b)
	operationID := id16[protocolsession.OperationID](0x7c)
	lane := LaneIdentity{ID: 9, Epoch: 4}
	failure, ok := protocolFailureForResponseSend(
		sessionID,
		operationID,
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageOperationError,
		body,
		lane,
		true,
		protocolsession.SendCompletion{
			Settled: true, Admitted: true, Outcome: protocolsession.SendOutcomeDelivered,
		},
	)
	if !ok || failure.IsZero() ||
		failure.RequestKind() != protocolsession.MessageOpenRevisions ||
		failure.WireScope() != ProtocolFailureRevision ||
		failure.WireCode() != 0x3008 ||
		!failure.Retryable() ||
		failure.ProtocolSessionID() != sessionID ||
		failure.ProtocolOperationID() != operationID {
		t.Fatalf("response-send protocol failure = %+v, present=%v", failure, ok)
	}
	if retryAfter, present := failure.RetryAfterMillis(); !present || retryAfter != 30_000 {
		t.Fatalf("retry after = %d, present=%v", retryAfter, present)
	}
	if gotLane, present := failure.Lane(); !present || gotLane != lane {
		t.Fatalf("failure lane = %+v, present=%v", gotLane, present)
	}
	settlement := failure.Settlement()
	response, present := settlement.ResponseSend()
	if settlement.Kind() != ProtocolFailureSettlementResponseSend || !present ||
		!response.Admitted || !response.Settled ||
		response.Outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("response-send settlement = %+v, present=%v", response, present)
	}

	if _, malformed := protocolFailureForResponseSend(
		sessionID,
		operationID,
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageOperationError,
		[]byte("provider text must not enter the trace"),
		lane,
		true,
		protocolsession.SendCompletion{Settled: true, Outcome: protocolsession.SendOutcomeDropped},
	); malformed {
		t.Fatal("malformed operation error exposed unverified classification")
	}
}

func TestProtocolOperationTraceCapturesAuthenticatedReceivedFailureAtSource(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x7d}, protocolsession.IdentityBytes)))
	if err := rpc.register(runtime.router); err != nil {
		t.Fatal(err)
	}
	selected, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- selected.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	call, err := rpc.begin(context.Background(), protocolsession.MessageRequestBlocks, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope:      protocolsession.OperationScopeBlock,
		Code:       0x4003,
		Retryable:  true,
		RetryAfter: 1250 * time.Millisecond,
		Message:    "private provider detail",
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := runtime.senderControlBase(runtime.initial)
	binding.Sequence = 1
	binding.MessageKind = protocolsession.MessageOperationError
	binding.OperationID = call.id
	binding.HasOperationID = true
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize))
	signed, err := protocolsession.SignControlBody(
		privateKey,
		protocolsession.ControlDomainOperation,
		binding,
		semantic,
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(
		protocolsession.MessageOperationError,
		&call.id,
		signed,
	)
	if err != nil {
		t.Fatal(err)
	}
	inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
	disposition, err := inbound.RouteInbound(context.Background(), message)
	if err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("route authenticated failure: disposition=%d error=%v", disposition, err)
	}
	queued, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if response, err := rpc.await(context.Background(), call); err != nil ||
		response.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("await authenticated failure: kind=%d error=%v", response.Kind(), err)
	}
	rpc.end(call)

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("protocol trace events = %d, want 1: %+v", len(events), events)
	}
	event := events[0]
	failure := event.Failure
	if event.Stage != ProtocolOperationReceiverFailed ||
		event.Cause != ProtocolOperationCauseProtocolFailure ||
		failure.IsZero() ||
		failure.RequestKind() != protocolsession.MessageRequestBlocks ||
		failure.WireScope() != ProtocolFailureBlock ||
		failure.WireCode() != 0x4003 ||
		!failure.Retryable() ||
		failure.ProtocolSessionID() != runtime.sessionID ||
		failure.ProtocolOperationID() != call.id {
		t.Fatalf("authenticated receive trace = %+v", event)
	}
	if retryAfter, present := failure.RetryAfterMillis(); !present || retryAfter != 1_250 {
		t.Fatalf("retry after = %d, present=%v", retryAfter, present)
	}
	if lane, present := failure.Lane(); !present || lane != runtime.initial {
		t.Fatalf("authenticated receive lane = %+v, present=%v", lane, present)
	}
	settlement := failure.Settlement()
	if settlement.Kind() != ProtocolFailureSettlementReceivedAuthenticated {
		t.Fatalf("authenticated receive settlement kind = %d", settlement.Kind())
	}
	if response, present := settlement.ResponseSend(); present {
		t.Fatalf("authenticated receive exposed response send settlement: %+v", response)
	}
}

func TestProtocolOperationTraceCorrelatesLaneGrantWithoutAuthenticatedBody(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := &protocolTraceRecorder{}
	runtime.protocolTracer = recorder
	operationID := id16[protocolsession.OperationID](0x7a)
	lane := LaneIdentity{ID: 7, Epoch: 2}
	runtime.traceProtocolOperation(ProtocolOperationTrace{
		Stage: ProtocolOperationReceiverCompleted, OperationID: operationID,
		RequestKind: protocolsession.MessageLaneAttach, ResponseKind: protocolsession.MessageLaneAttach,
		HasResponse: true, Lane: lane, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome: protocolsession.SendOutcomeDelivered, ResponseCount: 1,
	})
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("lane-grant trace events = %d, want 1", len(events))
	}
	event := events[0]
	if event.ProtocolSessionID != runtime.sessionID || event.OperationID != operationID ||
		event.RequestKind != protocolsession.MessageLaneAttach ||
		event.ResponseKind != protocolsession.MessageLaneAttach || !event.HasResponse ||
		!event.HasLane || event.Lane != lane || event.ResponseCount != 1 ||
		event.SendOutcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("lane-grant trace = %+v", event)
	}
}

func TestProtocolOperationTracerPanicCannotChangeRuntimeAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.protocolTracer = ProtocolOperationTraceFunc(func(ProtocolOperationTrace) {
		panic("observer failure")
	})
	operationID := id16[protocolsession.OperationID](0x79)
	runtime.traceProtocolOperation(ProtocolOperationTrace{
		Stage:       ProtocolOperationReceiverFailed,
		OperationID: operationID, RequestKind: protocolsession.MessageReleaseLease,
		Cause: ProtocolOperationCauseDeadline,
	})
	if runtime.ctx.Err() != nil || runtime.operations.Terminated() {
		t.Fatal("protocol trace observer gained runtime authority")
	}
}
