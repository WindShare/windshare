package receiversession

import (
	"context"

	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestAuthenticatedMalformedPeerFailurePublishesUnsafeExactOperationBeforeShutdown(t *testing.T) {
	fixture := newReceiverPeerTerminalFixture(t, 0xd4)
	received := make(chan PeerReceiveResult, 1)
	go func() { received <- fixture.operation.Receive(context.Background()) }()
	waitReceiverPeerState(t, fixture.operation, "receive admitted before malformed failure", func(
		_ receiverPeerTerminalTransition,
		_ receiverPeerTerminalConsequence,
		_ PeerDiagnosticSnapshot,
		receiving bool,
	) bool {
		return receiving
	})

	validator, err := newReceiverSenderControlValidator(nil)
	if err != nil {
		t.Fatal(err)
	}
	authenticator := protocolsession.SenderMessageValidator(validator)
	operationID := fixture.operation.OperationID()
	message, err := protocolsession.NewMessage(protocolsession.MessageOperationError, &operationID, []byte{0xf6})
	if err != nil {
		t.Fatal(err)
	}
	sendAuthenticatedTestMessage(t, fixture.runtime, message, authenticator)

	termination := requireReceiverPeerTermination(t, <-received)
	if !fixture.operation.OwnsTermination(termination) ||
		termination.Authority() != PeerTerminalAuthorityRemote ||
		termination.TransitionProvenance() != PeerProvenanceRemoteFailureMalformed ||
		termination.Severity() != PeerTerminalSessionUnsafe ||
		termination.ConsequenceProvenance() != PeerProvenanceRemoteFailureMalformed {
		t.Fatalf("malformed failure termination = %+v", termination)
	}
	if !receiverPeerDiagnosticsContain(
		termination.Diagnostics(),
		PeerDiagnosticRemoteFailureMalformed,
	) {
		t.Fatalf("malformed failure diagnostics = %+v", termination.Diagnostics().Components())
	}
}

func TestAuthenticatedMalformedPeerControlTerminatesOnlyExactOperation(t *testing.T) {
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
			received := make(chan PeerReceiveResult, 1)
			go func() { received <- fixture.operation.Receive(context.Background()) }()
			waitReceiverPeerState(t, fixture.operation, "receive admitted before malformed peer control", func(
				_ receiverPeerTerminalTransition,
				_ receiverPeerTerminalConsequence,
				_ PeerDiagnosticSnapshot,
				receiving bool,
			) bool {
				return receiving
			})

			validator, err := newReceiverSenderControlValidator(nil)
			if err != nil {
				t.Fatal(err)
			}
			authenticator := protocolsession.SenderMessageValidator(validator)
			operationID := fixture.operation.OperationID()
			message, err := protocolsession.NewMessage(testCase.kind, &operationID, []byte{0xf6})
			if err != nil {
				t.Fatal(err)
			}
			sendAuthenticatedTestMessage(t, fixture.runtime, message, authenticator)

			termination := requireReceiverPeerTermination(t, <-received)
			if !fixture.operation.OwnsTermination(termination) ||
				termination.Authority() != PeerTerminalAuthorityRemote ||
				termination.TransitionProvenance() != PeerProvenanceRemoteControlMalformed ||
				termination.Severity() != PeerTerminalOperationOnly ||
				termination.ConsequenceProvenance() != PeerProvenanceRemoteControlMalformed {
				t.Fatalf("malformed peer control termination = %+v", termination)
			}
			if !receiverPeerDiagnosticsContain(
				termination.Diagnostics(),
				PeerDiagnosticControlMalformed,
			) {
				t.Fatalf("malformed peer control diagnostics = %+v", termination.Diagnostics().Components())
			}
			if fixture.runtime.Lifetime().Err() != nil {
				t.Fatal("operation-local malformed peer control terminated the protocol session")
			}
		})
	}
}

func TestReceiverPeerAuthenticatedViolationEvidenceCodes(t *testing.T) {
	tests := []struct {
		code protocolsession.AuthenticatedOperationViolationCode
		want PeerTerminalProvenance
		diag PeerDiagnosticCode
	}{
		{
			protocolsession.AuthenticatedOperationViolationMalformedFailure,
			PeerProvenanceRemoteFailureMalformed,
			PeerDiagnosticRemoteFailureMalformed,
		},
		{
			protocolsession.AuthenticatedOperationViolationMalformedPeerControl,
			PeerProvenanceRemoteControlMalformed,
			PeerDiagnosticControlMalformed,
		},
		{
			protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer,
			PeerProvenanceRemoteAnswerConflict,
			PeerDiagnosticRemoteAnswerConflict,
		},
		{
			protocolsession.AuthenticatedOperationViolationConflictingFinal,
			PeerProvenanceRemoteFinalConflict,
			PeerDiagnosticRemoteFinalConflict,
		},
		{
			protocolsession.AuthenticatedOperationViolationContinuationContract,
			PeerProvenanceRemoteContinuationViolation,
			PeerDiagnosticRemoteContinuationViolation,
		},
	}
	for _, test := range tests {
		violation, err := protocolsession.NewAuthenticatedOperationViolation(test.code)
		if err != nil {
			t.Fatal(err)
		}
		evidence, ok := receiverPeerAuthenticatedViolationEvidence(violation)
		if !ok || evidence.transition.provenance != test.want || !receiverPeerDiagnosticsContain(evidence.diagnostics, test.diag) {
			t.Fatalf("code %v evidence = %+v, ok = %v", test.code, evidence, ok)
		}
	}
	invalidViolation := protocolsession.AuthenticatedOperationViolation{}
	if _, ok := receiverPeerAuthenticatedViolationEvidence(invalidViolation); ok {
		t.Fatal("invalid violation produced evidence")
	}
}
