package senderobject

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/senderauth"
)

func TestDescriptorSignatureCommitsToTheBoundSealedBytes(t *testing.T) {
	fixture := newDescriptorFixture(t)
	prefixEnd := len(fixture.object) - signatureBytes
	prefix := fixture.object[:prefixEnd]
	context := append([]byte{link.SuiteSenderAuthenticated}, fixture.pkHash...)
	context = append(context, fixture.shareID...)
	contextHash := sha256.Sum256(context)
	objectHash := sha256.Sum256(prefix)
	base := append([]byte("windshare/v2 object/descriptor\x00"), contextHash[:]...)
	preimage := append(bytes.Clone(base), objectHash[:]...)
	if !ed25519.Verify(fixture.sender.PublicKey(), preimage, fixture.object[prefixEnd:]) {
		t.Fatal("descriptor signature does not authenticate the bound commitment")
	}

	mutations := []struct {
		name   string
		offset int
		want   error
	}{
		{"nonce", headerBytes, ErrAuth},
		{"ciphertext", prefixBytes, ErrAuth},
		{"tag", prefixEnd - 1, ErrAuth},
		{"signature", len(fixture.object) - 1, ErrSignature},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			hostile := bytes.Clone(fixture.object)
			hostile[mutation.offset] ^= 1
			opened, err := OpenDescriptorBootstrap(fixture.pkHash, fixture.shareID, fixture.key, hostile, fixture.identity)
			if !errors.Is(err, mutation.want) || opened != nil {
				t.Fatalf("mutated descriptor = %x, %v, want %v", opened, err, mutation.want)
			}
		})
	}

	rawPreimage := append(base, prefix...)
	rawSigned := append(bytes.Clone(prefix), ed25519.Sign(fixture.privateKey, rawPreimage)...)
	opened, err := OpenDescriptorBootstrap(fixture.pkHash, fixture.shareID, fixture.key, rawSigned, fixture.identity)
	if !errors.Is(err, ErrSignature) || opened != nil {
		t.Fatalf("old raw signature = %x, %v", opened, err)
	}
}

func TestDescriptorBootstrapKeepsOneCommitmentAcrossIdentityLookup(t *testing.T) {
	fixture := newDescriptorFixture(t)
	opened, err := OpenDescriptorBootstrap(fixture.pkHash, fixture.shareID, fixture.key, fixture.object, func([]byte) (*senderauth.Verifier, error) {
		clear(fixture.object)
		clear(fixture.pkHash)
		return fixture.sender, nil
	})
	if err != nil || !bytes.Equal(opened, fixture.plaintext) {
		t.Fatalf("bootstrap snapshot = %x, %v", opened, err)
	}
}
