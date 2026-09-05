package v2peer

import (
	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"testing"
)

func TestSenderRetirementAdvancesWithoutCumulativeIdentityExhaustion(t *testing.T) {
	authority := newSenderEvidenceAuthority(2)
	first := testBinding(21)
	operation := testPeerOperation(testOperationID(22))
	for sequence := uint64(1); sequence <= 10000; sequence++ {
		binding := first
		binding.AttemptSequence = sequence
		binding.AttemptID[0] = byte(sequence)
		claim := authority.claim(operation, binding)
		if !claim.acquired || claim.capacity {
			t.Fatalf("ordinary retry %d exhausted identity retention", sequence)
		}
		if len(authority.claims) != 1 || len(authority.latest) != 1 {
			t.Fatal("historical identities retained")
		}
		if !authority.claimed(first) {
			t.Fatal("pruned identity became admissible")
		}
		replay := binding
		replay.AttemptID = v2signal.AttemptID{255}
		if authority.claim(operation, replay).acquired {
			t.Fatal("same sequence with fresh random ID replayed")
		}
	}
}

func TestReceiverRecoveryScopeUsesSealedDecisionAndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name     string
		decision receiverAttemptDecision
		scope    protocolsession.PeerFailureRecoveryScope
	}{
		{"local ICE", receiverOperationDecision(ReceiverTerminalLocal, ReceiverProvenanceLocalNegotiationFailure), protocolsession.PeerFailureAttemptTransient},
		{"unknown rejection", receiverOperationDecision(ReceiverTerminalRemote, ReceiverProvenanceRemoteOperationRejected), protocolsession.PeerFailurePathTerminal},
		{"authentication", receiverAttemptDecision{disposition: ReceiverDispositionSessionUnsafe}, protocolsession.PeerFailureSessionTerminal},
		{"runtime retired", receiverUnavailableDecision(), protocolsession.PeerFailureSessionTerminal},
		{"provider contract", receiverOperationDecision(ReceiverTerminalLocal, ReceiverProvenanceSignalingAdapterContract), protocolsession.PeerFailurePathTerminal},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := ReceiverAttemptOutcome{decision: test.decision}
			if outcome.RecoveryScope() != test.scope {
				t.Fatalf("scope=%s", outcome.RecoveryScope())
			}
		})
	}
	if scope := (ReceiverAttemptOutcome{decision: receiverAttemptDecision{recoveryScope: protocolsession.PeerFailureAttemptTransient}}).RecoveryScope(); scope != protocolsession.PeerFailureAttemptTransient {
		t.Fatal(scope)
	}
}

func TestReceiverImportsCoreSessionRejectionAuthority(t *testing.T) {
	provenance, ok := receiverProvenanceFromCore(sessionruntime.ReceiverPeerProvenanceRemoteSessionRejected)
	if !ok || provenance != ReceiverProvenanceRemoteSessionRejected {
		t.Fatal("lost sealed session rejection")
	}
	if !validReceiverBoundDecision(receiverAttemptDecision{
		transitionOwner: ReceiverTerminalRemote, transitionProvenance: provenance,
		disposition: ReceiverDispositionSessionUnsafe, consequenceProvenance: provenance,
	}) {
		t.Fatal("sealed session rejection invalidated")
	}
}
