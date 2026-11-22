package sessioncore

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
	operationID := id16[protocolsession.OperationID](121)
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
	rec := runtime.operations.lookupRecord(op.Ref())
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
	if err := outbound.SendControl(
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

func TestForcedHostileSameIDReuseRejectsDelayedOldRouteContext(t *testing.T) {
	synctest.Test(t, testForcedHostileSameIDReuseRejectsDelayedOldRouteContext)
}

func testForcedHostileSameIDReuseRejectsDelayedOldRouteContext(t *testing.T) {
	// Honest receiver issuers never reuse an OperationID within a ProtocolSession.
	// This forced post-retention collision exercises local ABA containment only.
	var nanos atomic.Int64
	nanos.Store(time.Unix(1_800_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxTracked: 4}, now,
	)
	handlerOps := make(chan protocolsession.Operation, 2)
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
	operationID := id16[protocolsession.OperationID](122)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	request, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, body)
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("first request = %d, %v", disposition, err)
	}
	oldOp := dispatchTestOperation(t, runtime, handlerOps)
	outbound := &Responses{runtime: runtime}
	if err := outbound.SendControl(
		context.Background(), oldOp, protocolsession.MessageOpenResults, body,
	); err != nil {
		t.Fatalf("first final = %v", err)
	}
	synctest.Wait()
	nanos.Add((protocolsession.OperationTombstoneLifetime + time.Nanosecond).Nanoseconds())
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("forced colliding request = %d, %v", disposition, err)
	}
	newOp := dispatchTestOperation(t, runtime, handlerOps)
	if oldOp.Same(newOp) {
		t.Fatal("forced same-ID collision did not receive distinct operation token")
	}
	if err := outbound.SendOperationError(context.Background(), oldOp, contentflow.OperationFailure{
		Scope:   contentflow.RevisionErrorScope,
		Code:    contentflow.RevisionCodeUnreadable,
		Message: "stale handler failure",
	}); !errors.Is(err, protocolsession.ErrOperationSuppressed) {
		t.Fatalf("delayed old generation error = %v", err)
	}
	if !newOp.IsActive() || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("old op changed forced colliding generation: active=%v count=%d",
			newOp.IsActive(), runtime.operations.ActiveCount())
	}
	if err := outbound.SendControl(
		context.Background(), newOp, protocolsession.MessageOpenResults, body,
	); err != nil {
		t.Fatalf("forced colliding generation final = %v", err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("forced colliding generation drain active=%d tombstones=%d",
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
