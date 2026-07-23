package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSignedMalformedPeerFailurePublishesUnsafeExactOperationBeforeShutdown(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xd4)
	received := make(chan ReceiverPeerReceiveResult, 1)
	go func() { received <- fixture.operation.Receive(context.Background()) }()
	waitReceiverPeerState(t, fixture.operation, "receive admitted before malformed failure", func(
		_ receiverPeerTerminalTransition,
		_ receiverPeerTerminalConsequence,
		_ ReceiverPeerDiagnosticSnapshot,
		receiving bool,
	) bool {
		return receiving
	})

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	base := protocolsession.ControlBinding{
		ShareInstance: id16Share(0xd5), ProtocolSessionID: id16[protocolsession.ProtocolSessionID](0xd6),
		LaneID: 7, LaneEpoch: 8, Direction: protocolsession.DirectionSenderToReceiver,
	}
	validator, err := newReceiverSenderControlValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := protocolsession.NewSenderControlAuthenticator(publicKey, base, validator)
	if err != nil {
		t.Fatal(err)
	}
	operationID := fixture.operation.OperationID()
	binding := base
	binding.Sequence = 1
	binding.MessageKind = protocolsession.MessageOperationError
	binding.OperationID = operationID
	binding.HasOperationID = true
	// Canonical null is valid signed control payload but not an OperationFailure.
	signed, err := protocolsession.SignControlBody(
		privateKey,
		protocolsession.ControlDomainOperation,
		binding,
		[]byte{0xf6},
	)
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageOperationError, &operationID, signed)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := protocolsession.EncodeMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	receiver, peer := newMemoryChannelPair()
	t.Cleanup(func() {
		_ = receiver.Close()
		_ = peer.Close()
	})
	pump, err := protocolsession.NewProtocolPump(
		receiver,
		semanticTestOpener{opened: protocolsession.OpenedEnvelope{Sequence: 1, Plaintext: plaintext}},
		authenticator,
		fixture.runtime.router,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Send(context.Background(), framechannel.Frame{1}); err != nil {
		t.Fatal(err)
	}
	if err := pump.Run(context.Background()); !errors.Is(err, protocolsession.ErrAuthenticatedOperationViolation) {
		t.Fatalf("pump malformed-failure result = %v", err)
	}

	termination := requireReceiverPeerTermination(t, <-received)
	if !fixture.operation.OwnsTermination(termination) ||
		termination.Authority() != ReceiverPeerTerminalAuthorityRemote ||
		termination.TransitionProvenance() != ReceiverPeerProvenanceRemoteFailureMalformed ||
		termination.Severity() != ReceiverPeerTerminalSessionUnsafe ||
		termination.ConsequenceProvenance() != ReceiverPeerProvenanceRemoteFailureMalformed {
		t.Fatalf("malformed failure termination = %+v", termination)
	}
	if !receiverPeerDiagnosticsContain(
		termination.Diagnostics(),
		ReceiverPeerDiagnosticRemoteFailureMalformed,
	) {
		t.Fatalf("malformed failure diagnostics = %+v", termination.Diagnostics().Components())
	}
}

func TestSignedMalformedPeerControlTerminatesOnlyExactOperation(t *testing.T) {
	testCases := []struct {
		name string
		kind protocolsession.MessageKind
		seed byte
	}{
		{name: "answer", kind: protocolsession.MessagePeerAnswer, seed: 0xd7},
		{name: "candidate", kind: protocolsession.MessagePeerCandidate, seed: 0xda},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReceiverPeerTerminalFixture(t, testCase.seed)
			received := make(chan ReceiverPeerReceiveResult, 1)
			go func() { received <- fixture.operation.Receive(context.Background()) }()
			waitReceiverPeerState(t, fixture.operation, "receive admitted before malformed peer control", func(
				_ receiverPeerTerminalTransition,
				_ receiverPeerTerminalConsequence,
				_ ReceiverPeerDiagnosticSnapshot,
				receiving bool,
			) bool {
				return receiving
			})

			publicKey, privateKey, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			base := protocolsession.ControlBinding{
				ShareInstance:     id16Share(testCase.seed + 1),
				ProtocolSessionID: id16[protocolsession.ProtocolSessionID](testCase.seed + 2),
				LaneID:            7, LaneEpoch: 8, Direction: protocolsession.DirectionSenderToReceiver,
			}
			validator, err := newReceiverSenderControlValidator(nil)
			if err != nil {
				t.Fatal(err)
			}
			authenticator, err := protocolsession.NewSenderControlAuthenticator(publicKey, base, validator)
			if err != nil {
				t.Fatal(err)
			}
			operationID := fixture.operation.OperationID()
			binding := base
			binding.Sequence = 1
			binding.MessageKind = testCase.kind
			binding.OperationID = operationID
			binding.HasOperationID = true
			// Canonical null proves authentication completed while leaving the
			// connectivity-specific peer control structurally malformed.
			signed, err := protocolsession.SignControlBody(
				privateKey,
				protocolsession.ControlDomainOperation,
				binding,
				[]byte{0xf6},
			)
			if err != nil {
				t.Fatal(err)
			}
			message, err := protocolsession.NewMessage(testCase.kind, &operationID, signed)
			if err != nil {
				t.Fatal(err)
			}
			plaintext, err := protocolsession.EncodeMessage(message)
			if err != nil {
				t.Fatal(err)
			}
			receiver, peer := newMemoryChannelPair()
			t.Cleanup(func() {
				_ = receiver.Close()
				_ = peer.Close()
			})
			pump, err := protocolsession.NewProtocolPump(
				receiver,
				semanticTestOpener{opened: protocolsession.OpenedEnvelope{Sequence: 1, Plaintext: plaintext}},
				authenticator,
				fixture.runtime.router,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := peer.Send(context.Background(), framechannel.Frame{1}); err != nil {
				t.Fatal(err)
			}
			// Closing after enqueue lets Run prove the violation is contained and
			// continues to the next frame instead of returning a session failure.
			if err := receiver.Close(); err != nil {
				t.Fatal(err)
			}
			if err := pump.Run(context.Background()); err != nil {
				t.Fatalf("pump contained malformed peer control = %v", err)
			}

			termination := requireReceiverPeerTermination(t, <-received)
			if !fixture.operation.OwnsTermination(termination) ||
				termination.Authority() != ReceiverPeerTerminalAuthorityRemote ||
				termination.TransitionProvenance() != ReceiverPeerProvenanceRemoteControlMalformed ||
				termination.Severity() != ReceiverPeerTerminalOperationOnly ||
				termination.ConsequenceProvenance() != ReceiverPeerProvenanceRemoteControlMalformed {
				t.Fatalf("malformed peer control termination = %+v", termination)
			}
			if !receiverPeerDiagnosticsContain(
				termination.Diagnostics(),
				ReceiverPeerDiagnosticControlMalformed,
			) {
				t.Fatalf("malformed peer control diagnostics = %+v", termination.Diagnostics().Components())
			}
			if fixture.runtime.operations.Terminated() {
				t.Fatal("operation-local malformed peer control terminated the protocol session")
			}
		})
	}
}
