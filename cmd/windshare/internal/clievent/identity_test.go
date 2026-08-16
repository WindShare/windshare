package clievent

import (
	"bytes"
	"strings"
	"testing"
)

func TestSemanticIdentitiesPreserveAllCanonicalBytes(t *testing.T) {
	raw := []byte{0x01, 0x12, 0x23, 0x34, 0x45, 0x56, 0x67, 0x78, 0x89, 0x9a, 0xab, 0xbc, 0xcd, 0xde, 0xef, 0xf0}
	wantHex := "0112233445566778899aabbccddeeff0"
	tests := []struct {
		name string
		new  func([]byte) ([]byte, string, bool, error)
	}{
		{"receive operation", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewReceiveOperationID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
		{"protocol session", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewProtocolSessionID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
		{"protocol operation", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewProtocolOperationID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
		{"transfer job", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewTransferJobID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
		{"peer path", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewPeerPathID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
		{"peer attempt", func(value []byte) ([]byte, string, bool, error) {
			id, err := NewPeerAttemptID(value)
			return id.Bytes(), id.Hex(), id.Valid(), err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, encoded, valid, err := test.new(raw)
			if err != nil || !valid || !bytes.Equal(got, raw) || encoded != wantHex {
				t.Fatalf("identity projection = %x %q valid=%t err=%v", got, encoded, valid, err)
			}
			got[0] = 0xff
			fresh, _, _, err := test.new(raw)
			if err != nil || fresh[0] != raw[0] {
				t.Fatalf("identity bytes alias caller storage: %x", fresh)
			}
			for _, invalid := range [][]byte{nil, make([]byte, IdentityBytes-1), make([]byte, IdentityBytes), make([]byte, IdentityBytes+1)} {
				if _, _, valid, err := test.new(invalid); err == nil || valid {
					t.Fatalf("accepted invalid identity length/content %d", len(invalid))
				}
			}
		})
	}
	if len(wantHex) != IdentityBytes*2 || strings.HasSuffix(wantHex, "...") {
		t.Fatalf("test fixture did not assert a full canonical identity: %q", wantHex)
	}
}

func TestLaneIdentityRetainsTranscriptEpochWithoutAllowingZeroLane(t *testing.T) {
	identity, err := NewLaneIdentity(7, 0)
	if err != nil || identity.ID() != 7 || identity.Epoch() != 0 || !identity.Valid() {
		t.Fatalf("transcript lane = %+v err=%v", identity, err)
	}
	if _, err := NewLaneIdentity(0, 1); err == nil {
		t.Fatal("accepted zero lane identity")
	}
}
