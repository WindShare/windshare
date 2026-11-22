package sessioncore

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRPCBeginPublishesAuthorityAfterFastResponseWasAlreadyDispatched(t *testing.T) {
	runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	rpc := NewRequests(runtime, nil)
	if err := rpc.Register(); err != nil {
		t.Fatal(err)
	}
	operationID := protocolsession.OperationID(1)
	response, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatched := make(chan struct{})
	var responseOnce sync.Once
	channel.onSend = func(framechannel.Frame) {
		responseOnce.Do(func() {
			disposition, routeErr := runtime.inbound.RouteInbound(runtime.initial, response)
			if routeErr != nil || disposition != protocolsession.OperationDeliver {
				t.Errorf("fast response admission: disposition=%d error=%v", disposition, routeErr)
				return
			}
			event, nextErr := runtime.inbound.Next(context.Background())
			if nextErr != nil {
				t.Errorf("fast response dequeue: %v", nextErr)
				return
			}
			if dispatchErr := runtime.inbound.Dispatch(context.Background(), event); dispatchErr != nil {
				t.Errorf("fast response dispatch: %v", dispatchErr)
				return
			}
			close(dispatched)
		})
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

	call, err := rpc.Begin(context.Background(), protocolsession.MessageListChildren, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	select {
	case <-dispatched:
	default:
		t.Fatal("request send returned before the synchronous response dispatch")
	}
	message, err := rpc.Await(context.Background(), call)
	if err != nil || message.Kind() != protocolsession.MessageCatalogResult {
		t.Fatalf("fast response result: kind=%d error=%v", message.Kind(), err)
	}
}

func TestRPCBeginCancellationAfterAdmissionQueuesOneExactCancel(t *testing.T) {
	runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	lane, _ := runtime.lanes.selectLane(&runtime.initial)
	firstSendStarted := make(chan struct{})
	releaseFirstSend := make(chan struct{})
	secondSend := make(chan struct{})
	var sends atomic.Int32
	channel.onSend = func(framechannel.Frame) {
		switch sends.Add(1) {
		case 1:
			close(firstSendStarted)
			<-releaseFirstSend
		case 2:
			close(secondSend)
		}
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	defer func() {
		select {
		case <-releaseFirstSend:
		default:
			close(releaseFirstSend)
		}
		stopWriter()
		<-writerDone
	}()
	rpc := NewRequests(runtime, nil)
	requestContext, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := rpc.Begin(requestContext, protocolsession.MessageListChildren, []byte{0xa0})
		result <- err
	}()
	<-firstSendStarted
	cancelRequest()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("admitted begin cancellation error=%v", err)
	}
	rpc.mu.Lock()
	calls := len(rpc.calls)
	rpc.mu.Unlock()
	if calls != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("admitted cancellation calls=%d active=%d tombstones=%d",
			calls, runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	close(releaseFirstSend)
	select {
	case <-secondSend:
	case <-time.After(time.Second):
		t.Fatal("admitted cancellation never reached the physical writer")
	}
	if sends.Load() != 2 {
		t.Fatalf("request/cancel physical sends=%d", sends.Load())
	}
}

func TestRPCFinalWinsCleanupPreservesFingerprintAndRejectsStaleReuse(t *testing.T) {
	synctest.Test(t, testRPCFinalWinsCleanupPreservesFingerprintAndRejectsStaleReuse)
}

func testRPCFinalWinsCleanupPreservesFingerprintAndRejectsStaleReuse(t *testing.T) {
	runtime, channel := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 2},
		nil,
	)
	lane, _ := runtime.lanes.selectLane(&runtime.initial)
	var physicalSends atomic.Int32
	channel.onSend = func(framechannel.Frame) { physicalSends.Add(1) }
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()

	rpc := NewRequests(runtime, nil)
	firstID := protocolsession.OperationID(1)
	request, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &firstID, []byte{0xa0})
	firstOp, maxCont, hasLimit, releaseFirst, err := runtime.operations.AdmitOutboundRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.writer.SendControl(context.Background(), request, releaseFirst); err != nil {
		t.Fatal(err)
	}
	call := &Call{
		id: firstID, messages: make(chan operationResponse, 1), lane: runtime.initial,
	}
	if !call.setAuthority(firstOp, maxCont, hasLimit) {
		t.Fatal("failed to bind first operation")
	}
	rpc.calls[firstID] = call
	final, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &firstID, []byte{0xa0})
	if _, err := runtime.operations.ObserveInbound(protocolsession.DirectionSenderToReceiver, runtime.initial, final); err != nil {
		t.Fatal(err)
	}
	if err := rpc.admitCancellation(call, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("final-wins cleanup: %v", err)
	}
	if physicalSends.Load() != 1 {
		t.Fatalf("final-wins cleanup emitted %d wire cancels", physicalSends.Load()-1)
	}
	if admission, err := runtime.operations.ObserveInbound(protocolsession.DirectionSenderToReceiver, runtime.initial, final); err != nil || admission.disposition != protocolsession.OperationDrop {
		t.Fatalf("exact final fingerprint changed: disposition=%d error=%v", admission.disposition, err)
	}
	conflictingBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	conflicting, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &firstID, conflictingBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.operations.ObserveInbound(protocolsession.DirectionSenderToReceiver, runtime.initial, conflicting); !errors.Is(err, protocolsession.ErrConflictingFinal) {
		t.Fatalf("conflicting final error=%v", err)
	}

	// Re-admitting the same operation ID is rejected outright.
	if _, _, _, _, err := runtime.operations.AdmitOutboundRequest(request, nil); !errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("same-ID outbound reuse error=%v, want ErrOperationIDReused", err)
	}

	secondID := protocolsession.OperationID(2)
	secondRequest, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &secondID, []byte{0xa0})
	secondOp, _, _, releaseSecond, err := runtime.operations.AdmitOutboundRequest(secondRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.writer.SendControl(context.Background(), secondRequest, releaseSecond); err != nil {
		t.Fatal(err)
	}
	if !secondOp.IsActive() {
		t.Fatal("second operation was not active")
	}
	if err := rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("stale operation cleanup: %v", err)
	}
	if !secondOp.IsActive() || runtime.operations.ActiveCount() != 1 || physicalSends.Load() != 2 {
		t.Fatalf("stale cleanup changed second operation active=%v count=%d sends=%d",
			secondOp.IsActive(), runtime.operations.ActiveCount(), physicalSends.Load())
	}
}

func TestRPCQueuedStaleResponseCannotAffectSubsequentOperation(t *testing.T) {
	synctest.Test(t, testRPCQueuedStaleResponseCannotAffectSubsequentOperation)
}

func testRPCQueuedStaleResponseCannotAffectSubsequentOperation(t *testing.T) {
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxRecentTombstones: 1},
		nil,
	)
	t.Cleanup(runtime.AbortBeforeStart)
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
	rpc := NewRequests(runtime, nil)
	if err := rpc.Register(); err != nil {
		t.Fatal(err)
	}
	beginOperation := func(id protocolsession.OperationID) (*Call, protocolsession.Operation) {
		request, err := protocolsession.NewMessage(protocolsession.MessageListChildren, &id, []byte{0xa0})
		if err != nil {
			t.Fatal(err)
		}
		op, maxCont, hasLimit, release, admitErr := runtime.operations.AdmitOutboundRequest(request, nil)
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		if sendErr := lane.writer.SendControl(context.Background(), request, release); sendErr != nil {
			t.Fatal(sendErr)
		}
		call := &Call{
			id: id, messages: make(chan operationResponse, 1), lane: runtime.initial,
		}
		if !call.setAuthority(op, maxCont, hasLimit) {
			t.Fatal("failed to bind operation")
		}
		rpc.calls[id] = call
		return call, op
	}

	idA := protocolsession.OperationID(1)
	callA, opA := beginOperation(idA)
	responseA, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &idA, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, responseA); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue operation A response: disposition=%d error=%v", disposition, err)
	}
	if opA.IsActive() || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("operation A final lifecycle active=%v activeCount=%d tombstones=%d",
			opA.IsActive(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	if admission, err := runtime.operations.ObserveInbound(
		protocolsession.DirectionSenderToReceiver, runtime.initial, responseA,
	); err != nil || admission.disposition != protocolsession.OperationDrop {
		t.Fatalf("operation A exact tombstone: disposition=%d error=%v", admission.disposition, err)
	}

	synctest.Wait()
	idB := protocolsession.OperationID(2)
	callB, opB := beginOperation(idB)
	if opA.Same(opB) || !opB.IsActive() {
		t.Fatalf("operation B authority A=%d B=%d", opA.ID(), opB.ID())
	}
	if err := rpc.CancelAndEnd(callA, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("operation A cleanup after B admission: %v", err)
	}
	select {
	case <-callA.doneChannel():
	default:
		t.Fatal("operation A cleanup did not close its exact response sink")
	}
	if !callA.Operation().IsZero() {
		t.Fatal("operation A cleanup retained operation authority")
	}
	rpc.mu.Lock()
	retainedB := rpc.calls[idB] == callB
	rpc.mu.Unlock()
	if !retainedB || !opB.IsActive() {
		t.Fatalf("operation A cleanup changed B: retained=%v active=%v", retainedB, opB.IsActive())
	}

	staleEvent, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.Dispatch(context.Background(), staleEvent); err != nil {
		t.Fatal(err)
	}
	select {
	case stale := <-callB.messages:
		t.Fatalf("operation A response crossed into B: kind=%d", stale.message.Kind())
	default:
	}
	rpc.mu.Lock()
	retainedB = rpc.calls[idB] == callB
	rpc.mu.Unlock()
	if !retainedB || !opB.IsActive() {
		t.Fatalf("stale dispatch changed B: retained=%v active=%v", retainedB, opB.IsActive())
	}

	bodyB, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(2)})
	if err != nil {
		t.Fatal(err)
	}
	responseB, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &idB, bodyB,
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, responseB); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue operation B response: disposition=%d error=%v", disposition, err)
	}
	eventB, err := runtime.inbound.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.inbound.Dispatch(context.Background(), eventB); err != nil {
		t.Fatal(err)
	}
	received, err := rpc.Await(context.Background(), callB)
	if err != nil || !bytes.Equal(received.Body(), bodyB) {
		t.Fatalf("operation B response body=%x error=%v", received.Body(), err)
	}
	rpc.End(callB)
	rpc.mu.Lock()
	remainingCalls := len(rpc.calls)
	rpc.mu.Unlock()
	if remainingCalls != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("operation B cleanup calls=%d active=%d tombstones=%d",
			remainingCalls, runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestOperationIDSourceIssuesConcurrentMonotonicIDs(t *testing.T) {
	source := &operationIDSource{}
	const count = 256
	identities := make(chan protocolsession.OperationID, count)
	errorsSeen := make(chan error, count)
	var wait sync.WaitGroup
	for range count {
		wait.Go(func() {
			identity, err := source.New()
			if err != nil {
				errorsSeen <- err
				return
			}
			identities <- identity
		})
	}
	wait.Wait()
	close(identities)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent identity: %v", err)
	}
	seen := make(map[protocolsession.OperationID]struct{}, count)
	for identity := range identities {
		if identity < 1 || identity > count {
			t.Fatalf("identity %d outside monotonic range 1..%d", identity, count)
		}
		if _, duplicate := seen[identity]; duplicate {
			t.Fatalf("duplicate identity=%d", identity)
		}
		seen[identity] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("issued identities=%d", len(seen))
	}
}

func TestOperationIDSourceFailsAtCounterExhaustion(t *testing.T) {
	source := &operationIDSource{counter: ^uint64(0) - 1}
	identity, err := source.New()
	if err != nil || identity != protocolsession.OperationID(^uint64(0)) {
		t.Fatalf("last identity=%d error=%v", identity, err)
	}
	if _, err := source.New(); err != ErrOperationIDExhausted {
		t.Fatalf("post-exhaustion error=%v", err)
	}
}
