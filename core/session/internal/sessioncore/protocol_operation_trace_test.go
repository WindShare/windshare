package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"testing/synctest"
	"time"
)

type protocolTraceRecorder struct {
	producer observationstream.Producer[sessionobs.ProtocolObservation]
	consumer observationstream.Consumer[sessionobs.ProtocolObservation]
	events   []sessionobs.ProtocolObservation
}

func newProtocolTraceRecorder(runtime *Runtime) *protocolTraceRecorder {
	producer, consumer, _ := observationstream.New[sessionobs.ProtocolObservation](256)
	recorder := &protocolTraceRecorder{producer: producer, consumer: consumer}
	runtime.protocolObservations = producer
	return recorder
}

func (recorder *protocolTraceRecorder) facts() []sessionobs.ProtocolObservation {
	for {
		select {
		case event := <-recorder.consumer:
			recorder.events = append(recorder.events, event)
		default:
			return append([]sessionobs.ProtocolObservation(nil), recorder.events...)
		}
	}
}

func (recorder *protocolTraceRecorder) snapshot() []sessionobs.ProtocolOperationObservation {
	var result []sessionobs.ProtocolOperationObservation
	for _, event := range recorder.facts() {
		if value, ok := event.(sessionobs.ProtocolOperationObservation); ok {
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
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x63}, protocolsession.IdentityBytes)))
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
	call, err := rpc.Begin(ctx, protocolsession.MessageReleaseLease, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rpc.Await(ctx, call); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("release lease wait error = %v", err)
	}
	_ = rpc.CancelAndEnd(call, contentflow.CancelReasonTimeout)

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("protocol trace events = %d, want 1: %+v", len(events), events)
	}
	event := events[0]
	if event.Stage != sessionobs.ProtocolOperationReceiverFailed ||
		event.Cause != sessionobs.ProtocolOperationCauseDeadline ||
		event.RequestKind != protocolsession.MessageReleaseLease ||
		event.OperationID != call.id || event.ProtocolSessionID != runtime.sessionID ||
		!event.HasLane || event.Lane != runtime.initial ||
		!event.HasSend || event.SendStatus != sessionobs.SendStatusConfirmed ||
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

	operationID := protocolsession.OperationID(0x71)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageReleaseLease, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	outbound := &Responses{runtime: runtime}
	responseBody, err := contentflow.EncodeOperationComplete(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.RegisterHandler(
		protocolsession.MessageReleaseLease,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			_, err := outbound.SendControl(ctx, op, protocolsession.MessageOperationComplete, responseBody)
			return err
		}),
	); err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("route release lease: disposition=%d error=%v", disposition, err)
	}
	queued, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("sender facts: %+v", facts)
	}
	received := facts[0].(sessionobs.ProtocolOperationObservation)
	responded := facts[1].(sessionobs.ResponseSendObserved)
	if received.OperationID != operationID || responded.Correlation() != received.Correlation() ||
		responded.ResponseKind() != protocolsession.MessageOperationComplete ||
		responded.Status() != sessionobs.SendStatusConfirmed ||
		responded.Lane() != runtime.initial || responded.Attempts() != 1 || responded.ResponseSequence() == 0 {
		t.Fatalf("request=%+v response=%+v", received, responded)
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

	operationID := protocolsession.OperationID(0x76)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageOpenRevisions,
		&operationID,
		[]byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	outbound := &Responses{runtime: runtime}
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
	if err := runtime.inbound.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			_, err := outbound.SendControl(ctx, op, protocolsession.MessageOperationError, failureBody)
			return err
		}),
	); err != nil {
		t.Fatal(err)
	}
	if disposition, routeErr := runtime.inbound.RouteInbound(runtime.initial, request); routeErr != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("route open revisions: disposition=%d error=%v", disposition, routeErr)
	}
	queued, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("sender facts: %+v", facts)
	}
	event := facts[1].(sessionobs.ResponseSendObserved)
	failure := event.Content()
	if event.Correlation().OperationID != operationID || event.ResponseKind() != protocolsession.MessageOperationError ||
		failure.WireScope() != sessionobs.ProtocolErrorRevision || failure.WireCode() != 0x3008 || !failure.Retryable() ||
		event.Status() != sessionobs.SendStatusConfirmed {
		t.Fatalf("fact=%+v", event)
	}
	if retry, present := failure.RetryAfterMillis(); !present || retry != 2000 {
		t.Fatalf("retry=%d/%v", retry, present)
	}
}

func TestProtocolOperationTraceSuppressesSuccessfulTransferHotPath(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	operationID := protocolsession.OperationID(0x78)

	for _, event := range []sessionobs.ProtocolOperationObservation{
		{
			Stage: sessionobs.ProtocolOperationReceiverCompleted, OperationID: operationID,
			RequestKind:  protocolsession.MessageRequestBlocks,
			ResponseKind: protocolsession.MessageOperationComplete, HasResponse: true,
			HasSend: true, SendStatus: sessionobs.SendStatusConfirmed,
		},
		{
			Stage: sessionobs.ProtocolOperationSenderRequestReceived, OperationID: operationID,
			RequestKind: protocolsession.MessageRequestBlocks,
		},
	} {
		runtime.traceProtocolOperation(event)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("successful hot-path protocol events were retained: %+v", events)
	}

	runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
		Stage: sessionobs.ProtocolOperationReceiverFailed, OperationID: operationID,
		RequestKind: protocolsession.MessageRequestBlocks,
		Cause:       sessionobs.ProtocolOperationCauseDeadline,
	})
	if events := recorder.snapshot(); len(events) != 1 || events[0].Cause != sessionobs.ProtocolOperationCauseDeadline {
		t.Fatalf("exceptional hot-path protocol event was lost: %+v", events)
	}
}

func TestProtocolOperationTraceCapturesAuthenticatedReceivedFailureAtSource(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	recorder := newProtocolTraceRecorder(runtime)
	receivedAt := time.Unix(1234, 0)
	runtime.now = func() time.Time { return receivedAt }
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x7d}, protocolsession.IdentityBytes)))
	if err := rpc.Register(); err != nil {
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

	call, err := rpc.Begin(context.Background(), protocolsession.MessageRequestBlocks, []byte{0xa0})
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
	message, err := protocolsession.NewMessage(
		protocolsession.MessageOperationError,
		&call.id,
		semantic,
	)
	if err != nil {
		t.Fatal(err)
	}
	disposition, err := runtime.inbound.RouteInbound(runtime.initial, message)
	if err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("route authenticated failure: disposition=%d error=%v", disposition, err)
	}
	queued, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.now = func() time.Time { return receivedAt.Add(time.Second) }
	if err := runtime.inbound.Dispatch(context.Background(), queued); err != nil {
		t.Fatal(err)
	}
	if response, err := rpc.Await(context.Background(), call); err != nil ||
		response.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("await authenticated failure: kind=%d error=%v", response.Kind(), err)
	}
	call.setProtocolTraceLane(session.LaneIdentity{ID: 9, Epoch: 4}, 1)
	rpc.End(call)

	facts := recorder.facts()
	if len(facts) != 2 {
		t.Fatalf("received facts=%+v", facts)
	}
	received := facts[0].(sessionobs.ReceivedProtocolError)
	failure := received.Content()
	if received.Lane() != runtime.initial || received.Correlation().OperationID != call.id ||
		failure.WireScope() != sessionobs.ProtocolErrorBlock || failure.WireCode() != 0x4003 || !failure.Retryable() ||
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
	operationID := protocolsession.OperationID(0x7a)
	lane := session.LaneIdentity{ID: 7, Epoch: 2}
	runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
		Stage: sessionobs.ProtocolOperationReceiverCompleted, OperationID: operationID,
		RequestKind: protocolsession.MessageLaneAttach, ResponseKind: protocolsession.MessageLaneAttach,
		HasResponse: true, Lane: lane, HasLane: true,
		HasSend: true, SendStatus: sessionobs.SendStatusConfirmed, ResponseCount: 1,
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
		event.SendStatus != sessionobs.SendStatusConfirmed {
		t.Fatalf("lane-grant trace = %+v", event)
	}
}

func TestProtocolObservationBlockedConsumerCannotChangeRuntimeAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	producer, _, _ := observationstream.New[sessionobs.ProtocolObservation](1)
	runtime.protocolObservations = producer
	for range 3 {
		runtime.traceProtocolOperation(sessionobs.ProtocolOperationObservation{
			Stage: sessionobs.ProtocolOperationReceiverFailed, OperationID: protocolsession.OperationID(0x79),
			RequestKind: protocolsession.MessageReleaseLease, Cause: sessionobs.ProtocolOperationCauseDeadline})
	}
	completion := producer.Complete()
	if completion.Enqueued != 1 || completion.CapacityDropped != 2 || runtime.ctx.Err() != nil || runtime.operations.Terminated() {
		t.Fatalf("completion=%+v", completion)
	}
}
