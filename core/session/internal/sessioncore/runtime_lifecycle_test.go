package sessioncore

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

func TestRuntimeStoppingPrecedesFinalDonePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &Runtime{ctx: ctx, done: make(chan struct{})}
	if runtime.Stopping() || runtime.Lifetime().Err() != nil {
		t.Fatal("live runtime reported stopping")
	}
	cancel()
	if !runtime.Stopping() || !errors.Is(runtime.Lifetime().Err(), context.Canceled) {
		t.Fatal("runtime cancellation was hidden until final Done publication")
	}
	select {
	case <-runtime.Lifetime().Done():
	default:
		t.Fatal("borrowed session lifetime did not revoke dependent work")
	}
	select {
	case <-runtime.Done():
		t.Fatal("test did not preserve the cancellation-to-finalization gap")
	default:
	}
	var missing *Runtime
	if !missing.Stopping() {
		t.Fatal("missing runtime reported available")
	}
}

func TestUnstartedRuntimeRollsBackLaneAuthorityAndOwnedChannels(t *testing.T) {
	runtime, initialChannel := newUnstartedRuntime(t, protocolsession.RoleSender)
	(*Runtime)(nil).abortBeforeStart()
	(*Runtime)(nil).close()
	if !errors.Is((*Runtime)(nil).Err(), sessioncontract.ErrRuntimeClosed) {
		t.Fatal("nil runtime did not report a closed authority")
	}

	authenticator := permissiveInboundAuthenticator()
	duplicate, duplicatePeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = duplicatePeer.Close() })
	if _, err := runtime.lanes.add(sessioncontract.LaneIdentity{ID: runtime.initial.ID, Epoch: 1}, duplicate, authenticator, false); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("duplicate active lane error = %v", err)
	}
	if duplicate.State() != framechannel.Closed {
		t.Fatal("duplicate candidate channel remained owned by connectivity")
	}

	fresh, freshPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = freshPeer.Close() })
	identity := sessioncontract.LaneIdentity{ID: 2, Epoch: 2}
	if _, err := runtime.lanes.add(identity, fresh, authenticator, false); err != nil {
		t.Fatal(err)
	}
	if !runtime.lanes.detach(identity) {
		t.Fatal("unstarted admitted lane did not detach")
	}
	stale, stalePeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = stalePeer.Close() })
	if _, err := runtime.lanes.add(sessioncontract.LaneIdentity{ID: identity.ID, Epoch: 1}, stale, authenticator, false); !errors.Is(err, ErrLaneStale) {
		t.Fatalf("stale lane error = %v", err)
	}

	admissionFailure := errors.New("admission failed")
	rolledBack, rolledBackPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = rolledBackPeer.Close() })
	if _, err := runtime.lanes.addWithAdmission(
		sessioncontract.LaneIdentity{ID: 3, Epoch: 1}, rolledBack, authenticator, false,
		func(*RuntimeLane) error { return admissionFailure },
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
	missing := sessioncontract.LaneIdentity{ID: 91, Epoch: 1}
	if _, err := empty.selectLane(&missing); !errors.Is(err, ErrLaneUnavailable) {
		t.Fatalf("missing preferred lane error = %v", err)
	}
	empty.started = true
	empty.start()
	empty.started = false
	empty.stopping = true
	empty.start()
	if _, err := empty.selectLane(nil); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
		t.Fatalf("stopping lane selection error = %v", err)
	}
	stopping, stoppingPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = stoppingPeer.Close() })
	if _, err := empty.add(sessioncontract.LaneIdentity{ID: 92, Epoch: 1}, stopping, authenticator, false); !errors.Is(err, sessioncontract.ErrRuntimeClosed) {
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
	for _, test := range []struct {
		name string
		err  error
	}{
		{"native failure", errors.New("component failed")},
		{"unexpected cancellation", context.Canceled},
		{"joined cancellation", errors.Join(errors.New("component interrupted"), context.Canceled)},
		{"unexpected normal return", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, initialChannel := newUnstartedRuntime(t, protocolsession.RoleSender)
			observed := make(chan sessionobs.SenderSessionTerminated, 1)
			runtime.sessionTerminalObserver = sessionobs.SenderSessionTerminalObserverFunc(func(event sessionobs.SenderSessionTerminated) { observed <- event })
			runtime.start(Component{RuntimeFailureSourcePeer, func(context.Context) error { return test.err }})
			select {
			case <-runtime.Done():
			case <-time.After(time.Second):
				t.Fatal("runtime did not finish after a component failure")
			}
			event := awaitSenderSessionTermination(t, observed)
			nodes := event.Failure.Nodes()
			if !event.Failure.Present() || event.Failure.Source() != "peer" ||
				(test.err == nil && len(nodes) != 0) ||
				(test.err != nil && (len(nodes) == 0 || nodes[0].Message != test.err.Error())) {
				t.Fatalf("component failure detail = %#v", event.Failure)
			}
			if !errors.Is(runtime.Err(), test.err) {
				t.Fatalf("runtime retained error = %v", runtime.Err())
			}
			if initialChannel.State() != framechannel.Closed {
				t.Fatal("runtime finished before closing its physical lane")
			}
			runtime.close()
		})
	}
}

func TestOperationLaneRoutesPreserveFirstPhysicalAuthorityUntilFinal(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	routes := newOperationLaneRoutes()
	operationID := id16[protocolsession.OperationID](61)
	other := sessioncontract.LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if routes.reserve(protocolsession.OperationID{}, runtime.initial) != nil ||
		routes.reserve(operationID, sessioncontract.LaneIdentity{}) != nil {
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
	identity := sessioncontract.LaneIdentity{ID: 2, Epoch: 1}
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
	if _, err := nilLanes.addWithAdmission(identity, nil, nil, false, nil); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("nil lane registry error = %v", err)
	}
}

func newUnstartedRuntime(t *testing.T, role protocolsession.Role) (*Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithPolicy(t, role, protocolsession.OperationLimits{}, nil)
}

func newUnstartedRuntimeWithPolicy(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
) (*Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithContinuations(t, role, limits, now, nil)
}

func newUnstartedRuntimeWithContinuations(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
	continuations protocolsession.OperationContinuationClassifier,
) (*Runtime, *memoryChannel) {
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
	runtime, err := New(Config{
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
