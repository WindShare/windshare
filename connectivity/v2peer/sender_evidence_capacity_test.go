package v2peer

import (
	pion "github.com/pion/webrtc/v4"
	"testing"

	"github.com/windshare/windshare/connectivity/v2signal"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestSenderPathMembershipBoundsNewPathsWithoutPoisoningRecovery(t *testing.T) {
	authority := newSenderEvidenceAuthority(2)
	operation := testPeerOperation(testOperationID(1))
	first, second, overflow := testBinding(2), testBinding(3), testBinding(4)
	for _, binding := range []v2signal.Binding{first, second} {
		if claim := authority.claim(operation, binding); !claim.acquired || claim.capacity {
			t.Fatal(claim)
		}
	}
	if claim := authority.claim(operation, overflow); claim.acquired || !claim.capacity {
		t.Fatal(claim)
	}
	for sequence := uint64(2); sequence < 10000; sequence++ {
		next := first
		next.AttemptSequence = sequence
		next.AttemptID[0] = byte(sequence)
		if next.AttemptID == (v2signal.AttemptID{}) {
			next.AttemptID[0] = 1
		}
		if claim := authority.claim(operation, next); !claim.acquired || claim.capacity {
			t.Fatal(sequence, claim)
		}
	}
	if !authority.claimed(first) || authority.retainedIdentityCount() != 2 {
		t.Fatal("lost bounded retirement watermark")
	}
	if authority.claimed(overflow) {
		t.Fatal("unadmitted path acquired authority")
	}
	authority.reset()
	if authority.retainedIdentityCount() != 0 {
		t.Fatal("session release retained paths")
	}
}
func TestSenderPathCapacityRejectsOnlyNewPath(t *testing.T) {
	peer := newTestPeerConnection()
	factory := mustTestFactory(t, Config{MaxPeerPaths: 1, PeerConnections: PeerConnectionFactoryFunc(func(_ pion.Configuration) (PeerConnection, error) { return peer, nil })})
	session := newTestPeerSession(10)
	handler, ctx, cancel, done := startSenderTestRuntime(t, factory, session)
	_, _, _ = sendSenderTestOffer(t, handler, ctx, 11)
	receiveTest(t, peer.remote)
	receiveTest(t, session.controls)
	operation, _, _ := sendSenderTestOffer(t, handler, ctx, 13)
	failure := receiveTest(t, session.failures)
	if failure.operation != operation || failure.code != protocolsession.PeerOperationCodePolicy {
		t.Fatal(failure)
	}
	select {
	case err := <-done:
		t.Fatalf("new path killed session: %v", err)
	default:
	}
	stopSenderTestRuntime(t, cancel, done)
}
func TestSenderPathMembershipConfiguration(t *testing.T) {
	for _, limit := range []int{-1, SenderMaxPeerPaths + 1} {
		if _, err := NewFactory(Config{MaxPeerPaths: limit}); err == nil {
			t.Fatal(limit)
		}
	}
	if _, err := NewFactory(Config{MaxPeerPaths: 1}); err != nil {
		t.Fatal(err)
	}
	var authority *senderEvidenceAuthority
	if authority.claimed(testBinding(1)) || authority.retainedIdentityCount() != 0 || authority.claim(peerOperation{}, testBinding(1)).acquired {
		t.Fatal("nil authority")
	}
	authority.reset()
}
