package sessioncore

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestConcurrentSameIDRequestsAdmitExactlyOneRouteAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	handlerOps := make(chan protocolsession.Operation, 1)
	if err := runtime.inbound.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(_ context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			handlerOps <- op
			return nil
		}),
	); err != nil {
		t.Fatal(err)
	}
	secondBase, secondPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = secondPeer.Close() })
	secondIdentity := session.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if _, err := runtime.lanes.add(
		secondIdentity, secondBase, permissiveInboundAuthenticator(), false,
	); err != nil {
		t.Fatal(err)
	}
	operationID := protocolsession.OperationID(121)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	request, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, body)
	type result struct {
		disposition protocolsession.OperationDisposition
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, identity := range []session.LaneIdentity{runtime.initial, secondIdentity} {
		wait.Go(func() {
			<-start
			disposition, err := runtime.inbound.RouteInbound(identity, request)
			results <- result{disposition: disposition, err: err}
		})
	}
	close(start)
	wait.Wait()
	close(results)
	delivered, replayed := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.disposition == protocolsession.OperationDeliver:
			delivered++
		case result.err == nil && result.disposition == protocolsession.OperationDrop:
			replayed++
		default:
			t.Fatalf("same-ID admission = disposition %d, error %v", result.disposition, result.err)
		}
	}
	if delivered != 1 || replayed != 1 || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("delivered=%d replayed=%d active=%d",
			delivered, replayed, runtime.operations.ActiveCount())
	}
	op := dispatchTestOperation(t, runtime, handlerOps)
	rec := runtime.operations.lookupRecord(op.ID())
	if rec == nil || !rec.preferredLane.Valid(true) {
		t.Fatal("winning request has no preferred lane authority")
	}
	winningLane := rec.preferredLane
	conflictingBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(2)})
	conflicting, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, conflictingBody)
	if disposition, err := runtime.inbound.RouteInbound(secondIdentity, conflicting); disposition != protocolsession.OperationDrop ||
		!errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("conflicting same-ID request = disposition %d, error %v", disposition, err)
	}
	if rec.preferredLane != winningLane {
		t.Fatal("conflicting replay replaced the first physical lane affinity")
	}
	lane, err := runtime.lanes.selectLane(&winningLane)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	outbound := &Responses{runtime: runtime}
	if _, err := outbound.SendControl(
		context.Background(), op, protocolsession.MessageOpenResults, body,
	); err != nil {
		t.Fatalf("winning authority response = %v", err)
	}
	stopWriter()
	<-writerDone
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("final drain active=%d tombstones=%d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestHostileSameIDReplayRejectedEvenAfterTombstoneEviction(t *testing.T) {
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxRecentTombstones: 2}, nil,
	)
	handlerOps := make(chan protocolsession.Operation, 4)
	if err := runtime.inbound.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(_ context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			handlerOps <- op
			return nil
		}),
	); err != nil {
		t.Fatal(err)
	}
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
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	outbound := &Responses{runtime: runtime}
	admitAndComplete := func(id protocolsession.OperationID) protocolsession.Operation {
		req, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &id, body)
		if disposition, err := runtime.inbound.RouteInbound(runtime.initial, req); err != nil ||
			disposition != protocolsession.OperationDeliver {
			t.Fatalf("request %d = %d, %v", id, disposition, err)
		}
		op := dispatchTestOperation(t, runtime, handlerOps)
		if _, err := outbound.SendControl(context.Background(), op, protocolsession.MessageOpenResults, body); err != nil {
			t.Fatalf("final %d = %v", id, err)
		}
		return op
	}
	oldID := protocolsession.OperationID(122)
	oldOp := admitAndComplete(oldID)
	admitAndComplete(123)
	admitAndComplete(124)

	// Tombstone 122 was evicted by FIFO cap (MaxRecentTombstones=2), yet replay window still rejects reuse.
	replayReq, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &oldID, body)
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, replayReq); disposition != protocolsession.OperationDrop ||
		!errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("evicted replay = (%d, %v), want ErrOperationIDReused", disposition, err)
	}

	nextID := protocolsession.OperationID(125)
	nextReq, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &nextID, body)
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, nextReq); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("next request = %d, %v", disposition, err)
	}
	newOp := dispatchTestOperation(t, runtime, handlerOps)
	if err := outbound.SendOperationError(context.Background(), oldOp, contentflow.OperationFailure{
		Scope:   contentflow.RevisionErrorScope,
		Code:    contentflow.RevisionCodeUnreadable,
		Message: "stale handler failure",
	}); !errors.Is(err, protocolsession.ErrOperationSuppressed) {
		t.Fatalf("delayed old operation error = %v", err)
	}
	if !newOp.IsActive() || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("old op affected active operation: active=%v count=%d",
			newOp.IsActive(), runtime.operations.ActiveCount())
	}
	if _, err := outbound.SendControl(context.Background(), newOp, protocolsession.MessageOpenResults, body); err != nil {
		t.Fatalf("new operation final = %v", err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 2 {
		t.Fatalf("final drain active=%d tombstones=%d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func dispatchTestOperation(
	t *testing.T,
	runtime *Runtime,
	ops <-chan protocolsession.Operation,
) protocolsession.Operation {
	t.Helper()
	event, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.Dispatch(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	return <-ops
}
