package sessionruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestPeerPathControlUsesAuthenticatedSessionWithoutNegotiation(t *testing.T) {
	fixture := newVerticalFixture(t)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	senderSession := senderPeerSession{runtime: sender, outbound: sender.outbound}
	senderReceived := make(chan []byte, 1)
	receiverReceived := make(chan []byte, 1)
	senderSession.SetPeerPathControlHandler(func(_ context.Context, body []byte) error { senderReceived <- body; return nil })
	receiver.SetPeerPathControlHandler(func(_ context.Context, body []byte) error { receiverReceived <- body; return nil })
	body, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1, Kind: protocolsession.PeerPathDemand, ValidFor: time.Minute, HoldFor: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := receiver.SendPeerPathControl(ctx, body); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-senderReceived:
		if !bytes.Equal(received, body) {
			t.Fatal("receiver body changed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := senderSession.SendPeerPathControl(ctx, body); err != nil {
		t.Fatal(err)
	}
	select {
	case received := <-receiverReceived:
		if !bytes.Equal(received, body) {
			t.Fatal("signed sender body changed")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	receiver.SetPeerPathControlHandler(nil)
	senderSession.SetPeerPathControlHandler(nil)
	if err := receiver.SendPeerPathControl(ctx, []byte{1}); err == nil {
		t.Fatal("invalid outbound body")
	}
	if err := senderSession.SendPeerPathControl(ctx, []byte{1}); err == nil {
		t.Fatal("invalid signed outbound body")
	}
	if err := (*ReceiverRuntime)(nil).SendPeerPathControl(ctx, body); err == nil {
		t.Fatal("nil receiver accepted control")
	}
	if err := (senderPeerSession{}).SendPeerPathControl(ctx, body); err == nil {
		t.Fatal("nil sender accepted control")
	}
}

func TestKnownPeerSessionFailureSealsUnsafeAuthority(t *testing.T) {
	for _, code := range []uint16{protocolsession.PeerOperationCodeAuthentication, protocolsession.PeerOperationCodeSessionInvariant, 0x5fff} {
		fixture := newReceiverPeerTerminalFixture(t, byte(code))
		semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{Scope: protocolsession.OperationScopePeer, PeerAttempt: &protocolsession.PeerAttemptBinding{PeerPathID: [16]byte{1}, AttemptID: [16]byte{2}, AttemptSequence: 1}, Code: code, Message: "remote peer failure"})
		if err != nil {
			t.Fatal(err)
		}
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
		message := signedPeerOperationControl(t, &ReceiverRuntime{runtimeCore: fixture.runtime}, key, protocolsession.MessageOperationError, fixture.operation.OperationID(), semantic)
		enqueueCallResponse(fixture.call, message)
		terminal := requireReceiverPeerTermination(t, fixture.operation.Receive(context.Background()))
		if !fixture.operation.OwnsTermination(terminal) {
			t.Fatal("unsealed termination")
		}
		if code == 0x5fff {
			if terminal.Severity() != ReceiverPeerTerminalOperationOnly || fixture.runtime.ctx.Err() != nil {
				t.Fatal("unknown reason escalated beyond path")
			}
		} else if terminal.Severity() != ReceiverPeerTerminalSessionUnsafe || terminal.ConsequenceProvenance() != ReceiverPeerProvenanceRemoteSessionRejected || fixture.runtime.ctx.Err() == nil {
			t.Fatalf("known session failure not sealed: %+v", terminal)
		}
	}
}
