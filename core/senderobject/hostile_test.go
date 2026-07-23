package senderobject

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/link"
)

type codecFixture struct {
	binding    Binding
	key        []byte
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	nonce      []byte
	plaintext  []byte
	object     []byte
}

func newCodecFixture(t *testing.T) codecFixture {
	t.Helper()
	share := bytes.Repeat([]byte{1}, 16)
	file := bytes.Repeat([]byte{2}, 16)
	binding, err := NewRevisionBinding(share, file)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	fixture := codecFixture{
		binding: binding, key: bytes.Repeat([]byte{4}, 32), privateKey: privateKey,
		publicKey: privateKey.Public().(ed25519.PublicKey), nonce: bytes.Repeat([]byte{5}, NonceBytes),
		plaintext: []byte{0xa1, 0x00, 0x01},
	}
	fixture.object, err = Seal(fixture.binding, fixture.key, fixture.privateKey, fixture.nonce, fixture.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestHostileObjectHeadersFailBeforeCryptography(t *testing.T) {
	fixture := newCodecFixture(t)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, ErrMalformed},
		{"short", func(value []byte) []byte { return value[:FixedBytes+TagBytes-1] }, ErrMalformed},
		{"too-large", func([]byte) []byte { return make([]byte, MaxRevisionBytes+1) }, ErrTooLarge},
		{"wire", func(value []byte) []byte { value[0] = 3; return value }, ErrMalformed},
		{"flags", func(value []byte) []byte { value[1] = 1; return value }, ErrMalformed},
		{"reserved", func(value []byte) []byte { value[2] = 1; return value }, ErrMalformed},
		{"short-tag", func(value []byte) []byte { clear(value[4:8]); return value }, ErrMalformed},
		{"length", func(value []byte) []byte { value[7]++; return value }, ErrMalformed},
		{"trailing", func(value []byte) []byte { return append(value, 0) }, ErrMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hostile := test.mutate(bytes.Clone(fixture.object))
			if err := Verify(fixture.binding, fixture.publicKey, hostile); !errors.Is(err, test.want) {
				t.Fatalf("Verify error = %v, want %v", err, test.want)
			}
		})
	}
	if err := Verify(Binding{}, fixture.publicKey, fixture.object); !errors.Is(err, ErrBinding) {
		t.Fatalf("zero binding error = %v", err)
	}
	if err := Verify(fixture.binding, fixture.publicKey[:31], fixture.object); !errors.Is(err, ErrKey) {
		t.Fatalf("short public key error = %v", err)
	}
}

func TestSealRejectsEveryInvalidBoundaryWithoutProducingBytes(t *testing.T) {
	fixture := newCodecFixture(t)
	tests := []struct {
		name       string
		binding    Binding
		key        []byte
		privateKey ed25519.PrivateKey
		nonce      []byte
		plaintext  []byte
		want       error
	}{
		{"binding", Binding{}, fixture.key, fixture.privateKey, fixture.nonce, fixture.plaintext, ErrBinding},
		{"key", fixture.binding, fixture.key[:31], fixture.privateKey, fixture.nonce, fixture.plaintext, ErrKey},
		{"private-key", fixture.binding, fixture.key, fixture.privateKey[:63], fixture.nonce, fixture.plaintext, ErrKey},
		{"nonce", fixture.binding, fixture.key, fixture.privateKey, fixture.nonce[:11], fixture.plaintext, ErrKey},
		{"empty", fixture.binding, fixture.key, fixture.privateKey, fixture.nonce, nil, ErrMalformed},
		{"oversized", fixture.binding, fixture.key, fixture.privateKey, fixture.nonce, make([]byte, MaxRevisionBytes), ErrTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if object, err := Seal(test.binding, test.key, test.privateKey, test.nonce, test.plaintext); !errors.Is(err, test.want) || object != nil {
				t.Fatalf("Seal = %x, %v, want %v", object, err, test.want)
			}
		})
	}
	if _, err := objectAEAD(make([]byte, 7)); err == nil {
		t.Fatal("invalid AES key reached an object cipher")
	}
}

func TestDescriptorBootstrapNeverReleasesUnauthenticatedPlaintext(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x20}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	pkHash, _ := link.SenderKeyHash(publicKey)
	shareID, _ := link.ShareIDForSenderKeyHash(pkHash[:])
	shareRaw, _ := base64.RawURLEncoding.DecodeString(shareID)
	binding, _ := NewDescriptorBinding(pkHash[:], shareRaw)
	key := bytes.Repeat([]byte{0x31}, 32)
	plaintext := []byte{0xa1, 0x07, 0x58, 0x20}
	plaintext = append(plaintext, publicKey...)
	object, err := Seal(binding, key, privateKey, bytes.Repeat([]byte{0x41}, NonceBytes), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	callbackError := errors.New("descriptor schema rejected")
	tests := []struct {
		name     string
		binding  Binding
		key      []byte
		object   []byte
		callback func([]byte) (ed25519.PublicKey, error)
		want     error
	}{
		{"wrong-domain", newCodecFixture(t).binding, key, object, func([]byte) (ed25519.PublicKey, error) { return publicKey, nil }, ErrBinding},
		{"nil-callback", binding, key, object, nil, ErrBinding},
		{"callback-error", binding, key, object, func([]byte) (ed25519.PublicKey, error) { return nil, callbackError }, ErrKey},
		{"short-public-key", binding, key, object, func([]byte) (ed25519.PublicKey, error) { return make([]byte, 31), nil }, ErrKey},
		{"wrong-public-key", binding, key, object, func([]byte) (ed25519.PublicKey, error) {
			other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x22}, 32))
			return other.Public().(ed25519.PublicKey), nil
		}, ErrSignature},
		{"wrong-aead-key", binding, bytes.Repeat([]byte{0x32}, 32), object, func([]byte) (ed25519.PublicKey, error) { return publicKey, nil }, ErrAuth},
		{"short-aead-key", binding, key[:31], object, func([]byte) (ed25519.PublicKey, error) { return publicKey, nil }, ErrKey},
		{"bad-signature", binding, key, func() []byte {
			value := bytes.Clone(object)
			value[len(value)-1] ^= 1
			return value
		}(), func([]byte) (ed25519.PublicKey, error) { return publicKey, nil }, ErrSignature},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if opened, err := OpenDescriptorBootstrap(test.binding, test.key, test.object, test.callback); !errors.Is(err, test.want) || opened != nil {
				t.Fatalf("bootstrap = %x, %v, want %v", opened, err, test.want)
			}
		})
	}
}

func TestEveryBindingConstructorRejectsWrongSemanticAxes(t *testing.T) {
	identity := bytes.Repeat([]byte{1}, 16)
	zero := make([]byte, 16)
	pkHash := bytes.Repeat([]byte{2}, link.PKHashBytes)
	shareID, _ := link.ShareIDForSenderKeyHash(pkHash)
	shareRaw, _ := base64.RawURLEncoding.DecodeString(shareID)
	if _, err := NewDescriptorBinding(pkHash[:15], shareRaw); !errors.Is(err, ErrBinding) {
		t.Fatalf("short pkHash error = %v", err)
	}
	wrongShare := bytes.Clone(shareRaw)
	wrongShare[0] ^= 1
	if _, err := NewDescriptorBinding(pkHash, wrongShare); !errors.Is(err, ErrBinding) {
		t.Fatalf("wrong share mapping error = %v", err)
	}
	tests := []func() error{
		func() error { _, err := NewCatalogPageBinding(zero, identity, 0); return err },
		func() error { _, err := NewCatalogPageBinding(identity, zero, 0); return err },
		func() error { _, err := NewDirectoryErrorBinding(zero, identity); return err },
		func() error { _, err := NewRevisionBinding(identity, zero); return err },
		func() error { _, err := NewBlockRecordBinding(identity, identity, zero, 0, 0); return err },
		func() error { _, err := NewOfflineCommitBinding(zero); return err },
	}
	for index, test := range tests {
		if err := test(); !errors.Is(err, ErrBinding) {
			t.Fatalf("binding case %d error = %v", index, err)
		}
	}
}
