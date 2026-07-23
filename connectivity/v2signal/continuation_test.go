package v2signal

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestOperationContinuationClassifierBindsCanonicalCandidateAndConfiguredLimit(t *testing.T) {
	binding := Binding{
		PeerPathID: PeerPathID(bytes.Repeat([]byte{0x31}, IdentityBytes)),
		AttemptID:  AttemptID(bytes.Repeat([]byte{0x32}, IdentityBytes)),
	}
	offerBody, err := EncodeOffer(Offer{Binding: binding, SDP: "v=0\r\ns=offer\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	classifier := OperationContinuationClassifier{MaximumCandidates: 32}
	authority, tracked, err := classifier.BeginOperationContinuation(
		protocolsession.MessagePeerOffer, offerBody,
	)
	if err != nil || !tracked || authority == nil {
		t.Fatalf("begin continuation authority: tracked=%v authority=%T error=%v", tracked, authority, err)
	}
	if maximum := authority.MaximumContinuations(); maximum != 32 {
		t.Fatalf("configured continuation maximum = %d, want 32", maximum)
	}
	if scope := authority.OperationContinuationScope(); scope == (protocolsession.OperationContinuationScope{}) {
		t.Fatal("offer authority exposed a zero continuation scope")
	}

	candidateBody, err := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:one"})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, tracked, err := authority.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, candidateBody,
	)
	if err != nil || !tracked || fingerprint == ([32]byte{}) {
		t.Fatalf("classify candidate: tracked=%v fingerprint=%x error=%v", tracked, fingerprint, err)
	}
	unboundScope, tracked, err := classifier.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate, candidateBody,
	)
	if err != nil || !tracked || unboundScope != authority.OperationContinuationScope() {
		t.Fatalf("unbound scope = %x, tracked=%v, error=%v", unboundScope, tracked, err)
	}

	distinctBody, _ := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:two"})
	distinctFingerprint, _, err := authority.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, distinctBody,
	)
	if err != nil || distinctFingerprint == fingerprint {
		t.Fatalf("distinct fingerprint = %x, error=%v", distinctFingerprint, err)
	}
	wrong := binding
	wrong.AttemptID[0]++
	wrongBody, _ := EncodeCandidate(Candidate{Binding: wrong, Candidate: "candidate:one"})
	if _, tracked, err := authority.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, wrongBody,
	); !tracked || !errors.Is(err, ErrSignalBinding) {
		t.Fatalf("wrong binding: tracked=%v error=%v", tracked, err)
	}
}

func TestReceiverControlValidatorPropagatesLowerContinuationLimitAndRejectsAboveProtocol(t *testing.T) {
	binding := Binding{
		PeerPathID: PeerPathID(bytes.Repeat([]byte{0x41}, IdentityBytes)),
		AttemptID:  AttemptID(bytes.Repeat([]byte{0x42}, IdentityBytes)),
	}
	offerBody, _ := EncodeOffer(Offer{Binding: binding, SDP: "v=0\r\ns=offer\r\n"})
	authority, tracked, err := (ReceiverControlValidator{MaximumCandidates: 32}).BeginOperationContinuation(
		protocolsession.MessagePeerOffer, offerBody,
	)
	maximum := 0
	if authority != nil {
		maximum = authority.MaximumContinuations()
	}
	if err != nil || !tracked || maximum != 32 {
		t.Fatalf("receiver continuation authority = %T, tracked=%v, max=%d, error=%v",
			authority, tracked, maximum, err)
	}
	if _, tracked, err := (ReceiverControlValidator{
		MaximumCandidates: MaximumCandidates + 1,
	}).BeginOperationContinuation(protocolsession.MessagePeerOffer, offerBody); !tracked ||
		!errors.Is(err, ErrContinuationLimit) {
		t.Fatalf("above-protocol continuation limit: tracked=%v error=%v", tracked, err)
	}
}
