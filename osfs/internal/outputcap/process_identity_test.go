package outputcap

import (
	"bytes"
	"errors"
	"testing"
)

func TestProcessIdentityIsExplicitlyDistinctFromPersistentIdentity(t *testing.T) {
	nonce := bytes.Repeat([]byte{1}, DestinationAuthorityIDBytes)
	first, err := NewProcessDestinationAuthorityID(nonce)
	if err != nil || first.IsZero() {
		t.Fatalf("process id=%v %v", first, err)
	}
	next, err := NewProcessDestinationAuthorityID(bytes.Repeat([]byte{2}, DestinationAuthorityIDBytes))
	if err != nil || first == next {
		t.Fatalf("distinct process id=%v %v", next, err)
	}
	persistent, err := NewDestinationAuthorityID(nonce, nonce)
	if err != nil || first == persistent {
		t.Fatal("process identity shares a persistent identity domain")
	}
	for _, invalid := range [][]byte{nil, nonce[:len(nonce)-1], make([]byte, DestinationAuthorityIDBytes)} {
		if _, err := NewProcessDestinationAuthorityID(invalid); !errors.Is(err, ErrInvalidDestinationAuthorityID) {
			t.Fatalf("invalid nonce=%x err=%v", invalid, err)
		}
	}
}
