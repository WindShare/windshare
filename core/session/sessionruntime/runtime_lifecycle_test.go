package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRuntimeStoppingPrecedesFinalDonePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &runtimeCore{ctx: ctx, done: make(chan struct{})}
	if runtime.Stopping() {
		t.Fatal("live runtime reported stopping")
	}
	cancel()
	if !runtime.Stopping() {
		t.Fatal("runtime cancellation was hidden until final Done publication")
	}
	select {
	case <-runtime.Done():
		t.Fatal("test did not preserve the cancellation-to-finalization gap")
	default:
	}
	var missing *runtimeCore
	if !missing.Stopping() {
		t.Fatal("missing runtime reported available")
	}
}

func TestUnstartedRuntimeRollsBackLaneAuthorityAndOwnedChannels(t *testing.T) {
	runtime, initialChannel := newUnstartedRuntime(t, protocolsession.RoleSender)
	(*runtimeCore)(nil).abortBeforeStart()
	(*runtimeCore)(nil).close()
	if !errors.Is((*runtimeCore)(nil).Err(), ErrRuntimeClosed) {
		t.Fatal("nil runtime did not report a closed authority")
	}

	authenticator := permissiveInboundAuthenticator()
	duplicate, duplicatePeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = duplicatePeer.Close() })
	if _, err := runtime.lanes.add(LaneIdentity{ID: runtime.initial.ID, Epoch: 1}, duplicate, authenticator, false); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("duplicate active lane error = %v", err)
	}
	if duplicate.State() != framechannel.Closed {
		t.Fatal("duplicate candidate channel remained owned by connectivity")
	}

	fresh, freshPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = freshPeer.Close() })
	identity := LaneIdentity{ID: 2, Epoch: 2}
	if _, err := runtime.lanes.add(identity, fresh, authenticator, false); err != nil {
		t.Fatal(err)
	}
	if !runtime.lanes.detach(identity) {
		t.Fatal("unstarted admitted lane did not detach")
	}
	stale, stalePeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = stalePeer.Close() })
	if _, err := runtime.lanes.add(LaneIdentity{ID: identity.ID, Epoch: 1}, stale, authenticator, false); !errors.Is(err, ErrLaneStale) {
		t.Fatalf("stale lane error = %v", err)
	}

	admissionFailure := errors.New("admission failed")
	rolledBack, rolledBackPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = rolledBackPeer.Close() })
	if _, err := runtime.lanes.addWithAdmission(
		LaneIdentity{ID: 3, Epoch: 1}, rolledBack, authenticator, false,
		func() error { return admissionFailure },
	); !errors.Is(err, admissionFailure) {
		t.Fatalf("admission rollback error = %v", err)
	}
	if rolledBack.State() != framechannel.Closed {
		t.Fatal("rejected lane channel was not closed")
	}

	initial := runtime.lanes.active[runtime.initial.ID]
	if initial == nil {
		t.Fatal("initial lane owner is missing")
	}
	initial.started = true
	runtime.lanes.startLocked(initial)
	initial.started = false
	initial.closing = true
	runtime.lanes.startLocked(initial)
	initial.closing = false

	empty := newRuntimeLanes(runtime)
	if _, err := empty.selectLane(nil); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("empty lane selection error = %v", err)
	}
	empty.order = []uint32{91}
	if _, err := empty.selectLane(nil); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("inactive lane-order error = %v", err)
	}
	missing := LaneIdentity{ID: 91, Epoch: 1}
	if _, err := empty.selectLane(&missing); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("missing preferred lane error = %v", err)
	}
	empty.started = true
	empty.start()
	empty.started = false
	empty.stopping = true
	empty.start()
	if _, err := empty.selectLane(nil); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("stopping lane selection error = %v", err)
	}
	stopping, stoppingPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = stoppingPeer.Close() })
	if _, err := empty.add(LaneIdentity{ID: 92, Epoch: 1}, stopping, authenticator, false); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("stopping lane admission error = %v", err)
	}
	if stopping.State() != framechannel.Closed {
		t.Fatal("stopping runtime retained candidate channel")
	}

	runtime.abortBeforeStart()
	runtime.abortBeforeStart()
	select {
	case <-runtime.Done():
	default:
		t.Fatal("construction abort did not finish runtime ownership")
	}
	if initialChannel.State() != framechannel.Closed {
		t.Fatal("construction abort did not close initial channel")
	}
}

func TestSelectedLaneSnapshotOutlivesNaturalOwnerRelease(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.lanes.mu.Lock()
	owner := runtime.lanes.active[runtime.initial.ID]
	runtime.lanes.mu.Unlock()
	if owner == nil {
		t.Fatal("initial lane owner is missing")
	}

	type selectionResult struct {
		lane selectedLane
		err  error
	}
	selected := make(chan selectionResult, 1)
	released := make(chan struct{})
	used := make(chan bool, 1)
	go func() {
		lane, err := runtime.lanes.selectLane(&runtime.initial)
		selected <- selectionResult{lane: lane, err: err}
		if err != nil {
			return
		}
		<-released
		used <- lane.identity == runtime.initial && lane.channel != nil &&
			lane.writer != nil && lane.writer.Done() != nil && lane.done != nil
	}()

	result := <-selected
	if result.err != nil {
		t.Fatal(result.err)
	}
	// This is the hazardous interleaving: registry selection has unlocked, then
	// natural detach clears its mutable owner before the selected caller uses it.
	runtime.lanes.finishLane(owner)
	owner.releaseRuntimeReferences()
	close(owner.done)
	close(released)
	if !<-used {
		t.Fatal("selected access lost immutable writer/channel references after detach")
	}
	if owner.writer != nil || owner.channel != nil || owner.sealer != nil || owner.opener != nil {
		t.Fatal("natural detach retained mutable lane-owned references")
	}
	select {
	case <-result.lane.done:
	default:
		t.Fatal("selected access did not retain the exact lane completion signal")
	}
}

func TestRuntimeComponentFailureCancelsPhysicalLaneBeforeFinish(t *testing.T) {
	runtime, initialChannel := newUnstartedRuntime(t, protocolsession.RoleSender)
	componentFailure := errors.New("component failed")
	runtime.start(func(context.Context) error { return componentFailure })
	select {
	case <-runtime.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime did not finish after a component failure")
	}
	if !errors.Is(runtime.Err(), componentFailure) {
		t.Fatalf("runtime retained error = %v", runtime.Err())
	}
	if initialChannel.State() != framechannel.Closed {
		t.Fatal("runtime finished before closing its physical lane")
	}
	runtime.close()
}

func TestRuntimeCleanCloseDrainsFullServiceQueue(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	handler := newCatalogHandler(nil, senderOutbound{})
	for range cap(handler.queue) {
		// Zero generations model work already invalidated by the same terminal
		// transition; Run must suppress it without touching the closed service.
		handler.queue <- catalogOperation{}
	}
	runtime.start(handler.Run)
	runtime.close()
	if runtime.Err() != nil {
		t.Fatalf("clean close retained component error=%v", runtime.Err())
	}
	if len(handler.queue) != 0 {
		t.Fatalf("clean close retained %d service queue entries", len(handler.queue))
	}
	handler.queueMu.Lock()
	stopping := handler.stopping
	handler.queueMu.Unlock()
	handler.mu.Lock()
	active := len(handler.active)
	handler.mu.Unlock()
	if !stopping || active != 0 {
		t.Fatalf("handler stopping=%v active=%d", stopping, active)
	}
	if runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 0 ||
		!runtime.operations.Terminated() {
		t.Fatalf(
			"clean close operation state active=%d tombstones=%d terminal=%v",
			runtime.operations.ActiveCount(), runtime.operations.TombstoneCount(), runtime.operations.Terminated(),
		)
	}
}

func TestOperationLaneRoutesPreserveFirstPhysicalAuthorityUntilFinal(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	routes := newOperationLaneRoutes()
	operationID := id16[protocolsession.OperationID](61)
	other := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if routes.reserve(protocolsession.OperationID{}, runtime.initial) != nil ||
		routes.reserve(operationID, LaneIdentity{}) != nil {
		t.Fatal("route accepted a zero operation or lane identity")
	}
	route := routes.reserve(operationID, runtime.initial)
	if route == nil || routes.reserve(operationID, other) != nil {
		t.Fatal("operation route did not retain its first physical lane")
	}
	lane, err := routes.resolveRoute(runtime.lanes, operationID, route)
	if err != nil || lane.identity != runtime.initial {
		t.Fatalf("resolved route = %+v, %v", lane, err)
	}
	routes.releaseRoute(operationID, &operationLaneRoute{preferred: other})
	lane, err = routes.resolveRoute(runtime.lanes, operationID, route)
	if err != nil || lane.identity != runtime.initial {
		t.Fatalf("mismatched release changed route = %+v, %v", lane, err)
	}
	routes.releaseRoute(operationID, route)
	if current := routes.current(operationID); current != nil {
		t.Fatalf("released route was implicitly resurrected = %+v", current)
	}
	route = routes.reserve(operationID, runtime.initial)
	if route == nil {
		t.Fatal("released operation route could not be explicitly reserved")
	}
	otherChannel := newMemoryChannel(t)
	authenticator := permissiveInboundAuthenticator()
	if _, err = runtime.lanes.add(other, otherChannel, authenticator, false); err != nil {
		t.Fatal(err)
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if lane, err = routes.resolveRoute(runtime.lanes, operationID, route); err != nil || lane.identity != other {
		t.Fatalf("detached-lane continuation route = %+v, %v", lane, err)
	}
	routes.releaseRoute(operationID, route)

	var nilRoutes *operationLaneRoutes
	nilRoutes.releaseRoute(operationID, route)
	if nilRoutes.reserve(operationID, runtime.initial) != nil {
		t.Fatal("nil route authority admitted an operation")
	}
	if current := nilRoutes.current(operationID); current != nil {
		t.Fatalf("nil route resolution = %+v", current)
	}

	blockRequest, err := contentflow.NewBlockRequest(id16[content.LeaseID](62), []uint64{0})
	if err != nil {
		t.Fatal(err)
	}
	body, err := contentflow.EncodeBlockRequest(blockRequest)
	if err != nil {
		t.Fatal(err)
	}
	request, err := protocolsession.NewMessage(protocolsession.MessageRequestBlocks, &operationID, body)
	if err != nil {
		t.Fatal(err)
	}
	firstInbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
	if disposition, err := firstInbound.RouteInbound(context.Background(), request); err != nil || disposition != protocolsession.OperationDeliver {
		t.Fatalf("first physical request disposition=%d error=%v", disposition, err)
	}
	inbound := laneInboundRouter{runtime: runtime, identity: other}
	if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil || disposition != protocolsession.OperationDrop {
		t.Fatalf("exact cross-lane replay disposition=%d error=%v", disposition, err)
	}
	if retained := runtime.routes.current(operationID); retained == nil || retained.preferred != runtime.initial {
		t.Fatalf("exact replay changed first physical authority: %+v", retained)
	}
	if inbound.InboundDirection() != protocolsession.DirectionReceiverToSender {
		t.Fatal("lane router changed the shared inbound direction")
	}
	if !senderResponseFinal(protocolsession.MessageOperationComplete) ||
		senderResponseFinal(protocolsession.MessageScanProgress) {
		t.Fatal("sender response finality changed")
	}
	if err := inbound.TerminateLocal(); err != nil {
		t.Fatal(err)
	}
}

func TestOperationLaneRouteCannotReviveAPathWithoutASurvivingLane(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	t.Cleanup(runtime.abortBeforeStart)
	routes := newOperationLaneRoutes()
	operationID := id16[protocolsession.OperationID](83)
	route := routes.reserve(operationID, runtime.initial)
	if route == nil {
		t.Fatal("operation route did not retain its original lane")
	}
	runtime.lanes.mu.Lock()
	runtime.lanes.active[runtime.initial.ID].closing = true
	runtime.lanes.mu.Unlock()
	if _, err := routes.resolveRoute(runtime.lanes, operationID, route); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("route without a surviving lane error = %v", err)
	}
}

func TestRuntimeLaneConstructionRejectsInvalidCryptographicDependencies(t *testing.T) {
	identity := LaneIdentity{ID: 2, Epoch: 1}
	t.Run("invalid runtime role", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		runtime.role = protocolsession.Role(255)
		if _, err := runtime.lanes.build(
			identity, newMemoryChannel(t), permissiveInboundAuthenticator(),
		); err == nil {
			t.Fatal("lane accepted an invalid runtime role")
		}
	})
	t.Run("missing nonce source", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		runtime.random = nil
		if _, err := runtime.lanes.build(
			identity, newMemoryChannel(t), permissiveInboundAuthenticator(),
		); err == nil {
			t.Fatal("lane accepted a missing envelope nonce source")
		}
	})
	t.Run("missing physical channel", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		if _, err := runtime.lanes.build(
			identity, nil, permissiveInboundAuthenticator(),
		); err == nil {
			t.Fatal("lane accepted a missing physical channel")
		}
	})
	t.Run("missing inbound authenticator", func(t *testing.T) {
		runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
		t.Cleanup(runtime.abortBeforeStart)
		if _, err := runtime.lanes.build(identity, newMemoryChannel(t), nil); err == nil {
			t.Fatal("lane accepted a missing inbound authenticator")
		}
	})
	var nilLanes *runtimeLanes
	if _, err := nilLanes.addWithAdmission(identity, nil, nil, false, nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil lane registry error = %v", err)
	}
}

func TestLaneAttachmentBoundarySilentlyClosesUntrustedFailures(t *testing.T) {
	fixture := newVerticalFixture(t)
	if _, err := (*SenderFactory)(nil).Attach(context.Background(), newMemoryChannel(t)); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil attach factory error = %v", err)
	}
	if _, err := fixture.senderFactory.Attach(context.Background(), nil); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("nil attach channel error = %v", err)
	}

	closed, closedPeer := newMemoryChannelPair()
	_ = closed.Close()
	t.Cleanup(func() { _ = closedPeer.Close() })
	if _, err := fixture.senderFactory.Attach(context.Background(), closed); !errors.Is(err, ErrHandshake) {
		t.Fatalf("closed candidate error = %v", err)
	}
	malformed, malformedPeer := newMemoryChannelPair()
	_ = malformedPeer.Send(context.Background(), framechannel.Frame{1})
	if _, err := fixture.senderFactory.Attach(context.Background(), malformed); !errors.Is(err, ErrHandshake) {
		t.Fatalf("malformed candidate error = %v", err)
	}
	if malformed.State() != framechannel.Closed {
		t.Fatal("malformed candidate was reflected instead of silently closed")
	}
	_ = malformedPeer.Close()

	key, err := protocolsession.TrafficKeyFromBytes(
		bytes.Repeat([]byte{7}, protocolsession.TrafficKeyBytes),
		protocolsession.DirectionReceiverToSender,
	)
	if err != nil {
		t.Fatal(err)
	}
	unknownHello, err := protocolsession.NewLaneHello(
		fixture.share, id16[protocolsession.ProtocolSessionID](73), 7, 1,
		id16[protocolsession.OperationID](74), bytes.Repeat([]byte{75}, protocolsession.LaneAttachNonceBytes), key,
	)
	if err != nil {
		t.Fatal(err)
	}
	unknown, unknownPeer := newMemoryChannelPair()
	_ = unknownPeer.Send(context.Background(), framechannel.Frame(unknownHello.Encoded()))
	if _, err := fixture.senderFactory.Attach(context.Background(), unknown); !errors.Is(err, ErrHandshake) {
		t.Fatalf("unknown session candidate error = %v", err)
	}
	if unknown.State() != framechannel.Closed {
		t.Fatal("unknown session candidate remained open")
	}
	_ = unknownPeer.Close()

	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	if _, err := (*ReceiverRuntime)(nil).RequestLane(context.Background(), 0); !errors.Is(err, ErrRuntimeClosed) {
		t.Fatalf("nil lane request error = %v", err)
	}
	if _, err := receiver.AttachLane(context.Background(), LaneAttachmentGrant{}, newMemoryChannel(t)); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("invalid lane grant error = %v", err)
	}

	initialID, initialEpoch := receiver.LaneIdentity()
	duplicateGrant, err := receiver.RequestLane(context.Background(), initialID)
	if err != nil {
		t.Fatal(err)
	}
	_, receiverErr, senderErr := attachGrantedLane(t, fixture.senderFactory, receiver, duplicateGrant)
	var receiverRejection, senderRejection *LaneRejectedError
	if !errors.As(receiverErr, &receiverRejection) || !errors.As(senderErr, &senderRejection) ||
		receiverRejection.Rejection.Code != protocolsession.LaneRejectAdmissionLimited ||
		senderRejection.Rejection.Code != protocolsession.LaneRejectAdmissionLimited {
		t.Fatalf("active lane replacement errors = receiver %v sender %v", receiverErr, senderErr)
	}

	sendFailureGrant := mustRequestLane(t, receiver)
	sendFailure, sendFailurePeer := newMemoryChannelPair()
	_ = sendFailure.Close()
	t.Cleanup(func() { _ = sendFailurePeer.Close() })
	if _, err := receiver.AttachLane(context.Background(), sendFailureGrant, sendFailure); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lane hello send error = %v", err)
	}

	receiveFailureGrant := mustRequestLane(t, receiver)
	receiveFailure, receiveFailurePeer := newMemoryChannelPair()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.AttachLane(cancelled, receiveFailureGrant, receiveFailure); !errors.Is(err, context.Canceled) {
		t.Fatalf("lane response cancellation = %v", err)
	}
	_ = receiveFailurePeer.Close()

	badRejectGrant := mustRequestLane(t, receiver)
	badReject, badRejectPeer := newMemoryChannelPair()
	go respondToLaneHello(badRejectPeer, make([]byte, protocolsession.LaneRejectBytes))
	if _, err := receiver.AttachLane(context.Background(), badRejectGrant, badReject); err == nil {
		t.Fatal("unsigned lane rejection was accepted")
	}
	_ = badRejectPeer.Close()

	badAcceptGrant := mustRequestLane(t, receiver)
	badAccept, badAcceptPeer := newMemoryChannelPair()
	go respondToLaneHello(badAcceptPeer, []byte{1})
	if _, err := receiver.AttachLane(context.Background(), badAcceptGrant, badAccept); err == nil {
		t.Fatal("malformed lane acceptance was accepted")
	}
	_ = badAcceptPeer.Close()

	sendResponseGrant := mustRequestLane(t, receiver)
	sendResponseHello := laneHelloForGrant(t, receiver, sendResponseGrant)
	sendResponse, sendResponsePeer := newMemoryChannelPair()
	_ = sendResponsePeer.Close()
	if _, err := sender.acceptCandidate(context.Background(), sendResponse, sendResponseHello); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lane acceptance send error = %v", err)
	}

	randomGrant := mustRequestLane(t, receiver)
	randomHello := laneHelloForGrant(t, receiver, randomGrant)
	sender.random = edgeErrorReader{}
	randomFailure, randomFailurePeer := newMemoryChannelPair()
	if _, err := sender.acceptCandidate(context.Background(), randomFailure, randomHello); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lane sender nonce error = %v", err)
	}
	if randomFailure.State() != framechannel.Closed {
		t.Fatal("nonce-failed candidate remained open")
	}
	_ = randomFailurePeer.Close()

	for _, invalid := range []string{string([]byte{0xff}), "e\u0301", strings.Repeat("x", MaximumTerminalMessageBytes+1)} {
		if err := sender.Stop(context.Background(), invalid); !errors.Is(err, ErrRuntimeConfig) {
			t.Fatalf("invalid terminal message error = %v", err)
		}
	}
	identity := LaneIdentity{ID: initialID, Epoch: initialEpoch}
	fragments, err := contentflow.FragmentRecord(id16[protocolsession.OperationID](77), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if !receiver.DetachLane(identity) || !sender.DetachLane(identity) {
		t.Fatal("initial lane did not detach")
	}
	if err := sender.outbound.SendFragment(context.Background(), fragments[0]); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("fragment without lane error = %v", err)
	}
	invalidFailureID := id16[protocolsession.OperationID](76)
	invalidFailureRoute := sender.routes.reserve(invalidFailureID, identity)
	if invalidFailureRoute == nil {
		t.Fatal("failed to reserve invalid failure route")
	}
	invalidFailureContext := bindOutboundRoute(context.Background(), invalidFailureID, invalidFailureRoute)
	if err := sender.outbound.SendOperationError(invalidFailureContext, invalidFailureID, contentflow.OperationFailure{}); err == nil {
		t.Fatal("invalid operation failure was sent")
	}
	sender.routes.mu.Lock()
	_, retainedInvalidFailure := sender.routes.routes[invalidFailureID]
	sender.routes.mu.Unlock()
	if retainedInvalidFailure {
		t.Fatal("invalid operation failure retained its lane route")
	}
}

func TestServiceHandlersEnforceBoundedQueuesAndSharedDirectoryFailureSchema(t *testing.T) {
	body, err := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	operationID := id16[protocolsession.OperationID](81)
	list, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &operationID, body)
	laneBody, _ := encodeLaneAttachRequest(0)
	laneRequest, _ := protocolsession.NewMessage(protocolsession.MessageLaneAttach, &operationID, laneBody)

	catalogHandler := newCatalogHandler(nil, senderOutbound{})
	if err := catalogHandler.HandleMessage(context.Background(), terminal); !errors.Is(err, contentflow.ErrUnexpectedMessage) {
		t.Fatalf("catalog wrong-kind error = %v", err)
	}
	listContext := senderIngressContext(t, list)
	for range cap(catalogHandler.queue) {
		if err := catalogHandler.HandleMessage(listContext, list); err != nil {
			t.Fatal(err)
		}
	}
	if err := catalogHandler.HandleMessage(listContext, list); !errors.Is(err, contentflow.ErrServiceQueueFull) {
		t.Fatalf("catalog queue overflow error = %v", err)
	}

	laneHandler := newLaneGrantHandler(nil, senderOutbound{}, edgeErrorReader{})
	if err := laneHandler.HandleMessage(context.Background(), terminal); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("lane wrong-kind error = %v", err)
	}
	laneContext := senderIngressContext(t, laneRequest)
	for range cap(laneHandler.queue) {
		if err := laneHandler.HandleMessage(laneContext, laneRequest); err != nil {
			t.Fatal(err)
		}
	}
	if err := laneHandler.HandleMessage(laneContext, laneRequest); !errors.Is(err, ErrOperationOverflow) {
		t.Fatalf("lane queue overflow error = %v", err)
	}
	malformedHandler := newLaneGrantHandler(nil, senderOutbound{}, edgeErrorReader{})
	malformed, _ := protocolsession.NewMessage(protocolsession.MessageLaneAttach, &operationID, body)
	if err := malformedHandler.HandleMessage(senderIngressContext(t, malformed), malformed); err != nil {
		t.Fatal(err)
	}
	if err := malformedHandler.Run(context.Background()); err == nil {
		t.Fatal("malformed queued lane request did not fail its handler")
	}
	if err := laneHandler.process(context.Background(), laneRequest); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lane grant nonce error = %v", err)
	}

	var cancelled bool
	cancelID := id16[protocolsession.OperationID](82)
	cancelKey := catalogOperationKey{id: cancelID}
	catalogHandler.active[cancelKey] = func() { cancelled = true }
	catalogHandler.cancel(cancelKey)
	if !cancelled || catalogHandler.add(cancelKey, func() {}) {
		t.Fatal("catalog operation cancellation lost duplicate authority")
	}
	catalogHandler.remove(cancelKey)
	if err := (cancelMux{catalog: catalogHandler}).HandleMessage(context.Background(), terminal); !errors.Is(err, ErrRuntimeConfig) {
		t.Fatalf("cancel without operation error = %v", err)
	}

	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	call, err := receiver.rpc.begin(context.Background(), protocolsession.MessageListChildren, body)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.end(call)
	message, err := receiver.rpc.await(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if message.Kind() != protocolsession.MessageOperationError {
		t.Fatalf("malformed catalog response kind = %d", message.Kind())
	}
	unsigned, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		t.Fatal(err)
	}
	want, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopeDirectory,
		Code:  catalogflow.DirectoryCodePermanentIO, Message: "Catalog operation failed",
	})
	if err != nil || !bytes.Equal(unsigned, want) {
		t.Fatalf("directory operation failure = %x, want %x, err %v", unsigned, want, err)
	}
}

func newUnstartedRuntime(t *testing.T, role protocolsession.Role) (*runtimeCore, *memoryChannel) {
	return newUnstartedRuntimeWithPolicy(t, role, protocolsession.OperationLimits{}, nil)
}

func newUnstartedRuntimeWithPolicy(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
) (*runtimeCore, *memoryChannel) {
	return newUnstartedRuntimeWithContinuations(t, role, limits, now, nil)
}

func newUnstartedRuntimeWithContinuations(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
	continuations protocolsession.OperationContinuationClassifier,
) (*runtimeCore, *memoryChannel) {
	t.Helper()
	share := id16[catalog.ShareInstance](91)
	authKey := bytes.Repeat([]byte{92}, protocolsession.SessionAuthKeyBytes)
	receiverPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: 93})
	if err != nil {
		t.Fatal(err)
	}
	client, err := protocolsession.NewClientHello(
		share, id16[protocolsession.ReceiverInstanceID](94),
		bytes.Repeat([]byte{95}, protocolsession.HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := protocolsession.NewClientHelloReplayGuard(8, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, err = replay.AcceptClientHello(client.Encoded(), share, authKey)
	if err != nil {
		t.Fatal(err)
	}
	senderPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: 96})
	if err != nil {
		t.Fatal(err)
	}
	signingKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{97}, ed25519.SeedSize))
	server, err := protocolsession.NewServerHello(
		client, bytes.Repeat([]byte{98}, protocolsession.HandshakeNonceBytes),
		senderPrivate.PublicKey(), 1, signingKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	var keys protocolsession.SessionKeys
	if role == protocolsession.RoleSender {
		keys, err = protocolsession.DeriveSenderSession(senderPrivate, authKey, client, server)
	} else {
		keys, err = protocolsession.DeriveReceiverSession(receiverPrivate, authKey, client, server)
	}
	if err != nil {
		t.Fatal(err)
	}
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	runtime, err := newRuntime(runtimeConfig{
		Share: share, Role: role, Keys: keys, LaneID: 1, Channel: channel,
		Random: &deterministicReader{next: 99}, Authenticator: permissiveInboundAuthenticator(),
		Continuations: continuations, OperationLimits: limits, Now: now,
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	t.Cleanup(runtime.abortBeforeStart)
	return runtime, channel
}

func permissiveInboundAuthenticator() protocolsession.InboundMessageAuthenticator {
	return protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
}

func mustRequestLane(t *testing.T, runtime *ReceiverRuntime) LaneAttachmentGrant {
	t.Helper()
	grant, err := runtime.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func laneHelloForGrant(t *testing.T, runtime *ReceiverRuntime, grant LaneAttachmentGrant) []byte {
	t.Helper()
	hello, err := protocolsession.NewLaneHello(
		runtime.descriptor.ShareInstance(), runtime.ProtocolSessionID(), grant.LaneID, grant.LaneEpoch,
		grant.OperationID, grant.AttachNonce[:], runtime.keys.ReceiverToSender(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return hello.Encoded()
}

func respondToLaneHello(channel *memoryChannel, response []byte) {
	<-channel.Recv()
	_ = channel.Send(context.Background(), framechannel.Frame(response))
}
