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

func TestTransientFileIdentityOwnsItsOpaqueEncoding(t *testing.T) {
	source := []byte{0, 1, 2, 0xff}
	identity := NewTransientFileIdentity("native-file-id", source)
	source[0] = 9

	if identity.IsZero() {
		t.Fatal("complete identity reported zero")
	}
	if !identity.Equal(NewTransientFileIdentity("native-file-id", []byte{0, 1, 2, 0xff})) {
		t.Fatal("identity changed with constructor input")
	}
	if identity.Equal(NewTransientFileIdentity("other-backend", []byte{0, 1, 2, 0xff})) {
		t.Fatal("identical encoding from a different backend compared equal")
	}
	if identity.Equal(NewTransientFileIdentity("native-file-id", []byte{0, 1, 2, 3})) {
		t.Fatal("different native encoding compared equal")
	}
}

func TestTransientFileIdentityIncompleteValuesAreNeverComparable(t *testing.T) {
	valid := NewTransientFileIdentity("native-file-id", []byte{1})
	tests := []struct {
		name     string
		identity TransientFileIdentity
	}{
		{name: "zero value"},
		{name: "missing domain", identity: NewTransientFileIdentity("", []byte{1})},
		{name: "missing encoding", identity: NewTransientFileIdentity("native-file-id", nil)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.identity.IsZero() {
				t.Fatal("incomplete identity reported non-zero")
			}
			if test.identity.Equal(test.identity) {
				t.Fatal("incomplete identity compared equal to itself")
			}
			if test.identity.Equal(valid) || valid.Equal(test.identity) {
				t.Fatal("incomplete identity compared equal to a complete identity")
			}
		})
	}
}
