package content

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestDerivedKeyLifecycleAndNilSafe(t *testing.T) {
	var nilKey *DerivedKey
	nilKey.Destroy()

	key := &DerivedKey{value: [DerivedKeyBytes]byte{1, 2, 3, 4}}
	bytesCopy := key.Bytes()
	if !bytes.Equal(bytesCopy, []byte{1, 2, 3, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("unexpected key bytes: %v", bytesCopy)
	}
	key.Destroy()
	if !bytes.Equal(key.Bytes(), make([]byte, DerivedKeyBytes)) {
		t.Fatalf("key not zeroed after destroy: %v", key.Bytes())
	}
}

func TestKeyTreeLifecycleAndValidation(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, ReadSecretBytes)
	share := catalogID[catalog.ShareInstance](10)
	pkHash := bytes.Repeat([]byte{0x55}, PKHashBytes)

	if _, err := NewKeyTree(secret[:10], share); err == nil {
		t.Fatal("expected error for short read secret")
	}
	if _, err := NewKeyTree(secret, catalog.ShareInstance{}); err == nil {
		t.Fatal("expected error for zero share instance")
	}
	tree, err := NewKeyTree(secret, share)
	if err != nil {
		t.Fatal(err)
	}

	var nilTree *KeyTree
	nilTree.Destroy()
	if _, err := nilTree.DescriptorKey(pkHash); err == nil {
		t.Fatal("nil tree derived a descriptor key")
	}
	if _, err := nilTree.SessionAuthKey(); err == nil {
		t.Fatal("nil tree derived a session-auth key")
	}
	if _, err := tree.DescriptorKey(pkHash[:10]); err == nil {
		t.Fatal("expected error for short pkHash")
	}

	descriptorKey, err := tree.DescriptorKey(pkHash)
	if err != nil {
		t.Fatal(err)
	}
	descriptorKey.Destroy()
	sessionKey, err := tree.SessionAuthKey()
	if err != nil {
		t.Fatal(err)
	}
	sessionKey.Destroy()

	tree.Destroy()
	if _, err := tree.DescriptorKey(pkHash); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("destroyed descriptor derivation: %v", err)
	}
	if _, err := tree.SessionAuthKey(); !errors.Is(err, ErrKeyTreeDestroyed) {
		t.Fatalf("destroyed session-auth derivation: %v", err)
	}
}

func TestOwnDerivedKeyEdgeCases(t *testing.T) {
	wantErr := errors.New("underlying error")
	if _, err := ownDerivedKey(nil, wantErr); !errors.Is(err, wantErr) {
		t.Fatalf("ownDerivedKey error = %v, want %v", err, wantErr)
	}
	if _, err := ownDerivedKey([]byte{1, 2, 3}, nil); err == nil {
		t.Fatal("ownDerivedKey accepted short slice")
	}
}

func directHKDF(t *testing.T, secret []byte, label string, context []byte) []byte {
	t.Helper()
	info := append(append([]byte(label), 0), context...)
	key, err := hkdf.Key(sha256.New, secret, nil, string(info), DerivedKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
