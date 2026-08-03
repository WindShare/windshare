package wire

import (
	"bytes"
	"testing"
)

func TestBoundedCaptureConsumesOverflowAndRetainsPrefix(t *testing.T) {
	capture := NewBoundedCapture(3)
	written, err := capture.Write([]byte("abcde"))
	if err != nil || written != 5 {
		t.Fatalf("Write() = (%d, %v), want (5, nil)", written, err)
	}
	if got := capture.Snapshot(); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("Snapshot() = %q, want %q", got, "abc")
	}
	if !capture.Exceeded() {
		t.Fatal("Exceeded() = false, want true")
	}
	snapshot := capture.Snapshot()
	snapshot[0] = 'z'
	if got := capture.Snapshot(); !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("Snapshot() retained caller mutation: %q", got)
	}
}
