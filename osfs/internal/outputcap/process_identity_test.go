package outputcap

import (
	"bytes"
	"errors"
	"testing"
)

func TestProcessIdentityIsExplicitlyDistinctFromPersistentIdentity(t *testing.T) {
	nonce := bytes.Repeat([]byte{1}, DestinationBindingIDBytes)
	first, err := NewProcessDestinationBindingID(nonce)
	if err != nil || first.IsZero() {
		t.Fatalf("process id=%v %v", first, err)
	}
	next, err := NewProcessDestinationBindingID(bytes.Repeat([]byte{2}, DestinationBindingIDBytes))
	if err != nil || first == next {
		t.Fatalf("distinct process id=%v %v", next, err)
	}
	persistent, err := NewDestinationBindingID(nonce, nonce)
	if err != nil || first == persistent {
		t.Fatal("process identity shares a persistent identity domain")
	}
	for _, invalid := range [][]byte{nil, nonce[:len(nonce)-1], make([]byte, DestinationBindingIDBytes)} {
		if _, err := NewProcessDestinationBindingID(invalid); !errors.Is(err, ErrInvalidDestinationBindingID) {
			t.Fatalf("invalid nonce=%x err=%v", invalid, err)
		}
	}
}
