package relayv2_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	capabilitylink "github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderobject"
	"github.com/windshare/windshare/internal/testloopback"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	relaytransport "github.com/windshare/windshare/transport/relayv2"
)

const (
	relayScenarioTimeout = 10 * time.Second

	relaySenderSeedByte       = byte(0x35)
	relayDescriptorKeyByte    = byte(0x46)
	relayDescriptorNonceByte  = byte(0x57)
	relayShareInstanceSeed    = byte(0x68)
	relayResumeTokenSeed      = byte(0x79)
	receiverToSenderFrameText = "receiver-to-sender-over-real-websocket"
	senderToReceiverFrameText = "sender-to-receiver-over-real-websocket"

	receiverSendFailureReason       relayFrameExchangeFailureReason = "receiver_send_failed"
	senderAcceptFailureReason       relayFrameExchangeFailureReason = "sender_accept_failed"
	sessionIdentityFailureReason    relayFrameExchangeFailureReason = "session_identity_mismatch"
	senderReceiveFailureReason      relayFrameExchangeFailureReason = "sender_receive_failed"
	senderSendFailureReason         relayFrameExchangeFailureReason = "sender_send_failed"
	receiverReceiveFailureReason    relayFrameExchangeFailureReason = "receiver_receive_failed"
	unexpectedExchangeFailureReason                                 = "unexpected_frame_exchange_failure"
)

// The relay must not interpret descriptor plaintext. A minimal canonical CBOR
// map keeps the signed envelope protocol-shaped without importing catalog state.
var relayDescriptorPlaintext = []byte{0xa1, 0x00, 0x01}

type relayProtocolFixture struct {
	init       v2.RegisterInit
	descriptor []byte
	privateKey ed25519.PrivateKey
}

type relayPeerContext struct {
	Address        string `json:"address"`
	RelaySessionID string `json:"relay_session_id,omitempty"`
}

type relayFrameExchangeContext struct {
	ReceiverToSenderBytes int `json:"receiver_to_sender_bytes"`
	SenderToReceiverBytes int `json:"sender_to_receiver_bytes"`
}

type relayFrameExchangeFailureReason string

type relayFrameExchangeError struct {
	reason relayFrameExchangeFailureReason
	cause  error
}

func (err *relayFrameExchangeError) Error() string {
	return fmt.Sprintf("relay frame exchange %s: %v", err.reason, err.cause)
}

func (err *relayFrameExchangeError) Unwrap() error { return err.cause }

func (err *relayFrameExchangeError) FailureReason() string { return string(err.reason) }

type relayConnectionOwner interface {
	Close() error
	Done() <-chan struct{}
	Err() error
}

func TestRelayWebSocketRegistrationAndBidirectionalFrameExchange(t *testing.T) {
	trace := startRelayScenario(t)

	ctx, cancel := context.WithTimeout(context.Background(), relayScenarioTimeout)
	trace.RequireCleanup(t, "scenario deadline", func(context.Context) error {
		cancel()
		return nil
	})
	loopback := testloopback.New(t)
	trace.RequireCleanup(t, "loopback network fixture", func(context.Context) error {
		return loopback.Close()
	})
	listener := loopback.ListenTCP()
	runtime, err := startRelayRuntime(ctx, listener, t.TempDir())
	if err != nil {
		t.Fatalf("start relay integration runtime: %v", err)
	}
	trace.RequireCleanup(t, "relay runtime", func(context.Context) error {
		return runtime.Close()
	})
	trace.RequireRecord(
		t,
		testrun.Milestone(testrun.ListenerReadyMilestone),
		testrun.OutcomeSucceeded,
		testrun.ListenerReadyContext{Address: listener.Addr().String()},
	)
	peerReadiness, err := trace.StartPhase(
		testrun.PeerReadyMilestone,
		relayPeerContext{Address: listener.Addr().String()},
	)
	if err != nil {
		t.Fatalf("start relay peer readiness trace: %v", err)
	}

	protocolFixture, err := newRelayProtocolFixture()
	if err != nil {
		t.Fatalf("create relay protocol fixture: %v", err)
	}
	sender, err := relaytransport.DialSender(ctx, relaytransport.SenderConfig{
		RelayBaseURL:     runtime.BaseURL(),
		Init:             protocolFixture.init,
		SenderPrivateKey: protocolFixture.privateKey,
		Descriptor:       protocolFixture.descriptor,
	})
	if err != nil {
		t.Fatalf("register sender through real WebSocket: %v", err)
	}
	trace.RequireCleanup(t, "sender relay connection", func(context.Context) error {
		return closeRelayConnection("sender", sender)
	})
	stats := sender.RegistrationStats()
	if stats.BytesSent == 0 || stats.BytesReceived == 0 {
		t.Fatalf("sender registration did not exchange protocol bytes: %+v", stats)
	}

	receiver, err := relaytransport.DialReceiver(ctx, relaytransport.ReceiverConfig{
		RelayBaseURL: runtime.BaseURL(),
		ShareID:      protocolFixture.init.ShareID,
	})
	if err != nil {
		t.Fatalf("join receiver through real WebSocket: %v", err)
	}
	trace.RequireCleanup(t, "receiver relay connection", func(context.Context) error {
		return closeRelayConnection("receiver", receiver)
	})
	if !bytes.Equal(receiver.Descriptor(), protocolFixture.descriptor) {
		t.Fatal("receiver did not obtain the sender's authenticated descriptor")
	}
	relaySessionID := receiver.Channel().RelaySessionID()
	if err := peerReadiness.Succeed(relayPeerContext{
		Address:        listener.Addr().String(),
		RelaySessionID: hex.EncodeToString(relaySessionID[:]),
	}); err != nil {
		t.Fatalf("record relay peer readiness: %v", err)
	}

	receiverFrame := framechannel.Frame(receiverToSenderFrameText)
	senderFrame := framechannel.Frame(senderToReceiverFrameText)
	exchangeContext := relayFrameExchangeContext{
		ReceiverToSenderBytes: len(receiverFrame),
		SenderToReceiverBytes: len(senderFrame),
	}
	if err := observeRelayFrameExchange(
		trace,
		exchangeContext,
		func() error {
			return exchangeRelayFrames(
				ctx,
				sender,
				receiver,
				relaySessionID,
				receiverFrame,
				senderFrame,
			)
		},
	); err != nil {
		t.Fatalf("exchange frames through real relay WebSocket: %v", err)
	}
	trace.RequireSuccess(t)
}

func observeRelayFrameExchange(
	trace *testscenario.Trace,
	payload relayFrameExchangeContext,
	exchange func() error,
) error {
	if trace == nil || exchange == nil {
		return errors.New("relay frame exchange observation is invalid")
	}
	phase, err := trace.StartPhase(frameExchangeMilestone, payload)
	if err != nil {
		return fmt.Errorf("start frame exchange evidence: %w", err)
	}
	if err := exchange(); err != nil {
		reason := unexpectedExchangeFailureReason
		var failure interface{ FailureReason() string }
		if errors.As(err, &failure) {
			reason = failure.FailureReason()
		}
		recordErr := phase.Fail(reason)
		if errors.Is(recordErr, testscenario.ErrFailureReason) {
			recordErr = phase.Fail(unexpectedExchangeFailureReason)
		}
		return errors.Join(err, recordErr)
	}
	if err := phase.Succeed(payload); err != nil {
		return fmt.Errorf("complete frame exchange evidence: %w", err)
	}
	return nil
}

func exchangeRelayFrames(
	ctx context.Context,
	sender *relaytransport.SenderConnection,
	receiver *relaytransport.ReceiverConnection,
	relaySessionID v2.RelaySessionID,
	receiverFrame framechannel.Frame,
	senderFrame framechannel.Frame,
) error {
	if err := receiver.Channel().Send(ctx, receiverFrame); err != nil {
		return newRelayFrameExchangeError(receiverSendFailureReason, err)
	}
	senderChannel, err := sender.Accept(ctx)
	if err != nil {
		return newRelayFrameExchangeError(senderAcceptFailureReason, err)
	}
	if senderChannel.RelaySessionID() != relaySessionID {
		return newRelayFrameExchangeError(
			sessionIdentityFailureReason,
			fmt.Errorf(
				"sender accepted relay session %x, want %x",
				senderChannel.RelaySessionID(),
				relaySessionID,
			),
		)
	}
	if err := requireRelayFrame(ctx, senderChannel.Recv(), receiverFrame); err != nil {
		return newRelayFrameExchangeError(senderReceiveFailureReason, err)
	}
	if err := senderChannel.Send(ctx, senderFrame); err != nil {
		return newRelayFrameExchangeError(senderSendFailureReason, err)
	}
	if err := requireRelayFrame(ctx, receiver.Channel().Recv(), senderFrame); err != nil {
		return newRelayFrameExchangeError(receiverReceiveFailureReason, err)
	}
	return nil
}

func newRelayFrameExchangeError(
	reason relayFrameExchangeFailureReason,
	cause error,
) error {
	if cause == nil {
		cause = errors.New("frame exchange failed without a cause")
	}
	return &relayFrameExchangeError{reason: reason, cause: cause}
}

func newRelayProtocolFixture() (relayProtocolFixture, error) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{relaySenderSeedByte}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	pkHashRaw, err := capabilitylink.SenderKeyHash(publicKey)
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("derive relay sender key hash: %w", err)
	}
	shareIDText, err := capabilitylink.ShareIDForSenderKeyHash(pkHashRaw[:])
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("derive relay share ID: %w", err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareIDText)
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("decode relay share ID: %w", err)
	}
	shareID, err := v2.ShareIDFromBytes(shareIDRaw)
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("convert relay share ID: %w", err)
	}
	pkHash, err := v2.PKHashFromBytes(pkHashRaw[:])
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("convert relay sender key hash: %w", err)
	}
	binding, err := senderobject.NewDescriptorBinding(pkHash[:], shareID[:])
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("bind relay descriptor: %w", err)
	}
	descriptor, err := senderobject.Seal(
		binding,
		bytes.Repeat([]byte{relayDescriptorKeyByte}, sha256.Size),
		privateKey,
		bytes.Repeat([]byte{relayDescriptorNonceByte}, senderobject.NonceBytes),
		relayDescriptorPlaintext,
	)
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("seal relay descriptor: %w", err)
	}
	var shareInstance v2.ShareInstance
	for index := range shareInstance {
		shareInstance[index] = relayShareInstanceSeed + byte(index)
	}
	var resumeToken v2.ResumeToken
	for index := range resumeToken {
		resumeToken[index] = relayResumeTokenSeed + byte(index)
	}
	init, err := relaytransport.NewFreshRegisterInit(
		shareID,
		shareInstance,
		pkHash,
		descriptor,
		resumeToken,
	)
	if err != nil {
		return relayProtocolFixture{}, fmt.Errorf("create fresh relay registration: %w", err)
	}
	return relayProtocolFixture{init: init, descriptor: descriptor, privateKey: privateKey}, nil
}

func closeRelayConnection(name string, connection relayConnectionOwner) error {
	if connection == nil {
		return errors.New("relay connection owner is nil")
	}
	closeErr := connection.Close()
	timer := time.NewTimer(relayCleanupTimeout)
	defer timer.Stop()
	select {
	case <-connection.Done():
		if terminalErr := connection.Err(); terminalErr != nil {
			return errors.Join(closeErr, fmt.Errorf("%s relay connection terminal error: %w", name, terminalErr))
		}
		return closeErr
	case <-timer.C:
		return errors.Join(closeErr, fmt.Errorf("%s relay connection did not stop within %s", name, relayCleanupTimeout))
	}
}

func requireRelayFrame(
	ctx context.Context,
	frames <-chan framechannel.Frame,
	want framechannel.Frame,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case frame, open := <-frames:
		if !open {
			return errors.New("relay frame channel closed before the expected frame")
		}
		if !bytes.Equal(frame, want) {
			return fmt.Errorf("relay frame = %q, want %q", frame, want)
		}
		return nil
	}
}
