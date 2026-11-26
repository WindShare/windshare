package sessioncore

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestRPCClientBoundsLifecycleAndCancellation(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	runtime.Start()
	defer runtime.Close()
	receiver := struct {
		*Runtime
		rpc *Requests
	}{runtime, NewRequests(runtime)}
	if err := receiver.rpc.Register(); err != nil {
		t.Fatal(err)
	}
	defer receiver.rpc.Close()
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	terminal, _ := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err := receiver.rpc.HandleMessage(context.Background(), protocolsession.Operation{}, terminal); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("message without operation error = %v", err)
	}
	missingID := protocolsession.OperationID(71)
	missing, _ := protocolsession.NewMessage(protocolsession.MessageCatalogResult, &missingID, body)
	if err := receiver.rpc.HandleMessage(context.Background(), protocolsession.Operation{}, missing); err != nil {
		t.Fatalf("late response error = %v", err)
	}
	bounded := &Call{id: missingID, messages: make(chan operationResponse, 1)}
	seedRequest, _ := protocolsession.NewMessage(protocolsession.MessageListChildren, &missingID, body)
	op, maxCont, hasLimit, release, err := receiver.operations.AdmitOutboundRequest(seedRequest, nil)
	if err != nil || !bounded.setAuthority(op, maxCont, hasLimit) {
		t.Fatalf("seed bounded call authority: %v", err)
	}
	release(false)
	receiver.rpc.mu.Lock()
	receiver.rpc.calls[missingID] = bounded
	receiver.rpc.mu.Unlock()
	if err := receiver.rpc.HandleMessage(context.Background(), op, missing); err != nil {
		t.Fatal(err)
	}
	if err := receiver.rpc.HandleMessage(context.Background(), op, missing); !errors.Is(err, session.ErrOperationOverflow) {
		t.Fatalf("response overflow error = %v", err)
	}
	wrong := &Call{id: missingID, messages: make(chan operationResponse, 1)}
	receiver.rpc.End(wrong)
	receiver.rpc.End(nil)
	receiver.rpc.End(bounded)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.rpc.Begin(cancelled, protocolsession.MessageListChildren, body); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin cancelled error = %v", err)
	}
	exhaustedIDs := NewRequests(runtime)
	exhaustedIDs.ids.counter = ^uint64(0)
	if _, err := exhaustedIDs.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, ErrOperationIDExhausted) {
		t.Fatalf("exhausted operation IDs error = %v", err)
	}
	invalidMessage := NewRequests(runtime)
	if _, err := invalidMessage.Begin(context.Background(), protocolsession.MessageKind(255), body); err == nil {
		t.Fatal("invalid operation kind was accepted")
	}
	duplicate := NewRequests(runtime)
	duplicateID := protocolsession.OperationID(1)
	duplicate.calls[duplicateID] = &Call{id: duplicateID}
	if _, err := duplicate.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("duplicate local operation error = %v", err)
	}
	if err := receiver.rpc.Register(); err == nil {
		t.Fatal("duplicate router registration was accepted")
	}
	if err := receiver.rpc.admitCancellation(nil, contentflow.CancelReasonUser); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("nil cancellation call error = %v", err)
	}
	if err := receiver.rpc.admitCancellation(
		&Call{id: missingID}, contentflow.CancelReason(0),
	); !errors.Is(err, session.ErrOperationMissing) {
		t.Fatalf("missing cancellation authority error = %v", err)
	}

	requestBody := body
	call, err := receiver.rpc.Begin(context.Background(), protocolsession.MessageListChildren, requestBody)
	if err != nil {
		t.Fatal(err)
	}

	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	if _, err := receiver.rpc.Await(waitContext, call); !errors.Is(err, context.Canceled) {
		t.Fatalf("await cancellation = %v", err)
	}
	receiver.rpc.End(call)

	doneCall := &Call{messages: make(chan operationResponse)}
	runtime.Close()
	if _, err := receiver.rpc.Await(context.Background(), doneCall); !errors.Is(err, session.ErrRuntimeClosed) {
		t.Fatalf("await closed runtime = %v", err)
	}
}

func newTestSenderSessionKeys(t *testing.T, seed byte) (catalog.ShareInstance, protocolsession.SessionKeys) {
	t.Helper()
	share := id16[catalog.ShareInstance](seed)
	authKey := bytes.Repeat([]byte{seed + 1}, protocolsession.SessionAuthKeyBytes)
	receiverPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: seed + 2})
	if err != nil {
		t.Fatal(err)
	}
	client, err := protocolsession.NewClientHello(
		share, id16[protocolsession.ReceiverInstanceID](seed+3),
		bytes.Repeat([]byte{seed + 4}, protocolsession.HandshakeNonceBytes), receiverPrivate.PublicKey(), authKey,
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
	senderPrivate, err := ecdh.X25519().GenerateKey(&deterministicReader{next: seed + 5})
	if err != nil {
		t.Fatal(err)
	}
	signingKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed + 6}, ed25519.SeedSize))
	server, err := protocolsession.NewServerHello(
		client, bytes.Repeat([]byte{seed + 7}, protocolsession.HandshakeNonceBytes),
		senderPrivate.PublicKey(), 1, signingKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := protocolsession.DeriveSenderSession(senderPrivate, authKey, client, server)
	if err != nil {
		t.Fatal(err)
	}
	return share, keys
}

func makePeerLaneCrypto(
	t *testing.T,
	sender *Runtime,
	identity session.LaneIdentity,
) (*protocolsession.EnvelopeSealer, *protocolsession.EnvelopeOpener) {
	t.Helper()
	outBind := protocolsession.EnvelopeBinding{
		ShareInstance: sender.share, ProtocolSessionID: sender.sessionID,
		LaneID: identity.ID, LaneEpoch: identity.Epoch, Direction: protocolsession.DirectionReceiverToSender,
	}
	inBind := outBind
	inBind.Direction = protocolsession.DirectionSenderToReceiver
	sealer, err := protocolsession.NewEnvelopeSealer(sender.keys.ReceiverToSender(), outBind, &deterministicReader{next: byte(identity.ID + 10)})
	if err != nil {
		t.Fatal(err)
	}
	opener, err := protocolsession.NewEnvelopeOpener(sender.keys.SenderToReceiver(), inBind)
	if err != nil {
		t.Fatal(err)
	}
	return sealer, opener
}

func sendSealedPeerMessage(
	t *testing.T,
	ch *memoryChannel,
	sealer *protocolsession.EnvelopeSealer,
	kind protocolsession.MessageKind,
	id protocolsession.OperationID,
	body []byte,
) {
	t.Helper()
	msg, err := protocolsession.NewMessage(kind, &id, body)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := protocolsession.EncodeMessage(msg)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealer.Seal(pt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), sealed.Frame); err != nil {
		t.Fatal(err)
	}
}

func recvSealedPeerMessage(
	t *testing.T,
	ch *memoryChannel,
	opener *protocolsession.EnvelopeOpener,
) protocolsession.Message {
	t.Helper()
	select {
	case frame := <-ch.Recv():
		opened, err := opener.Open(frame)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := protocolsession.DecodeMessage(opened.Plaintext)
		if err != nil {
			t.Fatal(err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for sender response")
		return protocolsession.Message{}
	}
}

func saturateLaneControlQueue(t *testing.T, lane *RuntimeLane, blocker *blockingRuntimeLaneChannel) {
	t.Helper()
	body, err := protocolsession.EncodeOperationRetry(protocolsession.OperationRetryReasonReplayWindow)
	if err != nil {
		t.Fatal(err)
	}
	fillID := protocolsession.OperationID(999_999)
	filler, err := protocolsession.NewMessage(protocolsession.MessageOperationRetry, &fillID, body)
	if err != nil {
		t.Fatal(err)
	}
	// First control frame enters Send and parks the writer on blocker.
	if err := lane.writer.EnqueueControl(filler, nil); err != nil {
		t.Fatalf("seed blocking control frame: %v", err)
	}
	select {
	case <-blocker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lane writer to block on transport")
	}
	for i := range protocolsession.ControlQueueFrameLimit {
		if err := lane.writer.EnqueueControl(filler, nil); err != nil {
			t.Fatalf("fill control queue slot %d: %v", i, err)
		}
	}
	if err := lane.writer.EnqueueControl(filler, nil); !errors.Is(err, protocolsession.ErrControlQueueFull) {
		t.Fatalf("expected full control queue, got %v", err)
	}
}

func TestDelayedFirstRequestBelowReplayWindowRepliesRetryWithoutEndingSession(t *testing.T) {
	share, keys := newTestSenderSessionKeys(t, 91)
	lane1SenderCh, lane1Peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = lane1SenderCh.Close()
		_ = lane1Peer.Close()
	})
	sender, err := New(Config{
		Share: share, Role: protocolsession.RoleSender, Keys: keys, LaneID: 1, Channel: lane1SenderCh,
		Random: &deterministicReader{next: 99}, Validator: permissiveInboundAuthenticator(),
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	keys.Destroy()
	recorder := newProtocolTraceRecorder(sender)

	responses := NewResponses(sender, ResponseConfig{})
	catalogBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: []byte{0xaa}})
	completeBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: uint64(0)})
	if err := sender.inbound.RegisterHandler(protocolsession.MessageListChildren, protocolsession.MessageHandlerFunc(
		func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			_, err := responses.SendControl(ctx, op, protocolsession.MessageCatalogResult, catalogBody)
			return err
		},
	)); err != nil {
		t.Fatal(err)
	}
	if err := sender.inbound.RegisterHandler(protocolsession.MessageRequestBlocks, protocolsession.MessageHandlerFunc(
		func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			_, err := responses.SendControl(ctx, op, protocolsession.MessageOperationComplete, completeBody)
			return err
		},
	)); err != nil {
		t.Fatal(err)
	}
	lane2SenderCh, lane2Peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = lane2SenderCh.Close()
		_ = lane2Peer.Close()
	})
	lane2ID := session.LaneIdentity{ID: sender.initial.ID + 1, Epoch: 1}
	if _, err := sender.lanes.add(lane2ID, lane2SenderCh, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	sender.Start()
	defer sender.Close()

	lane1Sealer, lane1Opener := makePeerLaneCrypto(t, sender, sender.initial)
	lane2Sealer, lane2Opener := makePeerLaneCrypto(t, sender, lane2ID)

	// Advance sender's replay window past (1 + OperationReplayWindow) on lane 2 with at most 2 ops.
	highID := protocolsession.OperationID(1 + protocolsession.OperationReplayWindow)
	sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, 2, []byte{0xa0})
	if got := recvSealedPeerMessage(t, lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("lane 2 op 2 response kind = %d", got.Kind())
	}
	sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, highID, []byte{0xa0})
	if got := recvSealedPeerMessage(t, lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("lane 2 op %d response kind = %d", highID, got.Kind())
	}

	// Deliver the delayed first catalog request (id = 1) on lane 1.
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageListChildren, 1, []byte{0xa0})
	retryReply := recvSealedPeerMessage(t, lane1Peer, lane1Opener)
	replyID, _ := retryReply.OperationID()
	if retryReply.Kind() != protocolsession.MessageOperationRetry || replyID != 1 {
		t.Fatalf("delayed op 1 reply = (kind=%d, id=%d), want (MessageOperationRetry, 1)", retryReply.Kind(), replyID)
	}
	reason, err := protocolsession.DecodeOperationRetry(retryReply.Body())
	if err != nil || reason != protocolsession.OperationRetryReasonReplayWindow {
		t.Fatalf("retry reason = (%d, %v)", reason, err)
	}
	if sender.Err() != nil || sender.AttachedLanes() != 2 {
		t.Fatalf("below-window request ended session: err=%v lanes=%d", sender.Err(), sender.AttachedLanes())
	}

	// Reissuing the catalog request with a fresh OperationID succeeds on the same healthy session.
	freshID := highID + 1
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageListChildren, freshID, []byte{0xa0})
	freshReply := recvSealedPeerMessage(t, lane1Peer, lane1Opener)
	if freshReply.Kind() != protocolsession.MessageCatalogResult {
		t.Fatalf("fresh retry reply kind = %d, want MessageCatalogResult", freshReply.Kind())
	}

	// Verify structured trace recorded the below-window event.
	foundBelowWindowTrace := false
	for _, opObs := range recorder.snapshot() {
		if opObs.Stage == sessionobs.ProtocolOperationSenderRequestBelowWindow &&
			opObs.OperationID == 1 &&
			opObs.Cause == sessionobs.ProtocolOperationCauseReplayWindow {
			foundBelowWindowTrace = true
			break
		}
	}
	if !foundBelowWindowTrace {
		t.Fatal("missing ProtocolOperationSenderRequestBelowWindow structured trace")
	}

	// Proven in-window duplicate (conflicting body on freshID) must still terminate the session.
	sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageListChildren, freshID, []byte{0xa1, 0x00, 0x01})
	select {
	case <-sender.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("proven in-window duplicate did not terminate sender session")
	}
	if !errors.Is(sender.Err(), protocolsession.ErrOperationIDReused) {
		t.Fatalf("proven duplicate termination err = %v, want ErrOperationIDReused", sender.Err())
	}
}

func TestBelowReplayWindowRetryFallbackSkipsFullLanesAndDropsWhenAllFull(t *testing.T) {
	t.Run("skips_saturated_primary_and_second_lane_to_deliver_on_open_lane", func(t *testing.T) {
		share, keys := newTestSenderSessionKeys(t, 111)
		lane1Base, lane1Peer := newMemoryChannelPair()
		lane2Base, lane2Peer := newMemoryChannelPair()
		lane3SenderCh, lane3Peer := newMemoryChannelPair()
		t.Cleanup(func() {
			_ = lane1Base.Close()
			_ = lane1Peer.Close()
			_ = lane2Base.Close()
			_ = lane2Peer.Close()
			_ = lane3SenderCh.Close()
			_ = lane3Peer.Close()
		})
		lane1Blocking := &blockingRuntimeLaneChannel{
			memoryChannel: lane1Base, started: make(chan struct{}), release: make(chan struct{}),
		}
		lane2Blocking := &blockingRuntimeLaneChannel{
			memoryChannel: lane2Base, started: make(chan struct{}), release: make(chan struct{}),
		}

		sender, err := New(Config{
			Share: share, Role: protocolsession.RoleSender, Keys: keys, LaneID: 1, Channel: lane1Blocking,
			Random: &deterministicReader{next: 119}, Validator: permissiveInboundAuthenticator(),
		})
		if err != nil {
			keys.Destroy()
			t.Fatal(err)
		}
		keys.Destroy()
		recorder := newProtocolTraceRecorder(sender)

		responses := NewResponses(sender, ResponseConfig{})
		completeBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: uint64(0)})
		if err := sender.inbound.RegisterHandler(protocolsession.MessageRequestBlocks, protocolsession.MessageHandlerFunc(
			func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
				_, err := responses.SendControl(ctx, op, protocolsession.MessageOperationComplete, completeBody)
				return err
			},
		)); err != nil {
			t.Fatal(err)
		}

		lane2ID := session.LaneIdentity{ID: 2, Epoch: 1}
		lane3ID := session.LaneIdentity{ID: 3, Epoch: 1}
		lane2, err := sender.lanes.add(lane2ID, lane2Blocking, permissiveInboundAuthenticator(), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sender.lanes.add(lane3ID, lane3SenderCh, permissiveInboundAuthenticator(), false); err != nil {
			t.Fatal(err)
		}
		lane1 := sender.lanes.active[sender.initial.ID]
		sender.Start()
		defer sender.Close()

		lane1Sealer, _ := makePeerLaneCrypto(t, sender, sender.initial)
		lane3Sealer, lane3Opener := makePeerLaneCrypto(t, sender, lane3ID)

		// Advance the replay window on lane 3 so op 1 falls below the window.
		highID := protocolsession.OperationID(1 + protocolsession.OperationReplayWindow)
		sendSealedPeerMessage(t, lane3Peer, lane3Sealer, protocolsession.MessageRequestBlocks, highID, []byte{0xa0})
		if got := recvSealedPeerMessage(t, lane3Peer, lane3Opener); got.Kind() != protocolsession.MessageOperationComplete {
			t.Fatalf("lane 3 op %d response kind = %d", highID, got.Kind())
		}

		// Saturate both the inbound lane (lane 1) and the next lane (lane 2).
		saturateLaneControlQueue(t, lane1, lane1Blocking)
		saturateLaneControlQueue(t, lane2, lane2Blocking)

		// Point round-robin cursor directly at lane 1 so single-pick fallback would re-select it.
		sender.lanes.mu.Lock()
		sender.lanes.nextSlot = 0
		sender.lanes.mu.Unlock()

		sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageListChildren, 1, []byte{0xa0})
		retryReply := recvSealedPeerMessage(t, lane3Peer, lane3Opener)
		replyID, _ := retryReply.OperationID()
		if retryReply.Kind() != protocolsession.MessageOperationRetry || replyID != 1 {
			t.Fatalf("fallback reply on lane 3 = (kind=%d, id=%d), want (MessageOperationRetry, 1)", retryReply.Kind(), replyID)
		}
		reason, err := protocolsession.DecodeOperationRetry(retryReply.Body())
		if err != nil || reason != protocolsession.OperationRetryReasonReplayWindow {
			t.Fatalf("fallback retry reason = (%d, %v)", reason, err)
		}

		foundFallbackTrace := false
		for _, obs := range recorder.snapshot() {
			if obs.Stage == sessionobs.ProtocolOperationSenderRequestBelowWindow &&
				obs.OperationID == 1 &&
				obs.Lane == lane3ID &&
				obs.SendStatus == sessionobs.SendStatusConfirmed {
				foundFallbackTrace = true
				break
			}
		}
		if !foundFallbackTrace {
			t.Fatalf("missing confirmed fallback trace on lane 3; events=%+v", recorder.snapshot())
		}
		if sender.Err() != nil {
			t.Fatalf("session failed unexpectedly: %v", sender.Err())
		}
	})

	t.Run("all_control_queues_full_drops_without_stalling_inbound_pump", func(t *testing.T) {
		share, keys := newTestSenderSessionKeys(t, 131)
		lane1Base, lane1Peer := newMemoryChannelPair()
		lane2Base, lane2Peer := newMemoryChannelPair()
		t.Cleanup(func() {
			_ = lane1Base.Close()
			_ = lane1Peer.Close()
			_ = lane2Base.Close()
			_ = lane2Peer.Close()
		})
		lane1Blocking := &blockingRuntimeLaneChannel{
			memoryChannel: lane1Base, started: make(chan struct{}), release: make(chan struct{}),
		}
		lane2Blocking := &blockingRuntimeLaneChannel{
			memoryChannel: lane2Base, started: make(chan struct{}), release: make(chan struct{}),
		}

		sender, err := New(Config{
			Share: share, Role: protocolsession.RoleSender, Keys: keys, LaneID: 1, Channel: lane1Blocking,
			Random: &deterministicReader{next: 139}, Validator: permissiveInboundAuthenticator(),
		})
		if err != nil {
			keys.Destroy()
			t.Fatal(err)
		}
		keys.Destroy()
		recorder := newProtocolTraceRecorder(sender)

		admitted := make(chan protocolsession.OperationID, 2)
		if err := sender.inbound.RegisterHandler(protocolsession.MessageRequestBlocks, protocolsession.MessageHandlerFunc(
			func(_ context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
				admitted <- op.ID()
				return nil
			},
		)); err != nil {
			t.Fatal(err)
		}

		lane2ID := session.LaneIdentity{ID: 2, Epoch: 1}
		lane2, err := sender.lanes.add(lane2ID, lane2Blocking, permissiveInboundAuthenticator(), false)
		if err != nil {
			t.Fatal(err)
		}
		lane1 := sender.lanes.active[sender.initial.ID]
		sender.Start()
		defer sender.Close()

		lane1Sealer, _ := makePeerLaneCrypto(t, sender, sender.initial)
		lane2Sealer, _ := makePeerLaneCrypto(t, sender, lane2ID)

		highID := protocolsession.OperationID(1 + protocolsession.OperationReplayWindow)
		sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, highID, []byte{0xa0})
		select {
		case gotID := <-admitted:
			if gotID != highID {
				t.Fatalf("admitted highID = %d, want %d", gotID, highID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for highID admission")
		}

		saturateLaneControlQueue(t, lane1, lane1Blocking)
		saturateLaneControlQueue(t, lane2, lane2Blocking)

		// Deliver a below-window request followed immediately by a fresh request on lane 1.
		// Dropping the retry hint must not block lane 1's inbound pump from delivering freshID.
		sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageListChildren, 1, []byte{0xa0})
		freshID := highID + 1
		sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageRequestBlocks, freshID, []byte{0xa0})
		select {
		case gotID := <-admitted:
			if gotID != freshID {
				t.Fatalf("admitted freshID = %d, want %d", gotID, freshID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("inbound pump stalled after full-queue below-window retry drop")
		}

		foundNotSent := false
		for _, obs := range recorder.snapshot() {
			if obs.Stage == sessionobs.ProtocolOperationSenderRequestBelowWindow &&
				obs.OperationID == 1 &&
				obs.Lane == sender.initial &&
				obs.SendStatus == sessionobs.SendStatusNotSent {
				foundNotSent = true
				break
			}
		}
		if !foundNotSent {
			t.Fatalf("missing SendStatusNotSent below-window trace; events=%+v", recorder.snapshot())
		}
		if sender.Err() != nil {
			t.Fatalf("session failed unexpectedly after best-effort drop: %v", sender.Err())
		}
	})
}

func TestDeferredPromotionBelowReplayWindowRepliesRetryAndDropsTrickledCandidate(t *testing.T) {
	share, keys := newTestSenderSessionKeys(t, 151)
	lane1SenderCh, lane1Peer := newMemoryChannelPair()
	lane2SenderCh, lane2Peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = lane1SenderCh.Close()
		_ = lane1Peer.Close()
		_ = lane2SenderCh.Close()
		_ = lane2Peer.Close()
	})
	sender, err := New(Config{
		Share:           share,
		Role:            protocolsession.RoleSender,
		Keys:            keys,
		LaneID:          1,
		Channel:         lane1SenderCh,
		Random:          &deterministicReader{next: 159},
		Validator:       permissiveInboundAuthenticator(),
		Continuations:   peerSlotTestClassifier{},
		OperationLimits: protocolsession.OperationLimits{MaxActive: 1, MaxRecentTombstones: 16},
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	keys.Destroy()
	recorder := newProtocolTraceRecorder(sender)
	responses := NewResponses(sender, ResponseConfig{})
	completeBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1), 1: uint64(0)})

	gateStarted := make(chan struct{})
	releaseGate := make(chan struct{})
	highAdmitted := make(chan struct{})
	freshAdmitted := make(chan protocolsession.OperationID, 1)
	highID := protocolsession.OperationID(1 + protocolsession.OperationReplayWindow)
	if err := sender.inbound.RegisterHandler(protocolsession.MessageRequestBlocks, protocolsession.MessageHandlerFunc(
		func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			switch op.ID() {
			case 500:
				close(gateStarted)
				<-releaseGate
			case highID:
				close(highAdmitted)
			default:
				freshAdmitted <- op.ID()
			}
			_, sendErr := responses.SendControl(ctx, op, protocolsession.MessageOperationComplete, completeBody)
			return sendErr
		},
	)); err != nil {
		t.Fatal(err)
	}
	if err := sender.inbound.RegisterHandler(protocolsession.MessagePeerOffer, protocolsession.MessageHandlerFunc(
		func(context.Context, protocolsession.Operation, protocolsession.Message) error {
			return errors.New("unexpected delivery of below-window deferred peer offer")
		},
	)); err != nil {
		t.Fatal(err)
	}
	if err := sender.inbound.RegisterHandler(protocolsession.MessagePeerCandidate, protocolsession.MessageHandlerFunc(
		func(context.Context, protocolsession.Operation, protocolsession.Message) error {
			return errors.New("unexpected delivery of below-window deferred peer candidate")
		},
	)); err != nil {
		t.Fatal(err)
	}

	lane2ID := session.LaneIdentity{ID: 2, Epoch: 1}
	if _, err := sender.lanes.add(lane2ID, lane2SenderCh, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	sender.Start()
	defer sender.Close()

	lane1Sealer, lane1Opener := makePeerLaneCrypto(t, sender, sender.initial)
	lane2Sealer, lane2Opener := makePeerLaneCrypto(t, sender, lane2ID)

	// Occupy the single active operation slot (MaxActive=1) with id=500.
	sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, 500, []byte{0xa0})
	select {
	case <-gateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for active gating request")
	}

	// Queue highID ahead of delayed op 1 in deferredOrder while highWater is still 500,
	// so op 1 (PeerOffer + trickled PeerCandidate) is Fresh on arrival and deferred behind highID.
	sendSealedPeerMessage(t, lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, highID, []byte{0xa0})
	deadline := time.Now().Add(2 * time.Second)
	for {
		sender.inbound.ingressMu.Lock()
		queuedHigh := len(sender.inbound.deferred[highID]) > 0
		sender.inbound.ingressMu.Unlock()
		if queuedHigh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for highID to enter deferred queue")
		}
		time.Sleep(time.Millisecond)
	}

	binding := protocolsession.PeerAttemptBinding{
		PeerSlot: 1, PeerPathID: [16]byte{7}, AttemptID: [16]byte{8}, AttemptSequence: 1,
	}
	bindingBody := encodeTestPeerBindingBody(t, binding)
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessagePeerOffer, 1, bindingBody)
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessagePeerCandidate, 1, bindingBody)

	deadline = time.Now().Add(2 * time.Second)
	for {
		sender.inbound.ingressMu.Lock()
		queuedDelayed := len(sender.inbound.deferred[1]) == 2
		sender.inbound.ingressMu.Unlock()
		if queuedDelayed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for delayed offer + candidate to enter deferred queue")
		}
		time.Sleep(time.Millisecond)
	}

	// Release id=500 so promotion advances highWater via highID and then promotes op 1 below window.
	close(releaseGate)
	if got := recvSealedPeerMessage(t, lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("gate op 500 reply kind = %d", got.Kind())
	}
	select {
	case <-highAdmitted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for promoted highID")
	}
	if got := recvSealedPeerMessage(t, lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("highID reply kind = %d", got.Kind())
	}

	// Deferred promotion must emit MessageOperationRetry for op 1 and drop its trickled candidate.
	retryReply := recvSealedPeerMessage(t, lane1Peer, lane1Opener)
	replyID, _ := retryReply.OperationID()
	if retryReply.Kind() != protocolsession.MessageOperationRetry || replyID != 1 {
		t.Fatalf("deferred below-window reply = (kind=%d, id=%d), want (MessageOperationRetry, 1)", retryReply.Kind(), replyID)
	}

	// A follow-up candidate arriving after promotion must also drop without killing the session.
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessagePeerCandidate, 1, bindingBody)
	freshID := highID + 1
	sendSealedPeerMessage(t, lane1Peer, lane1Sealer, protocolsession.MessageRequestBlocks, freshID, []byte{0xa0})
	select {
	case gotID := <-freshAdmitted:
		if gotID != freshID {
			t.Fatalf("admitted freshID = %d, want %d", gotID, freshID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session failed to serve fresh request after deferred below-window promotion")
	}
	if got := recvSealedPeerMessage(t, lane1Peer, lane1Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("freshID reply kind = %d", got.Kind())
	}

	foundTrace := false
	for _, obs := range recorder.snapshot() {
		if obs.Stage == sessionobs.ProtocolOperationSenderRequestBelowWindow &&
			obs.OperationID == 1 &&
			obs.SendStatus == sessionobs.SendStatusConfirmed &&
			obs.Cause == sessionobs.ProtocolOperationCauseReplayWindow {
			foundTrace = true
			break
		}
	}
	if !foundTrace {
		t.Fatalf("missing ProtocolOperationSenderRequestBelowWindow trace after deferred promotion; events=%+v", recorder.snapshot())
	}
	if sender.Err() != nil {
		t.Fatalf("session ended unexpectedly: %v", sender.Err())
	}
}
