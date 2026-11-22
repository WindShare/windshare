package protocolsession_test

import (
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/session/protocolsession"
	"golang.org/x/text/unicode/norm"
)

func TestSessionNFCIsNotThePinnedCatalogNamePolicy(t *testing.T) {
	pair := "\U00016D63\U00016D67"
	if norm.NFC.IsNormalString(pair) {
		t.Skip("runtime Unicode tables predate Kirat Rai composition")
	}
	id := protocolsession.OperationID(1)
	body, err := cbor.Marshal([]any{pair})
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocolsession.EncodeBody([]any{pair})
	requireNFCRejection(t, err)
	_, err = protocolsession.NewMessage(protocolsession.MessageListChildren, &id, body)
	requireNFCRejection(t, err)
	plaintext, err := cbor.Marshal(map[uint64]any{0: uint64(protocolsession.MessageListChildren), 1: id.Uint64(), 2: cbor.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = protocolsession.DecodeMessage(plaintext)
	requireNFCRejection(t, err)
}

func requireNFCRejection(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "NFC") {
		t.Fatalf("want NFC rejection, got %v", err)
	}
}
