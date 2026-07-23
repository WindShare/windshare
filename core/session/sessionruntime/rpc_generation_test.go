package sessionruntime

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type authoritylessOutboundPolicy struct{}

func (authoritylessOutboundPolicy) AdmitOutbound(
	protocolsession.Message,
	protocolsession.OutboundOperationPermit,
) (protocolsession.OutboundAdmission, error) {
	return protocolsession.OutboundAdmission{Disposition: protocolsession.OperationDeliver}, nil
}

func (authoritylessOutboundPolicy) AcceptOutboundReplay(
	protocolsession.Message,
	protocolsession.OutboundReplayPermit,
) (protocolsession.OutboundAdmission, error) {
	return protocolsession.OutboundAdmission{Disposition: protocolsession.OperationDrop}, nil
}

func (authoritylessOutboundPolicy) AcceptOutboundTerminal() error { return nil }
func (authoritylessOutboundPolicy) OutboundDirection() protocolsession.Direction {
	return protocolsession.DirectionReceiverToSender
}

func TestRPCBeginPublishesAuthorityAfterFastResponseWasAlreadyDispatched(t *testing.T) {
	runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x73}, protocolsession.IdentityBytes)))
	if err := rpc.register(runtime.router); err != nil {
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
			disposition, routeErr := runtime.router.RouteInbound(context.Background(), response)
			if routeErr != nil || disposition != protocolsession.OperationDeliver {
				t.Errorf("fast response admission: disposition=%d error=%v", disposition, routeErr)
				return
			}
			event, nextErr := runtime.router.Next(context.Background())
			if nextErr != nil {
				t.Errorf("fast response dequeue: %v", nextErr)
				return
			}
			if dispatchErr := runtime.router.Dispatch(context.Background(), event); dispatchErr != nil {
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

	call, err := rpc.begin(context.Background(), protocolsession.MessageListChildren, []byte{0xa0})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort) }()
	select {
	case <-dispatched:
	default:
		t.Fatal("request send returned before the synchronous response dispatch")
	}
	message, err := rpc.await(context.Background(), call)
	if err != nil || message.Kind() != protocolsession.MessageCatalogResult {
		t.Fatalf("fast response result: kind=%d error=%v", message.Kind(), err)
	}
}

func TestRPCBeginFailClosesAuthoritylessAdmittedCompletion(t *testing.T) {
	runtime, channel := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	binding := protocolsession.EnvelopeBinding{
		ShareInstance: runtime.share, ProtocolSessionID: runtime.sessionID,
		LaneID: runtime.initial.ID, LaneEpoch: runtime.initial.Epoch,
		Direction: protocolsession.DirectionReceiverToSender,
	}
	sealer, err := protocolsession.NewEnvelopeSealer(
		runtime.keys.ReceiverToSender(), binding, &deterministicReader{next: 0x81},
	)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := protocolsession.NewSessionWriter(channel, sealer, authoritylessOutboundPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].writer = writer
	runtime.lanes.mu.Unlock()
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x82}, 8)))
	if _, err := rpc.begin(context.Background(), protocolsession.MessageListChildren, []byte{0xa0}); !errors.Is(err, errRPCOperationAuthority) {
		t.Fatalf("authorityless admitted completion error=%v", err)
	}
	rpc.mu.Lock()
	calls := len(rpc.calls)
	rpc.mu.Unlock()
	if calls != 0 || !runtime.operations.Terminated() || runtime.ctx.Err() == nil {
		t.Fatalf("authority failure calls=%d terminal=%v canceled=%v",
			calls, runtime.operations.Terminated(), runtime.ctx.Err())
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
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x83}, 8)))
	requestContext, cancelRequest := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := rpc.begin(requestContext, protocolsession.MessageListChildren, []byte{0xa0})
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
	now := time.Unix(10_000, 0)
	runtime, channel := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2},
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

	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x84}, 8)))
	operationID := issuedOperationID(0x84, 1)
	request, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &operationID, []byte{0xa0})
	requestReceipt, err := lane.writer.TryControl(request)
	if err != nil {
		t.Fatal(err)
	}
	first := requestReceipt.Await(context.Background())
	if first.Outcome != protocolsession.SendOutcomeDelivered || !first.Admitted {
		t.Fatalf("first request completion=%+v", first)
	}
	call := &operationCall{
		id: operationID, messages: make(chan operationResponse, 1), lane: runtime.initial,
	}
	if !call.setAuthority(first.Generation, first.Operation) {
		t.Fatal("failed to bind first generation")
	}
	rpc.calls[operationID] = call
	final, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &operationID, []byte{0xa0})
	if _, err := runtime.operations.ObserveInbound(protocolsession.DirectionSenderToReceiver, final); err != nil {
		t.Fatal(err)
	}
	if err := rpc.admitCancellation(call, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("final-wins cleanup: %v", err)
	}
	if physicalSends.Load() != 1 {
		t.Fatalf("final-wins cleanup emitted %d wire cancels", physicalSends.Load())
	}
	if disposition, err := runtime.operations.Observe(protocolsession.DirectionSenderToReceiver, final); err != nil || disposition != protocolsession.OperationDrop {
		t.Fatalf("exact final fingerprint changed: disposition=%d error=%v", disposition, err)
	}
	conflictingBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	conflicting, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &operationID, conflictingBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.operations.Observe(protocolsession.DirectionSenderToReceiver, conflicting); !errors.Is(err, protocolsession.ErrConflictingFinal) {
		t.Fatalf("conflicting final error=%v", err)
	}

	// Honest issuers never reuse an OperationID. This forced reuse models a
	// hostile peer after the bounded replay window and exercises local ABA safety.
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	_ = runtime.operations.TombstoneCount()
	secondReceipt, err := lane.writer.TryControl(request)
	if err != nil {
		t.Fatal(err)
	}
	second := secondReceipt.Await(context.Background())
	if second.Outcome != protocolsession.SendOutcomeDelivered || !second.Generation.IsActive() {
		t.Fatalf("forced second generation: %v", err)
	}
	if err := rpc.cancelAndEnd(call, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("stale generation cleanup: %v", err)
	}
	if !second.Generation.IsActive() || runtime.operations.ActiveCount() != 1 || physicalSends.Load() != 2 {
		t.Fatalf("stale cleanup changed second generation active=%v count=%d sends=%d",
			second.Generation.IsActive(), runtime.operations.ActiveCount(), physicalSends.Load())
	}
}

func TestReceiverPeerOperationLateTerminateCannotCrossSameIDGeneration(t *testing.T) {
	now := time.Unix(15_000, 0)
	runtime, channel := newUnstartedRuntimeWithPolicy(
		t,
		protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2},
		func() time.Time { return now },
	)
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	var physicalSends atomic.Int32
	channel.onSend = func(framechannel.Frame) { physicalSends.Add(1) }
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	t.Cleanup(func() {
		stopWriter()
		<-writerDone
		runtime.abortBeforeStart()
	})

	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x86}, 8)))
	operationID := issuedOperationID(0x86, 1)
	offer, err := protocolsession.NewMessage(
		protocolsession.MessagePeerOffer,
		&operationID,
		[]byte{0xf6},
	)
	if err != nil {
		t.Fatal(err)
	}
	admit := func() (*ReceiverPeerOperation, *operationCall, protocolsession.OperationGeneration) {
		receipt, sendErr := lane.writer.TryControl(offer)
		if sendErr != nil {
			t.Fatal(sendErr)
		}
		completion := receipt.Await(context.Background())
		if completion.Outcome != protocolsession.SendOutcomeDelivered || !completion.Admitted ||
			completion.Generation.IsZero() || completion.Operation.IsZero() {
			t.Fatalf("same-ID peer offer completion=%+v", completion)
		}
		call := &operationCall{
			id: operationID, messages: make(chan operationResponse, 1), done: make(chan struct{}),
			lane: runtime.initial,
		}
		if !call.setAuthority(completion.Generation, completion.Operation) {
			t.Fatal("failed to bind peer operation generation")
		}
		rpc.mu.Lock()
		rpc.calls[operationID] = call
		rpc.mu.Unlock()
		return &ReceiverPeerOperation{
			rpc: rpc, call: call, token: new(receiverPeerOperationToken),
		}, call, completion.Generation
	}

	operationA, callA, generationA := admit()
	if err := runtime.operations.CancelGeneration(generationA); err != nil {
		t.Fatalf("retire generation A: %v", err)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("generation A retirement active=%d tombstones=%d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	_ = runtime.operations.TombstoneCount()

	operationB, callB, generationB := admit()
	if generationA.Same(generationB) || !generationB.IsActive() {
		t.Fatalf("same-ID generation reuse A=%+v B=%+v", generationA, generationB)
	}
	terminationA := operationA.Terminate(context.Background())
	assertReceiverPeerTermination(
		t,
		operationA,
		terminationA,
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerDiagnosticCleanupFailed,
	)
	rpc.mu.Lock()
	current := rpc.calls[operationID]
	rpc.mu.Unlock()
	callB.stateMu.Lock()
	callBClosed := callB.closed
	callB.stateMu.Unlock()
	if current != callB || callBClosed || callB.lane != runtime.initial ||
		!generationB.IsActive() || runtime.operations.ActiveCount() != 1 ||
		physicalSends.Load() != 2 {
		t.Fatalf(
			"late A cleanup crossed B current=%t closed=%t lane=%+v active=%t table=%d sends=%d",
			current == callB,
			callBClosed,
			callB.lane,
			generationB.IsActive(),
			runtime.operations.ActiveCount(),
			physicalSends.Load(),
		)
	}
	if callA == callB || operationA.OperationID() != operationID {
		t.Fatal("late generation A operation changed identity or aliased B watcher")
	}
	if _, err := operationB.SendCandidate(context.Background(), []byte{0xf6}); err != nil {
		t.Fatalf("generation B candidate after late A cancel: %v", err)
	}
	assertReceiverPeerTermination(
		t,
		operationB,
		operationB.Terminate(context.Background()),
		ReceiverPeerTerminalAuthorityLocal,
		ReceiverPeerProvenanceLocalExplicitStop,
		ReceiverPeerTerminalOperationOnly,
		ReceiverPeerProvenanceLocalExplicitStop,
	)
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("generation B completion active=%d tombstones=%d",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestRPCQueuedStaleResponseCannotCrossSameIDGeneration(t *testing.T) {
	now := time.Unix(20_000, 0)
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleReceiver,
		protocolsession.OperationLimits{MaxActive: 2, MaxTombstones: 2},
		func() time.Time { return now },
	)
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
	rpc := newRPCClient(runtime, bytes.NewReader(bytes.Repeat([]byte{0x85}, 8)))
	if err := rpc.register(runtime.router); err != nil {
		t.Fatal(err)
	}
	operationID := issuedOperationID(0x85, 1)
	request, err := protocolsession.NewMessage(
		protocolsession.MessageListChildren, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	beginGeneration := func() (*operationCall, protocolsession.OperationGeneration) {
		receipt, sendErr := lane.writer.TryControl(request)
		if sendErr != nil {
			t.Fatal(sendErr)
		}
		completion := receipt.Await(context.Background())
		if completion.Outcome != protocolsession.SendOutcomeDelivered || !completion.Admitted ||
			completion.Generation.IsZero() || completion.Operation.IsZero() {
			t.Fatalf("same-ID request completion=%+v", completion)
		}
		call := &operationCall{
			id: operationID, messages: make(chan operationResponse, 1), lane: runtime.initial,
		}
		if !call.setAuthority(completion.Generation, completion.Operation) {
			t.Fatal("failed to bind same-ID generation")
		}
		return call, completion.Generation
	}

	callA, generationA := beginGeneration()
	rpc.calls[operationID] = callA
	responseA, err := protocolsession.NewMessage(
		protocolsession.MessageCatalogResult, &operationID, []byte{0xa0},
	)
	if err != nil {
		t.Fatal(err)
	}
	if disposition, err := runtime.router.RouteInbound(context.Background(), responseA); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue generation A response: disposition=%d error=%v", disposition, err)
	}
	if generationA.IsActive() || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("generation A final lifecycle active=%v activeCount=%d tombstones=%d",
			generationA.IsActive(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
	if disposition, err := runtime.operations.Observe(
		protocolsession.DirectionSenderToReceiver, responseA,
	); err != nil || disposition != protocolsession.OperationDrop {
		t.Fatalf("generation A exact tombstone: disposition=%d error=%v", disposition, err)
	}

	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Nanosecond)
	_ = runtime.operations.TombstoneCount()
	callB, generationB := beginGeneration()
	if generationA.Same(generationB) || !generationB.IsActive() {
		t.Fatalf("generation B authority A=%+v B=%+v", generationA, generationB)
	}
	rpc.calls[operationID] = callB
	if err := rpc.cancelAndEnd(callA, contentflow.CancelReasonOutputAbort); err != nil {
		t.Fatalf("generation A cleanup after B admission: %v", err)
	}
	select {
	case <-callA.doneChannel():
	default:
		t.Fatal("generation A cleanup did not close its exact response sink")
	}
	if generation, authority := callA.operationAuthority(); !generation.IsZero() || !authority.IsZero() {
		t.Fatal("generation A cleanup retained operation authority")
	}
	rpc.mu.Lock()
	retainedB := rpc.calls[operationID] == callB
	rpc.mu.Unlock()
	if !retainedB || !generationB.IsActive() {
		t.Fatalf("generation A cleanup changed B: retained=%v active=%v", retainedB, generationB.IsActive())
	}

	staleEvent, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), staleEvent); err != nil {
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
	if !retainedB || !generationB.IsActive() {
		t.Fatalf("stale dispatch changed B: retained=%v active=%v", retainedB, generationB.IsActive())
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
	if disposition, err := runtime.router.RouteInbound(context.Background(), responseB); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("queue generation B response: disposition=%d error=%v", disposition, err)
	}
	eventB, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), eventB); err != nil {
		t.Fatal(err)
	}
	received, err := rpc.await(context.Background(), callB)
	if err != nil || !bytes.Equal(received.Body(), bodyB) {
		t.Fatalf("generation B response body=%x error=%v", received.Body(), err)
	}
	rpc.end(callB)
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
		wait.Add(1)
		go func() {
			defer wait.Done()
			identity, err := source.New()
			if err != nil {
				errorsSeen <- err
				return
			}
			identities <- identity
		}()
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
