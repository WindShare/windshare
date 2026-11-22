package v2signal

import (
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverControlValidatorClassifiesPeerAttemptContinuations(t *testing.T) {
	validator := ReceiverControlValidator{}
	binding := testBinding()
	offerBody, err := EncodeOffer(Offer{Binding: binding, SDP: "v=0"})
	if err != nil {
		t.Fatal(err)
	}
	contract, tracked, err := validator.BeginOperationContinuation(protocolsession.MessagePeerOffer, offerBody)
	if err != nil || !tracked || contract == nil {
		t.Fatalf("begin continuation: tracked=%v err=%v", tracked, err)
	}
	authority, ok := contract.(protocolsession.PeerAttemptAuthority)
	if !ok {
		t.Fatalf("contract %T does not expose PeerAttemptAuthority", contract)
	}
	wantBinding := protocolBinding(binding)
	if got := authority.PeerAttemptBinding(); got != wantBinding {
		t.Fatalf("peer attempt binding=%+v want %+v", got, wantBinding)
	}

	answerBody, err := EncodeAnswer(Answer{Binding: binding, SDP: "v=0"})
	if err != nil {
		t.Fatal(err)
	}
	candidateBody, err := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:late"})
	if err != nil {
		t.Fatal(err)
	}
	failureBody, err := protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{
		Scope: protocolsession.OperationScopePeer, Code: protocolsession.PeerOperationCodeAuthentication,
		Message: "late failure", PeerAttempt: &wantBinding,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind protocolsession.MessageKind
		body []byte
	}{
		{protocolsession.MessagePeerOffer, offerBody},
		{protocolsession.MessagePeerAnswer, answerBody},
		{protocolsession.MessagePeerCandidate, candidateBody},
	} {
		got, classified, err := validator.ClassifyPeerAttemptContinuation(tc.kind, tc.body)
		if err != nil || !classified || got != wantBinding {
			t.Fatalf("classify kind=%d: got=%+v classified=%v err=%v", tc.kind, got, classified, err)
		}
	}
	if decodedFailure, err := protocolsession.DecodeOperationFailure(failureBody); err != nil || decodedFailure.PeerAttempt == nil || *decodedFailure.PeerAttempt != wantBinding {
		t.Fatalf("decode peer failure: %+v err=%v", decodedFailure, err)
	}
	if _, tracked, err := contract.ClassifyOperationContinuation(protocolsession.MessagePeerCandidate, candidateBody); err != nil || !tracked {
		t.Fatalf("candidate continuation: tracked=%v err=%v", tracked, err)
	}
	other := binding
	other.AttemptSequence++
	other.AttemptID[0]++
	otherCandidate, err := EncodeCandidate(Candidate{Binding: other, Candidate: "candidate:other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := contract.ClassifyOperationContinuation(protocolsession.MessagePeerCandidate, otherCandidate); err == nil {
		t.Fatal("mismatched binding candidate was accepted")
	}
}
