package sessionruntime

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverPeerOperationClosedStateIsInert(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{1}, protocolsession.IdentityBytes)))
	receiver := &ReceiverRuntime{runtimeCore: runtime, rpc: rpc}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.OpenPeerOperation(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled peer open error = %v", err)
	}

	call := &operationCall{id: id16[protocolsession.OperationID](78), messages: make(chan operationResponse, 1)}
	closed := &ReceiverPeerOperation{
		rpc: rpc, call: call, token: new(receiverPeerOperationToken), closed: true,
	}
	if closed.OperationID() != call.id {
		t.Fatal("closed peer operation changed its exact identity")
	}
	if _, err := closed.SendCandidate(context.Background(), nil); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("closed peer candidate error = %v", err)
	}
	result := closed.Receive(context.Background())
	received := requireReceiverPeerTermination(t, result)
	assertReceiverPeerTermination(
		t,
		closed,
		received,
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalOperationContract,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalOperationContract,
		ReceiverPeerDiagnosticOperationMissing,
	)
	termination := closed.Terminate(context.Background())
	assertReceiverPeerTerminationsEqual(t, received, termination)
}

func TestPeerContinuationMigratesOnlyAfterOriginalLaneLoss(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{3}, protocolsession.IdentityBytes)))
	call := &operationCall{
		id: id16[protocolsession.OperationID](80), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	rpc.calls[call.id] = call
	if selected, err := rpc.continuationLane(call); err != nil || selected.identity != runtime.initial {
		t.Fatalf("live original continuation lane = %+v, %v", selected, err)
	}
	other := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if _, err := runtime.lanes.add(
		other, newMemoryChannel(t), permissiveInboundAuthenticator(), false,
	); err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if selected, err := rpc.continuationLane(call); err != nil || selected.identity != other || call.lane != other {
		t.Fatalf("migrated continuation lane = %+v, call=%+v, error=%v", selected, call.lane, err)
	}

	if _, err := rpc.sendContinuation(
		context.Background(), nil, protocolsession.MessagePeerCandidate, nil,
	); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("nil continuation call error = %v", err)
	}
	inactive := &operationCall{id: id16[protocolsession.OperationID](81)}
	if _, err := rpc.sendContinuation(
		context.Background(), inactive, protocolsession.MessagePeerCandidate, nil,
	); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("inactive continuation call error = %v", err)
	}
	if _, err := rpc.sendContinuation(
		context.Background(), call, protocolsession.MessageKind(0), nil,
	); err == nil {
		t.Fatal("continuation accepted an unknown message kind")
	}
}

func TestPeerContinuationFailsWhenEveryAuthenticatedLaneIsGone(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{4}, protocolsession.IdentityBytes)))
	call := &operationCall{
		id: id16[protocolsession.OperationID](82), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	rpc.calls[call.id] = call
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if _, err := rpc.continuationLane(call); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("all-lane-loss continuation error = %v", err)
	}
	if _, err := rpc.sendContinuation(
		context.Background(), call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("all-lane-loss send error = %v", err)
	}
}

func TestRPCBeginRollsBackEveryPreDeliveryFailure(t *testing.T) {
	canonicalBody := []byte{0xf6}
	t.Run("identity source", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		rpc := newRPCClient(runtime, bytes.NewReader(nil))
		if _, err := rpc.begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); err == nil {
			t.Fatal("peer operation accepted an exhausted identity source")
		}
	})
	t.Run("message schema", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{5}, protocolsession.IdentityBytes)))
		if _, err := rpc.begin(context.Background(), protocolsession.MessageKind(0), canonicalBody); err == nil {
			t.Fatal("RPC admitted an unknown request kind")
		}
	})
	t.Run("lane selection", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		runtime.lanes.mu.Lock()
		runtime.lanes.active[runtime.initial.ID].closing = true
		runtime.lanes.mu.Unlock()
		rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{6}, protocolsession.IdentityBytes)))
		if _, err := rpc.begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); !errors.Is(err, ErrLaneUnavailable) {
			t.Fatalf("no-lane RPC error = %v", err)
		}
		if len(rpc.calls) != 0 {
			t.Fatal("no-lane RPC retained its response sink")
		}
	})
	t.Run("writer shutdown", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		lane, err := runtime.lanes.selectLane(&runtime.initial)
		if err != nil {
			t.Fatal(err)
		}
		stopped, stop := context.WithCancel(context.Background())
		stop()
		_ = lane.writer.Run(stopped)
		rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{7}, protocolsession.IdentityBytes)))
		if _, err := rpc.begin(context.Background(), protocolsession.MessagePeerOffer, canonicalBody); err == nil {
			t.Fatal("RPC admitted work after its physical writer stopped")
		}
		if len(rpc.calls) != 0 {
			t.Fatal("writer-rejected RPC retained its response sink")
		}
	})
	t.Run("pre-transport timeout", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{8}, protocolsession.IdentityBytes)))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if _, err := rpc.begin(ctx, protocolsession.MessagePeerOffer, canonicalBody); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pre-transport timeout error = %v", err)
		}
		if len(rpc.calls) != 0 {
			t.Fatal("timed-out RPC retained its response sink")
		}
	})
}

func TestRPCContinuationRejectsStoppedPhysicalWriter(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	_ = lane.writer.Run(stopped)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{9}, protocolsession.IdentityBytes)))
	call := &operationCall{
		id: id16[protocolsession.OperationID](83), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	rpc.calls[call.id] = call
	if _, err := rpc.sendContinuation(
		context.Background(), call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	); err == nil {
		t.Fatal("candidate continuation bypassed a stopped physical writer")
	}
}

func TestRPCContinuationRetractsPendingPreTransportTimeout(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{10}, protocolsession.IdentityBytes)))
	call := &operationCall{
		id: id16[protocolsession.OperationID](84), messages: make(chan operationResponse, 1),
		lane: runtime.initial,
	}
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &call.id, []byte{0xf6})
	admission, err := runtime.router.AdmitOutbound(offer, protocolsession.OutboundOperationPermit{})
	if err != nil || !call.setAuthority(admission.Generation, admission.Operation) {
		t.Fatalf("seed continuation authority: %v", err)
	}
	rpc.calls[call.id] = call
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if outcome, err := rpc.sendContinuation(
		ctx, call, protocolsession.MessagePeerCandidate, []byte{0xf6},
	); outcome != protocolsession.SendOutcomeDropped || !errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("stalled continuation = outcome %d, error %v", outcome, err)
	}
}

func TestRPCAdmitCancellationRejectsInvalidAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{11}, protocolsession.IdentityBytes)))
	for _, call := range []*operationCall{nil, {}, {id: id16[protocolsession.OperationID](85)}} {
		if err := rpc.admitCancellation(call, contentflow.CancelReasonTimeout); !errors.Is(err, ErrOperationMissing) {
			t.Fatalf("invalid cancellation authority error = %v", err)
		}
	}
}

func TestReceiverPeerCancelReconcilesTombstonedWireSuppressionLocally(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	t.Cleanup(runtime.abortBeforeStart)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	t.Cleanup(func() {
		stopWriter()
		<-writerDone
	})

	operationID := id16[protocolsession.OperationID](86)
	offer, err := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &operationID, []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	offerAdmission, err := runtime.router.AdmitOutbound(offer, protocolsession.OutboundOperationPermit{})
	if err != nil {
		t.Fatal(err)
	}
	cancelBody, err := contentflow.EncodeCancelReason(contentflow.CancelReasonSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	cancelMessage, err := protocolsession.NewMessage(
		protocolsession.MessageCancel, &operationID, cancelBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.router.AdmitOutbound(cancelMessage, offerAdmission.Operation); err != nil {
		t.Fatal(err)
	}

	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{12}, protocolsession.IdentityBytes)))
	call := &operationCall{id: operationID, messages: make(chan operationResponse, 1), lane: runtime.initial}
	if !call.setAuthority(offerAdmission.Generation, offerAdmission.Operation) {
		t.Fatal("failed to seed peer operation authority")
	}
	rpc.calls[operationID] = call
	operation := &ReceiverPeerOperation{rpc: rpc, call: call, token: new(receiverPeerOperationToken)}
	assertReceiverPeerTermination(
		t,
		operation,
		operation.Terminate(context.Background()),
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
	)
	if _, active := rpc.calls[operationID]; active {
		t.Fatal("tombstoned peer cancel retained its RPC sink")
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("peer cancel reconciliation = active %d, tombstones %d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestDroppedPeerCancelRetiresLocallyActiveOperation(t *testing.T) {
	operations, err := protocolsession.NewOperationTable(
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	router, err := protocolsession.NewRoleRouter(protocolsession.RoleReceiver, operations)
	if err != nil {
		t.Fatal(err)
	}
	operationID := id16[protocolsession.OperationID](77)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	offer, _ := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &operationID, body)
	admission, err := router.AdmitOutbound(offer, protocolsession.OutboundOperationPermit{})
	if err != nil || operations.ActiveCount() != 1 {
		t.Fatalf("begin peer operation: active=%d, error=%v", operations.ActiveCount(), err)
	}
	if err := finalizeLocalCancelIfDropped(
		&runtimeCore{operations: operations, router: router, ctx: context.Background()},
		admission.Generation, protocolsession.SendOutcomeDropped,
	); err != nil {
		t.Fatalf("retire dropped cancel: %v", err)
	}
	if operations.ActiveCount() != 0 || operations.TombstoneCount() != 1 {
		t.Fatalf("dropped cancel lifecycle = active %d, tombstones %d",
			operations.ActiveCount(), operations.TombstoneCount())
	}
}

type inertSenderPeerHandler struct{}

func (inertSenderPeerHandler) HandleMessage(context.Context, protocolsession.Message) error {
	return nil
}

func (inertSenderPeerHandler) Cancel(context.Context, protocolsession.OperationID) error {
	return nil
}

func (inertSenderPeerHandler) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func inertSenderPeerFactory() SenderPeerHandlerFactory {
	return SenderPeerHandlerFactoryFunc(func(SenderPeerSession) (SenderPeerHandler, error) {
		return inertSenderPeerHandler{}, nil
	})
}
