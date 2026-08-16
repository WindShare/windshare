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
