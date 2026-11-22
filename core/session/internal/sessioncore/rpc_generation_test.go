package sessioncore

import (
	"bytes"
	"context"
	"encoding/binary"
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
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x73}, protocolsession.IdentityBytes)))
	if err := rpc.Register(); err != nil {
		t.Fatal(err)
	}
	operationID := issuedOperationID(0x73, 1)
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
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x83}, 8)))
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

func TestRPCFinalWinsCleanupPreservesFingerprintAndCannotCancelHostileSameIDGeneration(t *testing.T) {
	synctest.Test(t, testRPCFinalWinsCleanupPreservesFingerprintAndCannotCancelHostileSameIDGeneration)
}

func testRPCFinalWinsCleanupPreservesFingerprintAndCannotCancelHostileSameIDGeneration(t *testing.T) {
	now := time.Unix(10_000, 0)
	runtime, channel := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2},
		func() time.Time { return now },
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

	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x84}, 8)))
	operationID := issuedOperationID(0x84, 1)
	request, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &operationID, []byte{0xa0})
	firstOp, maxCont, hasLimit, releaseFirst, err := runtime.operations.AdmitOutboundRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.writer.SendControl(context.Background(), request, releaseFirst); err != nil {
		t.Fatal(err)
	}
	call := &Call{
		id: operationID, messages: make(chan operationResponse, 1), lane: runtime.initial,
	}
	if !call.setAuthority(firstOp, maxCont, hasLimit) {
		t.Fatal("failed to bind first generation")
	}
	rpc.calls[operationID] = call
	final, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &operationID, []byte{0xa0})
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
		protocolsession.MessageCatalogResult, &operationID, conflictingBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.operations.ObserveInbound(protocolsession.DirectionSenderToReceiver, runtime.initial, conflicting); !errors.Is(err, protocolsession.ErrConflictingFinal) {
		t.Fatalf("conflicting final error=%v", err)
	}

	synctest.Wait()
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	_ = runtime.operations.TombstoneCount()
	secondOp, _, _, releaseSecond, err := runtime.operations.AdmitOutboundRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.writer.SendControl(context.Background(), request, releaseSecond); err != nil {
		t.Fatal(err)
	}
	if !secondOp.IsActive() {
		t.Fatal("forced second generation was not active")
	}
	if err := rpc.CancelAndEnd(call, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("stale generation cleanup: %v", err)
	}
	if !secondOp.IsActive() || runtime.operations.ActiveCount() != 1 || physicalSends.Load() != 2 {
		t.Fatalf("stale cleanup changed second generation active=%v count=%d sends=%d",
			secondOp.IsActive(), runtime.operations.ActiveCount(), physicalSends.Load())
	}
}

func TestRPCQueuedStaleResponseCannotCrossSameIDGeneration(t *testing.T) {
	synctest.Test(t, testRPCQueuedStaleResponseCannotCrossSameIDGeneration)
}

func testRPCQueuedStaleResponseCannotCrossSameIDGeneration(t *testing.T) {
	now := time.Unix(20_000, 0)
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTracked: 2},
		func() time.Time { return now },
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
	rpc := NewRequests(runtime, bytes.NewReader(bytes.Repeat([]byte{0x85}, 8)))
	if err := rpc.Register(); err != nil {
		t.Fatal(err)
	}
	operationID := issuedOperationID(0x85, 1)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	beginGeneration := func() (*Call, protocolsession.Operation) {
		op, maxCont, hasLimit, release, admitErr := runtime.operations.AdmitOutboundRequest(request, nil)
		if admitErr != nil {
			t.Fatal(admitErr)
		}
		if sendErr := lane.writer.SendControl(context.Background(), request, release); sendErr != nil {
			t.Fatal(sendErr)
		}
		call := &Call{
			id: operationID, messages: make(chan operationResponse, 1), lane: runtime.initial,
		}
		if !call.setAuthority(op, maxCont, hasLimit) {
			t.Fatal("failed to bind same-ID generation")
		}
		return call, op
	}

	callA, opA := beginGeneration()
	rpc.calls[operationID] = callA
	responseA, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, responseA); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue generation A response: disposition=%d error=%v", disposition, err)
	}
	if opA.IsActive() || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("generation A final lifecycle active=%v activeCount=%d tombstones=%d",
			opA.IsActive(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	if admission, err := runtime.operations.ObserveInbound(
		protocolsession.DirectionSenderToReceiver, runtime.initial, responseA,
	); err != nil || admission.disposition != protocolsession.OperationDrop {
		t.Fatalf("generation A exact tombstone: disposition=%d error=%v", admission.disposition, err)
	}

	synctest.Wait()
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	_ = runtime.operations.TombstoneCount()
	callB, opB := beginGeneration()
	if opA.Same(opB) || !opB.IsActive() {
		t.Fatalf("generation B authority A=%+v B=%+v", opA.Ref(), opB.Ref())
	}
	rpc.calls[operationID] = callB
	if err := rpc.CancelAndEnd(callA, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("generation A cleanup after B admission: %v", err)
	}
	select {
	case <-callA.doneChannel():
	default:
		t.Fatal("generation A cleanup did not close its exact response sink")
	}
	if !callA.Operation().IsZero() {
		t.Fatal("generation A cleanup retained operation authority")
	}
	rpc.mu.Lock()
	retainedB := rpc.calls[operationID] == callB
	rpc.mu.Unlock()
	if !retainedB || !opB.IsActive() {
		t.Fatalf("generation A cleanup changed B: retained=%v active=%v", retainedB, opB.IsActive())
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
		t.Fatalf("generation A response crossed into B: kind=%d", stale.message.Kind())
	default:
	}
	rpc.mu.Lock()
	retainedB = rpc.calls[operationID] == callB
	rpc.mu.Unlock()
	if !retainedB || !opB.IsActive() {
		t.Fatalf("stale dispatch changed B: retained=%v active=%v", retainedB, opB.IsActive())
	}

	bodyB, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(2)})
	if err != nil {
		t.Fatal(err)
	}
	responseB, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &operationID, bodyB,
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.inbound.RouteInbound(runtime.initial, responseB); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue generation B response: disposition=%d error=%v", disposition, err)
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
		t.Fatalf("generation B response body=%x error=%v", received.Body(), err)
	}
	rpc.End(callB)
	rpc.mu.Lock()
	remainingCalls := len(rpc.calls)
	rpc.mu.Unlock()
	if remainingCalls != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("generation B cleanup calls=%d active=%d tombstones=%d",
			remainingCalls, runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestOperationIDSourceUsesOneRandomPrefixAndConcurrentMonotonicSuffixes(t *testing.T) {
	source := &operationIDSource{random: bytes.NewReader(bytes.Repeat([]byte{0x51}, 8))}
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
		if _, duplicate := seen[identity]; duplicate {
			t.Fatalf("duplicate identity=%x", identity)
		}
		seen[identity] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("issued identities=%d", len(seen))
	}
}

func TestOperationIDSourceFailsAtCounterExhaustion(t *testing.T) {
	source := &operationIDSource{
		random: bytes.NewReader([]byte{1}), prefix: [8]byte{1},
		counter: ^uint64(0) - 1, initialized: true,
	}
	identity, err := source.New()
	if err != nil || identity != issuedOperationIDWithPrefix(source.prefix, ^uint64(0)) {
		t.Fatalf("last identity=%x error=%v", identity, err)
	}
	if _, err := source.New(); err != ErrOperationIDExhausted {
		t.Fatalf("post-exhaustion error=%v", err)
	}
}

func issuedOperationID(prefix byte, counter uint64) protocolsession.OperationID {
	return issuedOperationIDWithPrefix([8]byte{
		prefix, prefix, prefix, prefix, prefix, prefix, prefix, prefix,
	}, counter)
}

func issuedOperationIDWithPrefix(prefix [8]byte, counter uint64) protocolsession.OperationID {
	var identity protocolsession.OperationID
	copy(identity[:8], prefix[:])
	binary.BigEndian.PutUint64(identity[8:], counter)
	return identity
}
