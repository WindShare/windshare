package sessioncore

import (
	"bytes"
	"errors"
	"testing"

	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestReadNonzeroLaneNonceRetriesZeroRandomness(t *testing.T) {
	readerBytes := append(make([]byte, protocolsession.LaneAttachNonceBytes), bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)...)
	nonce, err := ReadNonzeroLaneNonce(bytes.NewReader(readerBytes), protocolsession.LaneAttachNonceBytes)
	if err != nil || !bytes.Equal(nonce, bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)) {
		t.Fatalf("retried nonce = %x, %v", nonce, err)
	}
	if _, err := ReadNonzeroLaneNonce(bytes.NewReader(make([]byte, laneNonceAttempts)), 1); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("zero nonce source error = %v", err)
	}
}
