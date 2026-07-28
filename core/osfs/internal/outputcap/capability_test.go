package outputcap

import (
	"bytes"
	"testing"
)

func TestPersistentDirectoryIdentityOwnsItsOpaqueEncoding(t *testing.T) {
	source := []byte{0, 1, 2, 0xff}
	identity := NewPersistentDirectoryIdentity(source)
	source[0] = 9
	if got := identity.Bytes(); !bytes.Equal(got, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("identity changed with constructor input: %x", got)
	}

	exposed := identity.Bytes()
	exposed[1] = 9
	if got := identity.Bytes(); !bytes.Equal(got, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("identity changed through returned bytes: %x", got)
	}
	if identity.IsZero() {
		t.Fatal("non-empty identity reported zero")
	}
	if !identity.Equal(NewPersistentDirectoryIdentity([]byte{0, 1, 2, 0xff})) {
		t.Fatal("byte-identical identity did not compare equal")
	}
	if identity.Equal(NewPersistentDirectoryIdentity([]byte{0, 1, 2, 3})) {
		t.Fatal("different identity compared equal")
	}
}

func TestPersistentDirectoryIdentityZeroValueIsEmpty(t *testing.T) {
	var identity PersistentDirectoryIdentity
	if !identity.IsZero() || len(identity.Bytes()) != 0 {
		t.Fatalf("zero identity = %x", identity.Bytes())
	}
}
