package v2peer

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const (
	receiverRuntimeScopeCandidateLimit   = 2
	receiverRuntimeScopeInitialLaneID    = uint32(73)
	receiverRuntimeScopeTestTimeout      = 10 * time.Second
	receiverRuntimeScopeAttemptTimeout   = 5 * time.Second
	receiverRuntimeScopeRelay            = "ws://127.0.0.1:8484"
	receiverRuntimeScopeExpectedOfferSDP = "v=0\r\ns=authoritative-local-offer\r\n"
	receiverRuntimeScopeFailureMessage   = "Block failure sent for a peer operation"
)

type receiverRuntimeScopeConnectResult struct {
	runtime *sessionruntime.ReceiverRuntime
	err     error
}

func TestReceiverRuntimeCrossScopeOperationErrorIsRemoteSessionUnsafe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), receiverRuntimeScopeTestTimeout)
	t.Cleanup(cancel)

	selected := filepath.Join(t.TempDir(), "receiver-runtime-scope.txt")
	if err := os.WriteFile(selected, []byte("receiver runtime scope"), 0o600); err != nil {
		t.Fatal(err)
	}
	preparedSender, err := liveshare.PrepareSender(ctx, liveshare.SenderConfig{
		Paths: []string{selected}, Relays: []string{receiverRuntimeScopeRelay}, ChunkSize: catalog.MinChunkSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := preparedSender.Close(); err != nil {
			t.Errorf("close prepared sender: %v", err)
		}
	})

	capability := preparedSender.Capability()
	registration := preparedSender.Registration()
	t.Cleanup(func() {
		clear(capability.ReadSecret)
		clear(registration.SenderPrivateKey)
	})
	preparedReceiver, err := liveshare.PrepareReceiver(liveshare.ReceiverConfig{
		Capability:       capability,
		DescriptorObject: registration.Descriptor,
		PeerControls: v2signal.ReceiverControlValidator{
			MaximumCandidates: receiverRuntimeScopeCandidateLimit,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(preparedReceiver.Close)

	share := preparedReceiver.Descriptor().ShareInstance()
	keyTree, err := content.NewKeyTree(capability.ReadSecret, share)
	if err != nil {
		t.Fatal(err)
	}
	sessionAuth, err := keyTree.SessionAuthKey()
	if err != nil {
		keyTree.Destroy()
		t.Fatal(err)
	}
	sessionAuthKey := sessionAuth.Bytes()
	sessionAuth.Destroy()
	keyTree.Destroy()
	t.Cleanup(func() { clear(sessionAuthKey) })

	senderLane, receiverLane := newCandidateTransactionChannelPair()
	t.Cleanup(func() {
		_ = receiverLane.Close()
		_ = senderLane.Close()
	})
	connected := make(chan receiverRuntimeScopeConnectResult, 1)
	go func() {
		runtime, connectErr := preparedReceiver.Connect(ctx, receiverLane)
		connected <- receiverRuntimeScopeConnectResult{runtime: runtime, err: connectErr}
	}()

	// A fake signaling operation would bypass the transcript, envelope, and sender-signature
	// authorities whose interaction must preserve this authenticated semantic violation.
	clientFrame := receiveTest(t, senderLane.Recv())
	replayGuard, err := protocolsession.NewClientHelloReplayGuard(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientHello, err := replayGuard.AcceptClientHello(clientFrame, share, sessionAuthKey)
	if err != nil {
		t.Fatal(err)
	}
	senderEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	senderNonce := make([]byte, protocolsession.HandshakeNonceBytes)
	if _, err := io.ReadFull(rand.Reader, senderNonce); err != nil {
		t.Fatal(err)
	}
	serverHello, err := protocolsession.NewServerHello(
		clientHello,
		senderNonce,
		senderEphemeral.PublicKey(),
		receiverRuntimeScopeInitialLaneID,
		registration.SenderPrivateKey,
	)
	clear(senderNonce)
	if err != nil {
		t.Fatal(err)
	}
	senderKeys, err := protocolsession.DeriveSenderSession(
		senderEphemeral,
		sessionAuthKey,
		clientHello,
		serverHello,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(senderKeys.Destroy)
	if err := senderLane.Send(ctx, framechannel.Frame(serverHello.Encoded())); err != nil {
		t.Fatal(err)
	}
	connectedResult := receiveTest(t, connected)
	if connectedResult.err != nil || connectedResult.runtime == nil {
		t.Fatalf("connect receiver runtime: runtime=%p err=%v", connectedResult.runtime, connectedResult.err)
	}
	receiverRuntime := connectedResult.runtime
	t.Cleanup(receiverRuntime.Close)
	if laneID, laneEpoch := receiverRuntime.LaneIdentity(); laneID != receiverRuntimeScopeInitialLaneID || laneEpoch != serverHello.InitialLaneEpoch() {
		t.Fatalf("initial receiver lane=(%d,%d), want (%d,%d)",
			laneID, laneEpoch, receiverRuntimeScopeInitialLaneID, serverHello.InitialLaneEpoch())
	}

	sessionID := senderKeys.ProtocolSessionID()
	receiverToSenderBinding := protocolsession.EnvelopeBinding{
		ShareInstance: share, ProtocolSessionID: sessionID,
		LaneID: receiverRuntimeScopeInitialLaneID, LaneEpoch: serverHello.InitialLaneEpoch(),
		Direction: protocolsession.DirectionReceiverToSender,
	}
	senderToReceiverBinding := receiverToSenderBinding
	senderToReceiverBinding.Direction = protocolsession.DirectionSenderToReceiver
	offerOpener, err := protocolsession.NewEnvelopeOpener(
		senderKeys.ReceiverToSender(),
		receiverToSenderBinding,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(offerOpener.Destroy)
	responseSealer, err := protocolsession.NewEnvelopeSealer(
		senderKeys.SenderToReceiver(),
		senderToReceiverBinding,
		rand.Reader,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(responseSealer.Destroy)

	receiverPeer := newReceiverTestPeerConnection()
	receiverDataChannel := newReceiverTestChannel()
	traces := make(chan ReceiverTerminationTrace, 1)
	receiverFactory, err := NewReceiverFactory(ReceiverFactoryConfig{
		MaxCandidates:  receiverRuntimeScopeCandidateLimit,
		AttemptTimeout: receiverRuntimeScopeAttemptTimeout,
		PeerConnections: ReceiverPeerConnectionFactoryFunc(func(pion.Configuration) (ReceiverPeerConnection, error) {
			return receiverPeer, nil
		}),
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			return receiverDataChannel, nil
		}),
		OnTermination: func(trace ReceiverTerminationTrace) {
			traces <- trace
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	receiverSignaling, err := NewRuntimeReceiverSignaling(receiverRuntime)
	if err != nil {
		t.Fatal(err)
	}
	receiverAttempt, err := receiverFactory.Start(ctx, receiverSignaling, receiverRuntime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = receiverAttempt.Close() })

	offerEnvelope, err := offerOpener.Open(receiveTest(t, senderLane.Recv()))
	if err != nil {
		t.Fatal(err)
	}
	offerMessage, err := protocolsession.DecodeMessage(offerEnvelope.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	operationID, hasOperationID := offerMessage.OperationID()
	if offerMessage.Kind() != protocolsession.MessagePeerOffer || !hasOperationID || operationID.IsZero() {
		t.Fatalf("decrypted receiver offer kind=%d operation=%x present=%t",
			offerMessage.Kind(), operationID, hasOperationID)
	}
	offer, err := v2signal.DecodeOffer(offerMessage.Body())
	if err != nil {
		t.Fatal(err)
	}
	if offer.SDP != receiverRuntimeScopeExpectedOfferSDP {
		t.Fatalf("decrypted receiver offer SDP=%q", offer.SDP)
	}

	// A valid block failure isolates the peer-scope check from generic CBOR, signature,
	// and operation-error validation, so only the cross-scope authority is adversarial.
	failureBody, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope:   contentflow.BlockErrorScope,
		Code:    contentflow.BlockCodeInvalidRef,
		Message: receiverRuntimeScopeFailureMessage,
	})
	if err != nil {
		t.Fatal(err)
	}
	responseSequence, err := responseSealer.NextSequence()
	if err != nil {
		t.Fatal(err)
	}
	signedFailureBody, err := protocolsession.SignControlBody(
		registration.SenderPrivateKey,
		protocolsession.ControlDomainOperation,
		protocolsession.ControlBinding{
			ShareInstance: share, ProtocolSessionID: sessionID,
			LaneID: receiverRuntimeScopeInitialLaneID, LaneEpoch: serverHello.InitialLaneEpoch(),
			Direction: protocolsession.DirectionSenderToReceiver,
			Sequence:  responseSequence, MessageKind: protocolsession.MessageOperationError,
			OperationID: operationID, HasOperationID: true,
		},
		failureBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	failureMessage, err := protocolsession.NewMessage(
		protocolsession.MessageOperationError,
		&operationID,
		signedFailureBody,
	)
	if err != nil {
		t.Fatal(err)
	}
	failurePlaintext, err := protocolsession.EncodeMessage(failureMessage)
	if err != nil {
		t.Fatal(err)
	}
	sealedFailure, err := responseSealer.Seal(failurePlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if sealedFailure.Sequence != responseSequence {
		t.Fatalf("sealed response sequence=%d, signed sequence=%d", sealedFailure.Sequence, responseSequence)
	}
	if err := senderLane.Send(ctx, framechannel.Frame(sealedFailure.Frame)); err != nil {
		t.Fatal(err)
	}

	receiveTest(t, receiverAttempt.Done())
	outcome := receiverAttempt.Outcome()
	if outcome.OperationID() != operationID || outcome.LocalGeneration() == 0 {
		t.Fatalf("receiver outcome correlation=%+v, offer operation=%x", outcome, operationID)
	}
	if outcome.TransitionAuthority() != ReceiverTerminalRemote ||
		outcome.TransitionProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
		outcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		outcome.ConsequenceProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
		!outcome.RequiresSessionClose() || outcome.LocallyCanceled() {
		t.Fatalf("receiver terminal ownership=%+v", outcome)
	}
	if !errors.Is(outcome.RetainedCause(), protocolsession.ErrInvalidOperationFailure) {
		t.Fatalf("receiver retained cause=%v", outcome.RetainedCause())
	}
	var remoteFailure sessionruntime.RemoteOperationError
	if !errors.As(outcome.RetainedCause(), &remoteFailure) {
		t.Fatalf("receiver retained cause omitted immutable remote diagnostic: %v", outcome.RetainedCause())
	}
	failureSnapshot := remoteFailure.Failure()
	if failureSnapshot.Scope() != contentflow.BlockErrorScope ||
		failureSnapshot.Code() != contentflow.BlockCodeInvalidRef ||
		failureSnapshot.Message() != receiverRuntimeScopeFailureMessage {
		t.Fatalf("receiver remote diagnostic=%+v", failureSnapshot)
	}
	remoteFailure = sessionruntime.RemoteOperationError{}
	var repeatedRemoteFailure sessionruntime.RemoteOperationError
	if !errors.As(receiverAttempt.Outcome().RetainedCause(), &repeatedRemoteFailure) ||
		repeatedRemoteFailure.Failure() != failureSnapshot {
		t.Fatalf("receiver remote diagnostic changed after detached copy replacement: %+v", repeatedRemoteFailure)
	}
	if !outcome.HasRetainedCauseClass(ReceiverCauseProtocol) {
		t.Fatalf("receiver retained classes=%v", outcome.RetainedCauseClasses())
	}
	if len(outcome.BenignComponents()) != 0 {
		t.Fatalf("receiver benign components=%v", outcome.BenignComponents())
	}
	select {
	case <-receiverAttempt.Ready():
		t.Fatal("cross-scope failure published a ready peer lane")
	default:
	}
	if lane, ok := receiverAttempt.Lane(); ok || lane != (sessionruntime.LaneIdentity{}) {
		t.Fatalf("cross-scope failure adopted lane=%+v present=%t", lane, ok)
	}

	trace := receiveTest(t, traces)
	if trace.OperationID() != operationID || trace.LocalGeneration() != outcome.LocalGeneration() ||
		trace.TransitionAuthority() != ReceiverTerminalRemote ||
		trace.TransitionProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
		trace.Disposition() != ReceiverDispositionSessionUnsafe ||
		trace.ConsequenceProvenance() != ReceiverProvenanceRemoteFailureScopeViolation ||
		!trace.RequiresSessionClose() {
		t.Fatalf("receiver termination trace=%+v, outcome=%+v", trace, outcome)
	}
	if !receiverRuntimeScopeHasClass(trace.RetainedCauseClasses(), ReceiverCauseProtocol) ||
		len(trace.BenignComponents()) != 0 {
		t.Fatalf("receiver termination trace=%+v", trace)
	}
	receiveTest(t, receiverPeer.closed)
	receiveTest(t, receiverDataChannel.Done())

	if closeErr := receiverAttempt.Close(); closeErr != outcome.RetainedCause() {
		t.Fatalf("first Close result=%v, exact retained cause=%v", closeErr, outcome.RetainedCause())
	}
	if closeErr := receiverAttempt.Close(); closeErr != outcome.RetainedCause() {
		t.Fatalf("idempotent Close result=%v, exact retained cause=%v", closeErr, outcome.RetainedCause())
	}
	closedOutcome := receiverAttempt.Outcome()
	if closedOutcome.OperationID() != operationID ||
		closedOutcome.LocalGeneration() != outcome.LocalGeneration() ||
		closedOutcome.TransitionAuthority() != ReceiverTerminalRemote ||
		closedOutcome.Disposition() != ReceiverDispositionSessionUnsafe ||
		closedOutcome.RetainedCause() != outcome.RetainedCause() {
		t.Fatalf("Close changed exact remote outcome: before=%+v after=%+v", outcome, closedOutcome)
	}
}

func receiverRuntimeScopeHasClass(classes []ReceiverCauseClass, expected ReceiverCauseClass) bool {
	return slices.Contains(classes, expected)
}
