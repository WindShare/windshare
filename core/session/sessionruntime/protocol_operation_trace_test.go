package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type protocolTraceRecorder struct {
	producer observationstream.Producer[ProtocolObservation]
	consumer observationstream.Consumer[ProtocolObservation]
	events   []ProtocolObservation
}

func newProtocolTraceRecorder(runtime *runtimeCore) *protocolTraceRecorder {
	producer, consumer, _ := observationstream.New[ProtocolObservation](256)
	recorder := &protocolTraceRecorder{producer: producer, consumer: consumer}
	runtime.protocolObservations = producer
	return recorder
}
func (recorder *protocolTraceRecorder) facts() []ProtocolObservation {
	for {
		select {
		case event := <-recorder.consumer:
			recorder.events = append(recorder.events, event)
		default:
			return append([]ProtocolObservation(nil), recorder.events...)
		}
	}
}
func (recorder *protocolTraceRecorder) snapshot() []ProtocolOperationObservation {
	var result []ProtocolOperationObservation
	for _, event := range recorder.facts() {
		if value, ok := event.(ProtocolOperationObservation); ok {
			result = append(result, value)
		}
	}
	return result
}

func TestProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline(t *testing.T) {
	synctest.Test(t, testProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline)
}

func testProtocolOperationTraceExplainsDeliveredReleaseLeaseDeadline(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
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
		event.SendOutcome != protocolsession.SendOutcomeTransportConfirmed ||
		event.HasResponse || event.ResponseCount != 0 ||
		!event.HasDeadline || event.DeadlineRemainingMillis != 30_000 ||
		event.OperationElapsedMillis != 30_000 ||
		event.UsableLanesAtSelection != 1 || event.UsableLanesAtSettlement != 1 {
		t.Fatalf("release lease protocol trace = %+v", event)
	}
}

func TestSenderProtocolOperationTraceCorrelatesRequestAndResponse(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
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

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("sender facts: %+v", facts)
	}
	received := facts[0].(ProtocolOperationObservation)
	responded := facts[1].(ResponseSendReturned)
	if received.OperationID != operationID || responded.Correlation() != received.Correlation() ||
		responded.ResponseKind() != protocolsession.MessageOperationComplete || responded.Result().Evidence() != protocolsession.ResponseSendEvidenceTransportConfirmed {
		t.Fatalf("request=%+v response=%+v", received, responded)
	}
	attempt, ok := responded.Result().Attempt(0)
	if !ok || attempt.Identity().LaneID != runtime.initial.ID || attempt.Identity().ResponseSequence != responded.ResponseSequence() {
		t.Fatalf("attempt=%+v", attempt)
	}

}

func TestSenderProtocolOperationTraceCapturesFailureSendSettlement(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	recorder := newProtocolTraceRecorder(runtime)
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

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("sender facts: %+v", facts)
	}
	event := facts[1].(ResponseSendReturned)
	failure := event.Content()
	if event.Correlation().OperationID != operationID || event.ResponseKind() != protocolsession.MessageOperationError ||
		failure.WireScope() != ProtocolErrorRevision || failure.WireCode() != 0x3008 || !failure.Retryable() ||
		event.Result().Evidence() != protocolsession.ResponseSendEvidenceTransportConfirmed {
		t.Fatalf("fact=%+v", event)
	}
	if retry, present := failure.RetryAfterMillis(); !present || retry != 2000 {
		t.Fatalf("retry=%d/%v", retry, present)
	}

}

func TestProtocolOperationTraceSuppressesSuccessfulTransferHotPath(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	operationID := id16[protocolsession.OperationID](0x78)

	for _, event := range []ProtocolOperationObservation{
		{
			Stage: ProtocolOperationReceiverCompleted, OperationID: operationID,
			RequestKind:  protocolsession.MessageRequestBlocks,
			ResponseKind: protocolsession.MessageOperationComplete, HasResponse: true,
			HasSend: true, SendSettled: true, SendAdmitted: true,
			SendOutcome: protocolsession.SendOutcomeTransportConfirmed,
		},
		{
			Stage: ProtocolOperationSenderRequestReceived, OperationID: operationID,
			RequestKind: protocolsession.MessageRequestBlocks,
		},
	} {
		runtime.traceProtocolOperation(event)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("successful hot-path protocol events were retained: %+v", events)
	}

	runtime.traceProtocolOperation(ProtocolOperationObservation{
		Stage: ProtocolOperationReceiverFailed, OperationID: operationID,
		RequestKind: protocolsession.MessageRequestBlocks,
		Cause:       ProtocolOperationCauseDeadline,
	})
	if events := recorder.snapshot(); len(events) != 1 || events[0].Cause != ProtocolOperationCauseDeadline {
		t.Fatalf("exceptional hot-path protocol event was lost: %+v", events)
	}
}

func TestProtocolOperationTraceCapturesAuthenticatedReceivedFailureAtSource(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	receivedAt := time.Unix(1234, 0)
	runtime.now = func() time.Time { return receivedAt }
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
	runtime.now = func() time.Time { return receivedAt.Add(time.Second) }
	if err := runtime.router.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if response, err := rpc.await(context.Background(), call); err != nil ||
		response.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("await authenticated failure: kind=%d error=%v", response.Kind(), err)
	}
	call.setProtocolTraceLane(LaneIdentity{ID: 9, Epoch: 4}, 1)
	rpc.end(call)

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("received facts=%+v", facts)
	}
	received := facts[0].(ReceivedProtocolError)
	failure := received.Content()
	if received.Lane() != runtime.initial || received.Correlation().OperationID != call.id ||
		failure.WireScope() != ProtocolErrorBlock || failure.WireCode() != 0x4003 || !failure.Retryable() ||
		!received.ObservedAt().Equal(receivedAt) {
		t.Fatalf("received=%+v", received)
	}
	if retry, present := failure.RetryAfterMillis(); !present || retry != 1250 {
		t.Fatalf("retry=%d/%v", retry, present)
	}

}

func TestProtocolOperationTraceCorrelatesLaneGrantWithoutAuthenticatedBody(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	operationID := id16[protocolsession.OperationID](0x7a)
	lane := LaneIdentity{ID: 7, Epoch: 2}
	runtime.traceProtocolOperation(ProtocolOperationObservation{
		Stage: ProtocolOperationReceiverCompleted, OperationID: operationID,
		RequestKind: protocolsession.MessageLaneAttach, ResponseKind: protocolsession.MessageLaneAttach,
		HasResponse: true, Lane: lane, HasLane: true,
		HasSend: true, SendSettled: true, SendAdmitted: true,
		SendOutcome: protocolsession.SendOutcomeTransportConfirmed, ResponseCount: 1,
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
		event.SendOutcome != protocolsession.SendOutcomeTransportConfirmed {
		t.Fatalf("lane-grant trace = %+v", event)
	}
}

func TestProtocolObservationBlockedConsumerCannotChangeRuntimeAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	producer, _, _ := observationstream.New[ProtocolObservation](1)
	runtime.protocolObservations = producer
	for range 3 {
		runtime.traceProtocolOperation(ProtocolOperationObservation{
			Stage: ProtocolOperationReceiverFailed, OperationID: id16[protocolsession.OperationID](0x79),
			RequestKind: protocolsession.MessageReleaseLease, Cause: ProtocolOperationCauseDeadline})
	}
	completion := producer.Complete()
	if completion.Enqueued != 1 || completion.CapacityDropped != 2 || runtime.ctx.Err() != nil || runtime.operations.Terminated() {
		t.Fatalf("completion=%+v", completion)
	}
}
