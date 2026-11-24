package nativeconnectivity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/turnsession"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/receiversession"
	"github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/core/session/turngrant"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
)

type staticTURNProvider struct{ count int }

func (provider staticTURNProvider) Issue(_ context.Context, _ turngrant.IssueRequest) turngrant.IssueResult {
	result := turngrant.IssueResult{Status: protocolsession.TURNOK}
	for i := range provider.count {
		result.Credentials = append(result.Credentials, turngrant.Credential{
			ServiceID: fmt.Sprintf("turn-%d", i+1),
			URLs:      []string{"turn:127.0.0.1:3478"},
			Username:  "windshare",
			Password:  "secret",
		})
	}
	return result
}

type loopbackControl struct {
	kind protocolsession.MessageKind
	body []byte
}

func (control loopbackControl) Kind() protocolsession.MessageKind { return control.kind }
func (control loopbackControl) Body() []byte                      { return bytes.Clone(control.body) }

type loopbackOperationTracker struct {
	mu      sync.Mutex
	active  map[protocolsession.OperationID]bool
	current map[protocolsession.OperationID]bool
}

func newLoopbackOperationTracker() *loopbackOperationTracker {
	return &loopbackOperationTracker{
		active:  make(map[protocolsession.OperationID]bool),
		current: make(map[protocolsession.OperationID]bool),
	}
}

func (tracker *loopbackOperationTracker) admit(id protocolsession.OperationID) protocolsession.Operation {
	tracker.mu.Lock()
	tracker.active[id] = true
	tracker.current[id] = true
	tracker.mu.Unlock()
	return protocolsession.NewOperation(id, tracker)
}

func (tracker *loopbackOperationTracker) cancel(id protocolsession.OperationID) {
	tracker.mu.Lock()
	delete(tracker.active, id)
	tracker.mu.Unlock()
}

func (tracker *loopbackOperationTracker) Active(id protocolsession.OperationID) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.active[id]
}

func (tracker *loopbackOperationTracker) Current(id protocolsession.OperationID) bool {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return tracker.current[id]
}

func (tracker *loopbackOperationTracker) RequestKind(id protocolsession.OperationID) (protocolsession.MessageKind, bool) {
	if !tracker.Current(id) {
		return 0, false
	}
	return protocolsession.MessagePeerOffer, true
}

func (tracker *loopbackOperationTracker) PeerAttemptBinding(protocolsession.OperationID) (protocolsession.PeerAttemptBinding, bool) {
	return protocolsession.PeerAttemptBinding{}, false
}

func (tracker *loopbackOperationTracker) MaximumContinuations(protocolsession.OperationID) (int, bool) {
	return 0, false
}

type loopbackSignalingOp struct {
	bridge  *sameEngineTURNBridge
	op      protocolsession.Operation
	binding v2peer.ReceiverSignalingOperationBinding

	controls chan v2peer.ReceiverControl
	closed   chan struct{}
	once     sync.Once
}

func (operation *loopbackSignalingOp) OperationID() protocolsession.OperationID {
	return operation.op.ID()
}
func (operation *loopbackSignalingOp) MaximumContinuations() (int, bool) {
	return v2peer.DefaultMaxCandidates, true
}

func (operation *loopbackSignalingOp) SendCandidate(ctx context.Context, body []byte) (protocolsession.OperationDisposition, error) {
	id := operation.op.ID()
	message, err := protocolsession.NewMessage(protocolsession.MessagePeerCandidate, &id, bytes.Clone(body))
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if err := operation.bridge.handler.HandleMessage(ctx, operation.op, message); err != nil {
		return protocolsession.OperationDrop, err
	}
	return protocolsession.OperationDeliver, nil
}

func (operation *loopbackSignalingOp) Receive(ctx context.Context) v2peer.ReceiverSignalingReceiveResult {
	select {
	case <-ctx.Done():
		return v2peer.NewReceiverSignalingTerminationResult(
			v2peer.NewReceiverSignalingLocalTermination(operation.binding, ctx.Err()),
		)
	case <-operation.closed:
		return v2peer.NewReceiverSignalingTerminationResult(
			v2peer.NewReceiverSignalingLocalTermination(operation.binding, context.Canceled),
		)
	case control := <-operation.controls:
		return v2peer.NewReceiverSignalingControlResult(control)
	}
}

func (operation *loopbackSignalingOp) Terminate(ctx context.Context) v2peer.ReceiverSignalingTermination {
	operation.once.Do(func() {
		close(operation.closed)
		operation.bridge.tracker.cancel(operation.op.ID())
		_ = operation.bridge.handler.Cancel(ctx, operation.op)
	})
	return v2peer.NewReceiverSignalingLocalTermination(operation.binding, nil)
}

type sameEngineTURNBridge struct {
	share     catalog.ShareInstance
	sessionID protocolsession.ProtocolSessionID
	tracker   *loopbackOperationTracker
	handler   sendersession.PeerHandler

	mu          sync.Mutex
	turnHandler func(context.Context, []byte) error
	ops         map[protocolsession.OperationID]*loopbackSignalingOp
	nextOp      atomic.Uint32

	turnReplies chan []byte
	answers     chan protocolsession.OperationID
	failures    chan error
}

func newSameEngineTURNBridge() *sameEngineTURNBridge {
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{1}, catalog.IdentityBytes))
	sessionID, _ := protocolsession.ProtocolSessionIDFromBytes(bytes.Repeat([]byte{2}, protocolsession.IdentityBytes))
	return &sameEngineTURNBridge{
		share:       share,
		sessionID:   sessionID,
		tracker:     newLoopbackOperationTracker(),
		ops:         make(map[protocolsession.OperationID]*loopbackSignalingOp),
		turnReplies: make(chan []byte, 4),
		answers:     make(chan protocolsession.OperationID, protocolsession.MaxGrantsPerMessage*2),
		failures:    make(chan error, protocolsession.MaxGrantsPerMessage*2),
	}
}

func (bridge *sameEngineTURNBridge) ShareInstance() catalog.ShareInstance { return bridge.share }
func (bridge *sameEngineTURNBridge) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return bridge.sessionID
}
func (bridge *sameEngineTURNBridge) SetPeerPathControlHandler(func(context.Context, []byte) error) {}
func (bridge *sameEngineTURNBridge) SendPeerPathControl(context.Context, []byte) error             { return nil }
func (bridge *sameEngineTURNBridge) RetireReplacedLane(_, _ session.LaneIdentity) bool             { return false }

func (bridge *sameEngineTURNBridge) SetTURNControlHandler(handler func(context.Context, []byte) error) {
	bridge.mu.Lock()
	bridge.turnHandler = handler
	bridge.mu.Unlock()
}

func (bridge *sameEngineTURNBridge) SendTURNControl(_ context.Context, body []byte) error {
	bridge.turnReplies <- bytes.Clone(body)
	return nil
}

func (bridge *sameEngineTURNBridge) SendPeerControl(
	_ context.Context,
	op protocolsession.Operation,
	kind protocolsession.MessageKind,
	body []byte,
) (protocolsession.OperationDisposition, error) {
	bridge.mu.Lock()
	target := bridge.ops[op.ID()]
	bridge.mu.Unlock()
	if target == nil {
		return protocolsession.OperationDrop, nil
	}
	if kind == protocolsession.MessagePeerAnswer {
		bridge.answers <- op.ID()
	}
	select {
	case target.controls <- loopbackControl{kind: kind, body: bytes.Clone(body)}:
	default:
	}
	return protocolsession.OperationDeliver, nil
}

func (bridge *sameEngineTURNBridge) FailPeerOperation(_ context.Context, op protocolsession.Operation, code uint16, message string) error {
	bridge.failures <- fmt.Errorf("operation %v failed (%d): %s", op.ID(), code, message)
	return nil
}

func (bridge *sameEngineTURNBridge) AdmitPeerChannel(
	context.Context,
	protocolsession.FrameChannel,
	sendersession.PeerAdmissionControl,
) (sendersession.PeerAdmissionResult, error) {
	return sendersession.PeerAdmissionResult{}, errors.New("unexpected lane admission")
}

func (bridge *sameEngineTURNBridge) OpenPeerOperation(
	ctx context.Context,
	binding v2peer.ReceiverSignalingOperationBinding,
	offer []byte,
) (v2peer.ReceiverSignalingOperation, error) {
	id, err := protocolsession.OperationIDFromUint64(uint64(bridge.nextOp.Add(1)))
	if err != nil {
		return nil, err
	}
	op := bridge.tracker.admit(id)
	signalingOp := &loopbackSignalingOp{
		bridge:   bridge,
		op:       op,
		binding:  binding,
		controls: make(chan v2peer.ReceiverControl, 64),
		closed:   make(chan struct{}),
	}
	bridge.mu.Lock()
	bridge.ops[id] = signalingOp
	bridge.mu.Unlock()

	message, err := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &id, bytes.Clone(offer))
	if err != nil {
		return nil, err
	}
	if err := bridge.handler.HandleMessage(ctx, op, message); err != nil {
		return nil, err
	}
	return signalingOp, nil
}

type noopReceiverLanes struct {
	sessionID protocolsession.ProtocolSessionID
}

func (lanes noopReceiverLanes) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return lanes.sessionID
}
func (noopReceiverLanes) RequestLane(context.Context, uint32) (protocolsession.LaneGrant, error) {
	return protocolsession.LaneGrant{}, errors.New("unused in negotiation test")
}
func (noopReceiverLanes) AttachLane(
	context.Context,
	protocolsession.LaneGrant,
	protocolsession.FrameChannel,
	transferlanes.Route,
) (receiversession.LaneAdmissionResult, error) {
	return receiversession.LaneAdmissionResult{}, errors.New("unused in negotiation test")
}

func TestSameEngineFullLoadTURNNegotiationCompletesWithoutDeadlock(t *testing.T) {
	capacity := newTestCapacity(t)
	senderFactory, err := NewSender(SenderConfig{
		Capacity:     capacity,
		TURNProvider: staticTURNProvider{count: protocolsession.MaxGrantsPerMessage},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer senderFactory.NativeConnectivity().Close(context.Background())

	receiverNative, err := capacity.NewNative(nativepeer.Config{Side: nativepeer.SideReceiver})
	if err != nil {
		t.Fatal(err)
	}
	defer receiverNative.Close(context.Background())

	receiverFactory, err := NewReceiver(ReceiverConfig{Capacity: capacity, Native: receiverNative})
	if err != nil {
		t.Fatal(err)
	}

	bridge := newSameEngineTURNBridge()
	handler, err := senderFactory.NewSenderPeerHandler(bridge)
	if err != nil {
		t.Fatal(err)
	}
	bridge.handler = handler

	runCtx, stopHandler := context.WithCancel(t.Context())
	handlerDone := make(chan error, 1)
	go func() { handlerDone <- handler.Run(runCtx) }()
	stopAndJoin := sync.OnceFunc(func() {
		stopHandler()
		<-handlerDone
	})
	defer stopAndJoin()

	// Wait until Run registers the TURN control handler on the session.
	deadline := time.Now().Add(2 * time.Second)
	var turnHandler func(context.Context, []byte) error
	for turnHandler == nil {
		bridge.mu.Lock()
		turnHandler = bridge.turnHandler
		bridge.mu.Unlock()
		if turnHandler == nil {
			if time.Now().After(deadline) {
				t.Fatal("sender handler did not register TURN control handler")
			}
			time.Sleep(time.Millisecond)
		}
	}

	demandBody, err := protocolsession.EncodeTURNDemand(protocolsession.TURNDemand{
		RequestID: 1,
		Mode:      protocolsession.TURNAll,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := turnHandler(runCtx, demandBody); err != nil {
		t.Fatal(err)
	}
	var replyBody []byte
	select {
	case replyBody = <-bridge.turnReplies:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TURN grants")
	}
	grantsReply, err := protocolsession.DecodeTURNGrants(replyBody)
	if err != nil || len(grantsReply.Grants) != protocolsession.MaxGrantsPerMessage {
		t.Fatalf("grants reply = %+v, err = %v", grantsReply, err)
	}

	// Launch all four receiver TURN negotiations simultaneously against the
	// paired sender backed by the same Engine Capacity.
	type startResult struct {
		attempt *v2peer.ReceiverAttempt
		err     error
	}
	started := make(chan startResult, len(grantsReply.Grants))
	lanes := noopReceiverLanes{sessionID: bridge.sessionID}
	for i, grant := range grantsReply.Grants {
		binding := v2signal.Binding{
			Purpose:         v2signal.TURNRelay,
			PeerSlot:        uint8(i),
			AttemptSequence: 1,
		}
		binding.PeerPathID[0] = byte(i + 1)
		binding.AttemptID[0] = byte(i + 10)
		go func(b v2signal.Binding, g protocolsession.TURNGrant) {
			attempt, startErr := receiverFactory.StartTURNBinding(runCtx, bridge, lanes, b, g)
			started <- startResult{attempt: attempt, err: startErr}
		}(binding, grant)
	}

	attempts := make([]*v2peer.ReceiverAttempt, 0, len(grantsReply.Grants))
	for range grantsReply.Grants {
		select {
		case res := <-started:
			if res.err != nil {
				t.Fatalf("start TURN receiver attempt: %v", res.err)
			}
			attempts = append(attempts, res.attempt)
		case <-time.After(3 * time.Second):
			t.Fatal("timed out starting full-load receiver TURN attempts")
		}
	}
	closedAttempts := false
	defer func() {
		if !closedAttempts {
			for _, attempt := range attempts {
				_ = attempt.Close()
			}
		}
	}()

	// All four paired local sender attempts must answer well before the 10s
	// signaling preparation timeout instead of deadlocking on receiver permits.
	answered := make(map[protocolsession.OperationID]bool, len(grantsReply.Grants))
	answerDeadline := time.After(3 * time.Second)
	for len(answered) < len(grantsReply.Grants) {
		select {
		case opID := <-bridge.answers:
			answered[opID] = true
		case failErr := <-bridge.failures:
			t.Fatalf("paired local TURN sender rejected offer: %v", failErr)
		case <-answerDeadline:
			t.Fatalf("deadlock: only %d/%d paired TURN offers received answers", len(answered), len(grantsReply.Grants))
		}
	}

	// Both directional pools must genuinely be at full load (4/4) while all four
	// paired negotiations remain unattached.
	probePoolFull := func(name string, pool *turnsession.Capacity) {
		probeCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		release, err := pool.Acquire(probeCtx)
		if err == nil {
			release()
			t.Fatalf("%s pool was not at full capacity during 4-way negotiation", name)
		}
	}
	probePoolFull("receiver", capacity.ReceiverTURN())
	probePoolFull("sender", capacity.SenderTURN())

	for _, attempt := range attempts {
		_ = attempt.Close()
	}
	closedAttempts = true
	stopAndJoin()

	// All permits on both sides must return cleanly after shutdown.
	for _, pair := range []struct {
		name string
		pool *turnsession.Capacity
	}{
		{"receiver", capacity.ReceiverTURN()},
		{"sender", capacity.SenderTURN()},
	} {
		var releases []func()
		for i := range protocolsession.MaxGrantsPerMessage {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			release, err := pair.pool.Acquire(ctx)
			cancel()
			if err != nil {
				t.Fatalf("%s permit %d leaked after teardown: %v", pair.name, i+1, err)
			}
			releases = append(releases, release)
		}
		for _, release := range releases {
			release()
		}
	}
}
