package senderobject

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/internal/keyderiv"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderauth"
)

type descriptorFixture struct {
	pkHash     []byte
	shareID    []byte
	key        []byte
	privateKey ed25519.PrivateKey
	sender     *senderauth.Verifier
	nonce      []byte
	plaintext  []byte
	object     []byte
}

func newDescriptorFixture(t *testing.T) descriptorFixture {
	t.Helper()
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	pkHash, err := link.SenderKeyHash(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	shareID, err := link.ShareIDForSenderKeyHash(pkHash[:])
	if err != nil {
		t.Fatal(err)
	}
	shareRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := descriptorFixture{
		pkHash: pkHash[:], shareID: shareRaw, key: bytes.Repeat([]byte{4}, keyderiv.KeyBytes),
		privateKey: privateKey, sender: checkedSender(t, publicKey), nonce: bytes.Repeat([]byte{5}, NonceBytes),
		plaintext: append([]byte{0xa1, 0x07, 0x58, 0x20}, publicKey...),
	}
	fixture.object, err = SealDescriptor(fixture.pkHash, fixture.shareID, fixture.key, privateKey, fixture.nonce, fixture.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture descriptorFixture) identity([]byte) (*senderauth.Verifier, error) {
	return fixture.sender, nil
}

func TestHostileDescriptorHeadersFailBeforeIdentityLookup(t *testing.T) {
	fixture := newDescriptorFixture(t)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{"empty", func([]byte) []byte { return nil }, ErrMalformed},
		{"short", func(value []byte) []byte { return value[:fixedBytes+tagBytes-1] }, ErrMalformed},
		{"too-large", func([]byte) []byte { return make([]byte, maxDescriptorBytes+1) }, ErrTooLarge},
		{"previous-wire", func(value []byte) []byte { value[0] = wireVersion - 1; return value }, ErrMalformed},
		{"future-wire", func(value []byte) []byte { value[0] = wireVersion + 1; return value }, ErrMalformed},
		{"flags", func(value []byte) []byte { value[1] = 1; return value }, ErrMalformed},
		{"reserved", func(value []byte) []byte { value[2] = 1; return value }, ErrMalformed},
		{"short-tag", func(value []byte) []byte { clear(value[4:headerBytes]); return value }, ErrMalformed},
		{"length", func(value []byte) []byte { value[headerBytes-1]++; return value }, ErrMalformed},
		{"trailing", func(value []byte) []byte { return append(value, 0) }, ErrMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hostile := test.mutate(bytes.Clone(fixture.object))
			opened, err := OpenDescriptorBootstrap(fixture.pkHash, fixture.shareID, fixture.key, hostile, func([]byte) (*senderauth.Verifier, error) {
				t.Fatal("invalid envelope reached identity lookup")
				return nil, nil
			})
			if !errors.Is(err, test.want) || opened != nil {
				t.Fatalf("hostile descriptor = %x, %v, want %v", opened, err, test.want)
			}
		})
	}
}

func TestSealDescriptorRejectsInvalidBoundariesWithoutProducingBytes(t *testing.T) {
	fixture := newDescriptorFixture(t)
	tests := []struct {
		name      string
		pkHash    []byte
		key       []byte
		signing   ed25519.PrivateKey
		nonce     []byte
		plaintext []byte
		want      error
	}{
		{"binding", nil, fixture.key, fixture.privateKey, fixture.nonce, fixture.plaintext, ErrBinding},
		{"key", fixture.pkHash, fixture.key[:31], fixture.privateKey, fixture.nonce, fixture.plaintext, ErrKey},
		{"private-key", fixture.pkHash, fixture.key, fixture.privateKey[:63], fixture.nonce, fixture.plaintext, ErrKey},
		{"nonce", fixture.pkHash, fixture.key, fixture.privateKey, fixture.nonce[:11], fixture.plaintext, ErrKey},
		{"empty", fixture.pkHash, fixture.key, fixture.privateKey, fixture.nonce, nil, ErrMalformed},
		{"oversized", fixture.pkHash, fixture.key, fixture.privateKey, fixture.nonce, make([]byte, maxDescriptorBytes), ErrTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sealed, err := SealDescriptor(test.pkHash, fixture.shareID, test.key, test.signing, test.nonce, test.plaintext)
			if !errors.Is(err, test.want) || sealed != nil {
				t.Fatalf("sealed = %x, %v, want %v", sealed, err, test.want)
			}
		})
	}
}

func TestDescriptorBootstrapNeverReleasesUnauthenticatedPlaintext(t *testing.T) {
	fixture := newDescriptorFixture(t)
	wrongKey := bytes.Clone(fixture.key)
	wrongKey[0] ^= 1
	otherPrivate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x22}, ed25519.SeedSize))
	other := checkedSender(t, otherPrivate.Public().(ed25519.PublicKey))
	tests := []struct {
		name     string
		key      []byte
		callback func([]byte) (*senderauth.Verifier, error)
		want     error
	}{
		{"nil-callback", fixture.key, nil, ErrBinding},
		{"callback-error", fixture.key, func([]byte) (*senderauth.Verifier, error) {
			return nil, errors.New("descriptor schema rejected")
		}, ErrKey},
		{"unchecked-sender", fixture.key, func([]byte) (*senderauth.Verifier, error) { return new(senderauth.Verifier), nil }, ErrKey},
		{"wrong-public-key", fixture.key, func([]byte) (*senderauth.Verifier, error) { return other, nil }, ErrSignature},
		{"wrong-aead-key", wrongKey, fixture.identity, ErrAuth},
		{"short-aead-key", fixture.key[:31], fixture.identity, ErrKey},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened, err := OpenDescriptorBootstrap(fixture.pkHash, fixture.shareID, test.key, fixture.object, test.callback)
			if !errors.Is(err, test.want) || opened != nil {
				t.Fatalf("bootstrap = %x, %v, want %v", opened, err, test.want)
			}
		})
	}
}

func TestDescriptorBootstrapRejectsInvalidRoute(t *testing.T) {
	fixture := newDescriptorFixture(t)
	wrongShare := bytes.Clone(fixture.shareID)
	wrongShare[0] ^= 1
	tests := []struct {
		name    string
		pkHash  []byte
		shareID []byte
	}{
		{"short-pkHash", fixture.pkHash[:link.PKHashBytes-1], fixture.shareID},
		{"short-share-id", fixture.pkHash, fixture.shareID[:link.SenderAuthenticatedShareIDBytes-1]},
		{"wrong-share-id", fixture.pkHash, wrongShare},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened, err := OpenDescriptorBootstrap(test.pkHash, test.shareID, fixture.key, fixture.object, fixture.identity)
			if !errors.Is(err, ErrBinding) || opened != nil {
				t.Fatalf("invalid route = %x, %v", opened, err)
			}
		})
	}
}

func checkedSender(t testing.TB, publicKey []byte) *senderauth.Verifier {
	t.Helper()
	sender, err := senderauth.NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return sender
}
