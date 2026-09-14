package v2

import (
	"encoding/hex"
	"testing"
)

func TestReceiveCreditWireContract(t *testing.T) {
	credit := ReceiveCredit{RelaySessionID: RelaySessionID{1, 2, 3, 4, 5, 6, 7, 8},
		Frames: ReceiveWindowFrames, Bytes: ReceiveWindowBytes}
	encoded, err := credit.MarshalBinary()
	const golden = "575332420200000001020304050607080000010001000000"
	if err != nil || hex.EncodeToString(encoded) != golden {
		t.Fatalf("receive credit=%x error=%v", encoded, err)
	}
	decoded, err := ParseReceiveCredit(encoded)
	if err != nil || decoded != credit {
		t.Fatalf("decoded=%+v error=%v", decoded, err)
	}
	for _, delta := range []ReceiveCredit{
		{RelaySessionID: credit.RelaySessionID, Frames: 1},
		{RelaySessionID: credit.RelaySessionID, Bytes: 1},
	} {
		wire, err := delta.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if got, err := ParseReceiveCredit(wire); err != nil || got != delta {
			t.Fatalf("independent credit=%+v error=%v", got, err)
		}
	}
}

func TestReceiveCreditRejectsMalformedAndUnboundedInput(t *testing.T) {
	id := RelaySessionID{1}
	for _, credit := range []ReceiveCredit{
		{}, {Frames: 1}, {RelaySessionID: id},
		{RelaySessionID: id, Frames: ReceiveWindowFrames + 1},
		{RelaySessionID: id, Bytes: ReceiveWindowBytes + 1},
	} {
		if _, err := credit.MarshalBinary(); err == nil {
			t.Fatalf("accepted %+v", credit)
		}
	}
	valid, _ := (ReceiveCredit{RelaySessionID: id, Frames: 1, Bytes: 1}).MarshalBinary()
	for _, offset := range []int{0, 4, 5, 6, 7, 16, 20} {
		malformed := append([]byte(nil), valid...)
		malformed[offset] = 0xff
		if _, err := ParseReceiveCredit(malformed); err == nil {
			t.Fatalf("accepted mutation at %d", offset)
		}
	}
	for _, wire := range [][]byte{nil, valid[:len(valid)-1], append(valid, 0), make([]byte, ReceiveCreditBytes)} {
		if _, err := ParseReceiveCredit(wire); err == nil {
			t.Fatalf("accepted %x", wire)
		}
	}
	zeroID := append([]byte(nil), valid...)
	clear(zeroID[8:16])
	if _, err := ParseReceiveCredit(zeroID); err == nil {
		t.Fatal("accepted zero session")
	}
}
