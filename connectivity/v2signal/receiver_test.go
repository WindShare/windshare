package v2signal

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReceiverControlValidatorOwnsOnlyTypedSenderSignals(t *testing.T) {
	binding := Binding{
		PeerPathID: testSignalIdentity[PeerPathID](1), AttemptID: testSignalIdentity[AttemptID](2),
	}
	operation, _ := protocolsession.OperationIDFromBytes(bytes.Repeat([]byte{3}, protocolsession.IdentityBytes))
	answer, _ := EncodeAnswer(Answer{Binding: binding, SDP: "answer"})
	candidate, _ := EncodeCandidate(Candidate{Binding: binding, Candidate: "candidate:1"})
	validator := ReceiverControlValidator{}
	for kind, body := range map[protocolsession.MessageKind][]byte{
		protocolsession.MessagePeerAnswer:    answer,
		protocolsession.MessagePeerCandidate: candidate,
	} {
		if err := validator.ValidateSenderControl(kind, operation, body); err != nil {
			t.Fatalf("kind %d: %v", kind, err)
		}
	}
	if err := validator.ValidateSenderControl(protocolsession.MessagePeerAnswer, operation, []byte{0xf6}); err == nil {
		t.Fatal("malformed answer passed receiver validator")
	}
	if err := validator.ValidateSenderControl(protocolsession.MessageCatalogResult, operation, answer); err == nil {
		t.Fatal("non-peer control passed receiver validator")
	}
	if err := validator.ValidateSenderControl(protocolsession.MessagePeerAnswer, protocolsession.OperationID{}, answer); err == nil {
		t.Fatal("zero operation passed receiver validator")
	}
}

func testSignalIdentity[T ~[IdentityBytes]byte](value byte) T {
	var result T
	result[0] = value
	return result
}
