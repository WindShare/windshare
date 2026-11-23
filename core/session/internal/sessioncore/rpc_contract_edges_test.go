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
	}{runtime, NewRequests(runtime, nil)}
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
	exhaustedIDs := NewRequests(runtime, nil)
	exhaustedIDs.ids.counter = ^uint64(0)
	if _, err := exhaustedIDs.Begin(context.Background(), protocolsession.MessageListChildren, body); !errors.Is(err, ErrOperationIDExhausted) {
		t.Fatalf("exhausted operation IDs error = %v", err)
	}
	invalidMessage := NewRequests(runtime, nil)
	if _, err := invalidMessage.Begin(context.Background(), protocolsession.MessageKind(255), body); err == nil {
		t.Fatal("invalid operation kind was accepted")
	}
	duplicate := NewRequests(runtime, nil)
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

func TestDelayedFirstRequestBelowReplayWindowRepliesRetryWithoutEndingSession(t *testing.T) {
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
	keys, err := protocolsession.DeriveSenderSession(senderPrivate, authKey, client, server)
	if err != nil {
		t.Fatal(err)
	}
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
			return responses.SendControl(ctx, op, protocolsession.MessageCatalogResult, catalogBody)
		},
	)); err != nil {
		t.Fatal(err)
	}
	if err := sender.inbound.RegisterHandler(protocolsession.MessageRequestBlocks, protocolsession.MessageHandlerFunc(
		func(ctx context.Context, op protocolsession.Operation, _ protocolsession.Message) error {
			return responses.SendControl(ctx, op, protocolsession.MessageOperationComplete, completeBody)
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

	makePeerLane := func(identity session.LaneIdentity) (*protocolsession.EnvelopeSealer, *protocolsession.EnvelopeOpener) {
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
	lane1Sealer, lane1Opener := makePeerLane(sender.initial)
	lane2Sealer, lane2Opener := makePeerLane(lane2ID)

	sendOn := func(ch *memoryChannel, sealer *protocolsession.EnvelopeSealer, kind protocolsession.MessageKind, id protocolsession.OperationID, body []byte) {
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
	recvOn := func(ch *memoryChannel, opener *protocolsession.EnvelopeOpener) protocolsession.Message {
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

	// Advance sender's replay window past (1 + OperationReplayWindow) on lane 2 with at most 2 ops.
	highID := protocolsession.OperationID(1 + protocolsession.OperationReplayWindow)
	sendOn(lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, 2, []byte{0xa0})
	if got := recvOn(lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("lane 2 op 2 response kind = %d", got.Kind())
	}
	sendOn(lane2Peer, lane2Sealer, protocolsession.MessageRequestBlocks, highID, []byte{0xa0})
	if got := recvOn(lane2Peer, lane2Opener); got.Kind() != protocolsession.MessageOperationComplete {
		t.Fatalf("lane 2 op %d response kind = %d", highID, got.Kind())
	}

	// Deliver the delayed first catalog request (id = 1) on lane 1.
	sendOn(lane1Peer, lane1Sealer, protocolsession.MessageListChildren, 1, []byte{0xa0})
	retryReply := recvOn(lane1Peer, lane1Opener)
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
	sendOn(lane1Peer, lane1Sealer, protocolsession.MessageListChildren, freshID, []byte{0xa0})
	freshReply := recvOn(lane1Peer, lane1Opener)
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
	sendOn(lane2Peer, lane2Sealer, protocolsession.MessageListChildren, freshID, []byte{0xa1, 0x00, 0x01})
	select {
	case <-sender.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("proven in-window duplicate did not terminate sender session")
	}
	if !errors.Is(sender.Err(), protocolsession.ErrOperationIDReused) {
		t.Fatalf("proven duplicate termination err = %v, want ErrOperationIDReused", sender.Err())
	}
}
