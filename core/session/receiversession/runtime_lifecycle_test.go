package receiversession

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session"

	"github.com/windshare/windshare/core/session/internal/sessioncore"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestLaneAttachmentBoundarySilentlyClosesUntrustedFailures(t *testing.T) {
	fixture := newVerticalFixture(t)
	random := &switchableReader{reader: fixture.senderConfig.Random}
	fixture.senderConfig.Random = random
	fixture.rebuildSender(t)
	if _, err := (*sendersession.Factory)(nil).Attach(context.Background(), newMemoryChannel(t)); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("nil attach factory error = %v", err)
	}
	if _, err := fixture.senderFactory.Attach(context.Background(), nil); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("nil attach channel error = %v", err)
	}

	closed, closedPeer := newMemoryChannelPair()
	_ = closed.Close()
	t.Cleanup(func() { _ = closedPeer.Close() })
	if _, err := fixture.senderFactory.Attach(context.Background(), closed); !errors.Is(err, session.ErrHandshake) {
		t.Fatalf("closed candidate error = %v", err)
	}
	malformed, malformedPeer := newMemoryChannelPair()
	_ = malformedPeer.Send(context.Background(), framechannel.Frame{1})
	if _, err := fixture.senderFactory.Attach(context.Background(), malformed); !errors.Is(err, session.ErrHandshake) {
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
	if _, err := fixture.senderFactory.Attach(context.Background(), unknown); !errors.Is(err, session.ErrHandshake) {
		t.Fatalf("unknown session candidate error = %v", err)
	}
	if unknown.State() != framechannel.Closed {
		t.Fatal("unknown session candidate remained open")
	}
	_ = unknownPeer.Close()

	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	if _, err := (*Runtime)(nil).RequestLane(context.Background(), 0); !errors.Is(err, session.ErrRuntimeClosed) {
		t.Fatalf("nil lane request error = %v", err)
	}
	if _, err := receiver.AttachLane(context.Background(), protocolsession.LaneGrant{}, newMemoryChannel(t), transferlanes.RouteDirect); !errors.Is(err, session.ErrRuntimeConfig) {
		t.Fatalf("invalid lane grant error = %v", err)
	}

	initialID, initialEpoch := receiver.LaneIdentity()
	duplicateGrant, err := receiver.RequestLane(context.Background(), initialID)
	if err != nil {
		t.Fatal(err)
	}
	_, receiverErr, senderErr := attachGrantedLane(t, fixture.senderFactory, receiver, duplicateGrant)
	var receiverRejection, senderRejection *session.LaneRejectedError
	if !errors.As(receiverErr, &receiverRejection) || !errors.As(senderErr, &senderRejection) ||
		receiverRejection.Rejection.Code != protocolsession.LaneRejectAdmissionLimited ||
		senderRejection.Rejection.Code != protocolsession.LaneRejectAdmissionLimited {
		t.Fatalf("active lane replacement errors = receiver %v sender %v", receiverErr, senderErr)
	}

	sendFailureGrant := mustRequestLane(t, receiver)
	sendFailure, sendFailurePeer := newMemoryChannelPair()
	_ = sendFailure.Close()
	t.Cleanup(func() { _ = sendFailurePeer.Close() })
	if _, err := receiver.AttachLane(context.Background(), sendFailureGrant, sendFailure, transferlanes.RouteDirect); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lane hello send error = %v", err)
	}

	receiveFailureGrant := mustRequestLane(t, receiver)
	receiveFailure, receiveFailurePeer := newMemoryChannelPair()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := receiver.AttachLane(cancelled, receiveFailureGrant, receiveFailure, transferlanes.RouteDirect); !errors.Is(err, context.Canceled) {
		t.Fatalf("lane response cancellation = %v", err)
	}
	_ = receiveFailurePeer.Close()

	badRejectGrant := mustRequestLane(t, receiver)
	badReject, badRejectPeer := newMemoryChannelPair()
	go respondToLaneHello(badRejectPeer, make([]byte, protocolsession.LaneRejectBytes))
	if _, err := receiver.AttachLane(context.Background(), badRejectGrant, badReject, transferlanes.RouteDirect); err == nil {
		t.Fatal("unsigned lane rejection was accepted")
	}
	_ = badRejectPeer.Close()

	badAcceptGrant := mustRequestLane(t, receiver)
	badAccept, badAcceptPeer := newMemoryChannelPair()
	go respondToLaneHello(badAcceptPeer, []byte{1})
	if _, err := receiver.AttachLane(context.Background(), badAcceptGrant, badAccept, transferlanes.RouteDirect); err == nil {
		t.Fatal("malformed lane acceptance was accepted")
	}
	_ = badAcceptPeer.Close()

	sendResponseGrant := mustRequestLane(t, receiver)
	sendResponseHello := laneHelloForGrant(t, receiver, sendResponseGrant)
	sendResponse, sendResponsePeer := newMemoryChannelPair()
	_ = sendResponsePeer.Send(context.Background(), sendResponseHello)
	_ = sendResponsePeer.Close()
	if _, err := sender.AdmitPeerChannel(
		context.Background(), sendResponse, allowSenderPeerSettlement,
	); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lane acceptance send error = %v", err)
	}

	randomGrant := mustRequestLane(t, receiver)
	randomHello := laneHelloForGrant(t, receiver, randomGrant)
	random.fail(io.ErrUnexpectedEOF)
	randomFailure, randomFailurePeer := newMemoryChannelPair()
	_ = randomFailurePeer.Send(context.Background(), randomHello)
	if _, err := sender.AdmitPeerChannel(
		context.Background(), randomFailure, allowSenderPeerSettlement,
	); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lane sender nonce error = %v", err)
	}
	if randomFailure.State() != framechannel.Closed {
		t.Fatal("nonce-failed candidate remained open")
	}
	_ = randomFailurePeer.Close()

	for _, invalid := range []string{string([]byte{0xff}), "e\u0301", strings.Repeat("x", session.MaximumTerminalMessageBytes+1)} {
		if err := sender.Stop(context.Background(), invalid); !errors.Is(err, session.ErrRuntimeConfig) {
			t.Fatalf("invalid terminal message error = %v", err)
		}
	}
	identity := session.LaneIdentity{ID: initialID, Epoch: initialEpoch}
	if !receiver.DetachLane(identity) || !sender.DetachLane(identity) {
		t.Fatal("initial lane did not detach")
	}
}

func newUnstartedRuntime(t *testing.T, role protocolsession.Role) (*sessioncore.Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithPolicy(t, role, protocolsession.OperationLimits{}, nil)
}

func newUnstartedRuntimeWithPolicy(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
) (*sessioncore.Runtime, *memoryChannel) {
	return newUnstartedRuntimeWithContinuations(t, role, limits, now, nil)
}

func newUnstartedRuntimeWithContinuations(
	t *testing.T,
	role protocolsession.Role,
	limits protocolsession.OperationLimits,
	now func() time.Time,
	continuations protocolsession.OperationContinuationClassifier,
) (*sessioncore.Runtime, *memoryChannel) {
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
	producer, consumer, _ := observationstream.New[sessionobs.ProtocolObservation](256)
	auth := &switchableAuthenticator{current: permissiveInboundAuthenticator()}
	runtime, err := sessioncore.New(sessioncore.Config{
		Share: share, Role: role, Keys: keys, LaneID: 1, Channel: channel,
		Random: &deterministicReader{next: 99}, Authenticator: auth,
		Continuations: continuations, OperationLimits: limits, Now: now, ProtocolObservations: producer,
	})
	if err != nil {
		keys.Destroy()
		t.Fatal(err)
	}
	coreFixtures.Store(runtime, &coreFixture{auth: auth, keys: keys, channel: channel, peer: peer, recorder: &protocolTraceRecorder{producer: producer, consumer: consumer}})
	t.Cleanup(func() { stopTestCore(runtime); coreFixtures.Delete(runtime) })
	return runtime, channel
}

func permissiveInboundAuthenticator() protocolsession.InboundMessageAuthenticator {
	return protocolsession.InboundMessageAuthenticatorFunc(
		func(uint64, protocolsession.Message) (protocolsession.InboundAuthenticationResult, error) {
			return protocolsession.InboundAuthenticationResult{}, nil
		},
	)
}

func mustRequestLane(t *testing.T, runtime *Runtime) protocolsession.LaneGrant {
	t.Helper()
	grant, err := runtime.RequestLane(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func laneHelloForGrant(t *testing.T, runtime *Runtime, grant protocolsession.LaneGrant) []byte {
	t.Helper()
	hello, err := runtime.core.NewLaneHello(grant)
	if err != nil {
		t.Fatal(err)
	}
	return hello.Encoded()
}

func respondToLaneHello(channel *memoryChannel, response []byte) {
	<-channel.Recv()
	_ = channel.Send(context.Background(), framechannel.Frame(response))
}
