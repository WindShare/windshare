package v2

import (
	"encoding/hex"
	"testing"
)

func TestCapacityWaitWireAndBounds(t *testing.T) {
	frame := CapacityWait{RelaySessionID: RelaySessionID{1, 2, 3, 4, 5, 6, 7, 8}, Direction: 1, Milliseconds: 15000}
	wire, err := frame.MarshalBinary()
	if err != nil || hex.EncodeToString(wire) != "5753325a02010000010203040506070800003a98" {
		t.Fatalf("wire: %x %v", wire, err)
	}
	parsed, err := ParseCapacityWait(wire)
	if err != nil || parsed != frame {
		t.Fatalf("decode: %+v %v", parsed, err)
	}
	for _, index := range []int{4, 5, 6, 7} {
		corrupt := append([]byte(nil), wire...)
		corrupt[index] = 3
		if _, err := ParseCapacityWait(corrupt); err == nil {
			t.Fatalf("accepted invalid header byte %d", index)
		}
	}
	frame.Milliseconds = MaxCapacityWaitMilliseconds + 1
	if _, err := frame.MarshalBinary(); err == nil {
		t.Fatal("unbounded wait")
	}
	frame.Milliseconds = 0
	if _, err := frame.MarshalBinary(); err != nil {
		t.Fatal("zero must clear", err)
	}
}
