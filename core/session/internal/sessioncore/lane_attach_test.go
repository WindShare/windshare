package sessioncore

import (
	"bytes"
	"errors"
	sessioncontract "github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
)

func TestLaneAttachBodiesRejectAlternateSemanticsAndZeroNonces(t *testing.T) {
	requestBody, err := EncodeLaneAttachRequest(7)
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeLaneAttachRequest(requestBody)
	if err != nil || request.requestedLaneID != 7 {
		t.Fatalf("request = %+v, %v", request, err)
	}
	hostile, _ := protocolsession.EncodeBody(map[uint64]any{
		0: laneAttachBodyVersion, 1: laneAttachRequestMode, 2: uint64(7), 3: uint64(0),
	})
	if _, err := DecodeLaneAttachRequest(hostile); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("extra request field error = %v", err)
	}
	grant := protocolsession.LaneGrant{
		LaneID: 7, LaneEpoch: 3, OperationID: id16[protocolsession.OperationID](9),
	}
	if _, err := EncodeLaneGrant(grant); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("zero grant nonce error = %v", err)
	}
	copy(grant.AttachNonce[:], bytes.Repeat([]byte{1}, protocolsession.LaneAttachNonceBytes))
	grantBody, err := EncodeLaneGrant(grant)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeLaneGrant(grantBody, grant.OperationID)
	if err != nil || decoded.LaneID != grant.LaneID || decoded.LaneEpoch != grant.LaneEpoch || decoded.AttachNonce != grant.AttachNonce {
		t.Fatalf("grant = %+v, %v", decoded, err)
	}
	readerBytes := append(make([]byte, protocolsession.LaneAttachNonceBytes), bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)...)
	nonce, err := ReadNonzeroLaneNonce(bytes.NewReader(readerBytes), protocolsession.LaneAttachNonceBytes)
	if err != nil || !bytes.Equal(nonce, bytes.Repeat([]byte{2}, protocolsession.LaneAttachNonceBytes)) {
		t.Fatalf("retried nonce = %x, %v", nonce, err)
	}
	if _, err := ReadNonzeroLaneNonce(bytes.NewReader(make([]byte, laneNonceAttempts)), 1); !errors.Is(err, sessioncontract.ErrRuntimeConfig) {
		t.Fatalf("zero nonce source error = %v", err)
	}
}
