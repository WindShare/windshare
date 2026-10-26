package v2signal

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestOperationContinuationClassifierBindsCanonicalCandidateAndConfiguredLimit(t *testing.T) {
	binding := Binding{AttemptSequence: 1,
		PeerPathID: PeerPathID(bytes.Repeat([]byte{0x31}, IdentityBytes)),
		AttemptID:  AttemptID(bytes.Repeat([]byte{0x32}, IdentityBytes)),
	}
	offerBody, err := EncodeOffer(Offer{Binding: binding, SDP: "v=0\r\ns=offer\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	classifier := OperationContinuationClassifier{MaximumCandidates: 32}
	contract, tracked, err := classifier.BeginOperationContinuation(
		protocolsession.MessagePeerOffer, offerBody,
	)
	if err != nil || !tracked || contract == nil {
		t.Fatalf("begin continuation contract: tracked=%v contract=%T error=%v", tracked, contract, err)
	}
	if maximum := contract.MaximumContinuations(); maximum != 32 {
		t.Fatalf("configured continuation maximum = %d, want 32", maximum)
	}
	if scope := contract.OperationContinuationScope(); scope == (protocolsession.OperationContinuationScope{}) {
		t.Fatal("offer contract exposed a zero continuation scope")
	}

	candidateBody, err := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:one"})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, tracked, err := contract.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, candidateBody,
	)
	if err != nil || !tracked || fingerprint == ([32]byte{}) {
		t.Fatalf("classify candidate: tracked=%v fingerprint=%x error=%v", tracked, fingerprint, err)
	}
	unboundScope, tracked, err := classifier.ClassifyUnboundOperationContinuation(
		protocolsession.MessagePeerCandidate, candidateBody,
	)
	if err != nil || !tracked || unboundScope != contract.OperationContinuationScope() {
		t.Fatalf("unbound scope = %x, tracked=%v, error=%v", unboundScope, tracked, err)
	}

	distinctBody, _ := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:two"})
	distinctFingerprint, _, err := contract.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, distinctBody,
	)
	if err != nil || distinctFingerprint == fingerprint {
		t.Fatalf("distinct fingerprint = %x, error=%v", distinctFingerprint, err)
	}
	wrong := binding
	wrong.AttemptID[0]++
	wrongBody, _ := EncodeCandidate(Candidate{Binding: wrong, Candidate: "candidate:one"})
	if _, tracked, err := contract.ClassifyOperationContinuation(
		protocolsession.MessagePeerCandidate, wrongBody,
	); !tracked || !errors.Is(err, ErrSignalBinding) {
		t.Fatalf("wrong binding: tracked=%v error=%v", tracked, err)
	}
}

func TestReceiverControlValidatorPropagatesLowerContinuationLimitAndRejectsAboveProtocol(t *testing.T) {
	binding := Binding{AttemptSequence: 1,
		PeerPathID: PeerPathID(bytes.Repeat([]byte{0x41}, IdentityBytes)),
		AttemptID:  AttemptID(bytes.Repeat([]byte{0x42}, IdentityBytes)),
	}
	offerBody, _ := EncodeOffer(Offer{Binding: binding, SDP: "v=0\r\ns=offer\r\n"})
	contract, tracked, err := (ReceiverControlValidator{MaximumCandidates: 32}).BeginOperationContinuation(
		protocolsession.MessagePeerOffer, offerBody,
	)
	maximum := 0
	if contract != nil {
		maximum = contract.MaximumContinuations()
	}
	if err != nil || !tracked || maximum != 32 {
		t.Fatalf("receiver continuation contract = %T, tracked=%v, max=%d, error=%v",
			contract, tracked, maximum, err)
	}
	if _, tracked, err := (ReceiverControlValidator{
		MaximumCandidates: MaximumCandidates + 1,
	}).BeginOperationContinuation(protocolsession.MessagePeerOffer, offerBody); !tracked ||
		!errors.Is(err, ErrContinuationLimit) {
		t.Fatalf("above-protocol continuation limit: tracked=%v error=%v", tracked, err)
	}
}

func TestAttemptSequenceIsCanonicalAndPartOfContinuationScope(t *testing.T) {
	binding := testBinding()
	original := peerContinuationScope(binding)
	for _, sequence := range []uint64{1, 2, ^uint64(0)} {
		binding.AttemptSequence = sequence
		encoded, err := EncodeOffer(Offer{Binding: binding, SDP: "v=0"})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeOffer(encoded)
		if err != nil || decoded.Binding != binding {
			t.Fatal(decoded, err)
		}
		if sequence != 1 && peerContinuationScope(binding) == original {
			t.Fatal("sequence did not change continuation scope")
		}
	}
	binding.AttemptSequence = 0
	if _, err := EncodeOffer(Offer{Binding: binding, SDP: "v=0"}); err == nil {
		t.Fatal("zero attempt sequence accepted")
	}
}
