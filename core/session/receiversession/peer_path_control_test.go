package receiversession

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestKnownPeerSessionFailureSealsUnsafeAuthority(t *testing.T) {
	for _, code := range []uint16{protocolsession.PeerOperationCodeAuthentication, protocolsession.PeerOperationCodeSessionInvariant, 0x5fff} {
		fixture := newReceiverPeerTerminalFixture(t, byte(code))
		semantic, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{Scope: protocolsession.OperationScopePeer, PeerAttempt: &protocolsession.PeerAttemptBinding{PeerPathID: [16]byte{1}, AttemptID: [16]byte{2}, AttemptSequence: 1}, Code: code, Message: "remote peer failure"})
		if err != nil {
			t.Fatal(err)
		}
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
		message := signedPeerOperationControl(t, &Runtime{core: fixture.runtime}, key, protocolsession.MessageOperationError, fixture.operation.OperationID(), semantic)
		enqueueCallResponse(fixture.call, message)
		terminal := requireReceiverPeerTermination(t, fixture.operation.Receive(context.Background()))
		if !fixture.operation.OwnsTermination(terminal) {
			t.Fatal("unsealed termination")
		}
		if code == 0x5fff {
			if terminal.Severity() != PeerTerminalOperationOnly || fixture.runtime.Lifetime().Err() != nil {
				t.Fatal("unknown reason escalated beyond path")
			}
		} else if terminal.Severity() != PeerTerminalSessionUnsafe || terminal.ConsequenceProvenance() != PeerProvenanceRemoteSessionRejected || fixture.runtime.Lifetime().Err() == nil {
			t.Fatalf("known session failure not sealed: %+v", terminal)
		}
	}
}
