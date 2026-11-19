package protocolsession

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestControlSemanticDecodersRejectCanonicalSchemaViolations(t *testing.T) {
	attempt := catalog.ScanAttemptID{1}
	if _, err := EncodeScanProgress(ScanProgress{}); !errors.Is(err, ErrInvalidScanProgress) {
		t.Fatalf("zero scan attempt error = %v, want %v", err, ErrInvalidScanProgress)
	}
	for name, encoded := range map[string][]byte{
		"non-canonical": {0xa1, 0x00, 0x18, 0x01},
		"wrong field type": mustControlBody(t, map[uint64]any{
			0: "1", 1: attempt.Bytes(), 2: uint64(1),
		}),
		"invalid attempt": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: []byte{1}, 2: uint64(1),
		}),
	} {
		t.Run("scan "+name, func(t *testing.T) {
			if _, err := DecodeScanProgress(encoded); !errors.Is(err, ErrInvalidScanProgress) {
				t.Fatalf("DecodeScanProgress error = %v, want %v", err, ErrInvalidScanProgress)
			}
		})
	}

	if _, err := EncodeSessionTerminal(SessionTerminal{}); !errors.Is(err, ErrInvalidSessionTerminal) {
		t.Fatalf("empty terminal error = %v, want %v", err, ErrInvalidSessionTerminal)
	}
	for name, encoded := range map[string][]byte{
		"non-canonical": {0xa1, 0x00, 0x18, 0x01},
		"wrong field type": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: "4097", 2: "stopped",
		}),
		"semantic rejection": mustControlBody(t, map[uint64]any{
			0: uint64(1), 1: uint64(SessionTerminalCodeFirst), 2: "",
		}),
	} {
		t.Run("terminal "+name, func(t *testing.T) {
			if _, err := DecodeSessionTerminal(encoded); !errors.Is(err, ErrInvalidSessionTerminal) {
				t.Fatalf("DecodeSessionTerminal error = %v, want %v", err, ErrInvalidSessionTerminal)
			}
		})
	}
}

func largestControlSemanticAcceptedBy(t *testing.T, accept func([]byte) error) []byte {
	t.Helper()
	low, high := 0, MaxEnvelopePlaintextBytes
	var largest []byte
	for low <= high {
		middle := low + (high-low)/2
		semantic, err := EncodeBody(make([]byte, middle))
		if err != nil {
			t.Fatal(err)
		}
		if err := accept(semantic); err == nil {
			largest = semantic
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if largest == nil {
		t.Fatal("no canonical byte-string semantic body fit the control wrapper")
	}
	return largest
}
