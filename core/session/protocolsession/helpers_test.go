package protocolsession

import (
	"crypto/ed25519"
	"testing"

	"github.com/windshare/windshare/core/senderauth"
)

func testOperationID(seed byte) OperationID {
	return OperationID(uint64(seed) + 1)
}

func mustMessage(t *testing.T, kind MessageKind, operationID *OperationID, body any) Message {
	t.Helper()
	encoded, err := EncodeBody(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	message, err := NewMessage(kind, operationID, encoded)
	if err != nil {
		t.Fatalf("new message: %v", err)
	}
	return message
}

func checkedSender(t *testing.T, publicKey ed25519.PublicKey) *senderauth.Verifier {
	t.Helper()
	verifier, err := senderauth.NewVerifier(publicKey)
	if err != nil {
		t.Fatalf("new sender verifier: %v", err)
	}
	return verifier
}
