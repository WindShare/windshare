package protocolsession_test

import (
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turncontrol"
	"golang.org/x/text/unicode/norm"
)

func TestSessionNFCIsNotThePinnedCatalogNamePolicy(t *testing.T) {
	pair := "\U00016D63\U00016D67"
	if norm.NFC.IsNormalString(pair) {
		t.Skip("runtime Unicode tables predate Kirat Rai composition")
	}
	id := protocolsession.OperationID{1}
	body, err := cbor.Marshal([]any{pair})
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocolsession.EncodeBody([]any{pair})
	requireNFCRejection(t, err)
	_, err = protocolsession.NewMessage(protocolsession.MessageListChildren, &id, body)
	requireNFCRejection(t, err)
	plaintext, err := cbor.Marshal(map[uint64]any{0: uint64(protocolsession.MessageListChildren), 1: id.Bytes(), 2: cbor.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocolsession.DecodeMessage(plaintext)
	requireNFCRejection(t, err)

	// The TURN codec itself only bounds UTF-8. The session envelope enforces NFC.
	header := turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}
	for _, control := range []turncontrol.Message{
		turncontrol.Unavailable{Authority: header, Profile: "default", Reason: pair},
		turncontrol.Grant{Authority: header, PathID: [16]byte{3}, Provider: "provider", Account: "account", Profile: "default",
			CredentialKind: turncontrol.Fixed, Username: pair, Password: "password", URLs: []string{"turn:example.com"}},
	} {
		turnBody, err := turncontrol.Encode(control)
		if err != nil {
			t.Fatal(err)
		}
		_, err = protocolsession.NewMessage(protocolsession.MessageTURNControl, nil, turnBody)
		requireNFCRejection(t, err)
		plaintext, err = cbor.Marshal(map[uint64]any{0: uint64(protocolsession.MessageTURNControl), 1: nil, 2: cbor.RawMessage(turnBody)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = protocolsession.DecodeMessage(plaintext)
		requireNFCRejection(t, err)
	}
}

func requireNFCRejection(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "NFC") {
		t.Fatalf("want NFC rejection, got %v", err)
	}
}
