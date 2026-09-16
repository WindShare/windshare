package senderobject_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/senderobject"
)

func TestBlockRecordSignatureCommitsToEverySealedByte(t *testing.T) {
	const (
		blockBytes          = 1 << 20
		signatureInputBytes = 97
	)
	binding, err := senderobject.NewBlockRecordBinding(sequence(1, 16), sequence(21, 16), sequence(41, 16), 9, blockBytes)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := privateKeyForLimit()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	plaintext := bytes.Repeat([]byte{0x5a}, blockBytes)
	key := bytes.Repeat([]byte{0x41}, 32)
	object, err := senderobject.Seal(binding, key, privateKey, sequence(0x61, senderobject.NonceBytes), plaintext)
	if err != nil {
		t.Fatal(err)
	}
	prefixEnd := len(object) - senderobject.SignatureBytes
	contextHash := sha256.Sum256(binding.Context())
	objectHash := sha256.Sum256(object[:prefixEnd])
	preimage := append([]byte(string(binding.Domain())+"\x00"), contextHash[:]...)
	preimage = append(preimage, objectHash[:]...)
	if len(preimage) != signatureInputBytes || !ed25519.Verify(publicKey, preimage, object[prefixEnd:]) {
		t.Fatal("block signature does not authenticate the fixed-size commitment")
	}
	opened, err := senderobject.Open(binding, key, checkedSender(t, publicKey), object)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("open committed block: %v", err)
	}

	for name, offset := range map[string]int{
		"nonce":              senderobject.HeaderBytes,
		"ciphertext-start":   senderobject.HeaderBytes + senderobject.NonceBytes,
		"ciphertext-middle":  prefixEnd / 2,
		"ciphertext-end":     prefixEnd - senderobject.TagBytes - 1,
		"authentication-tag": prefixEnd - 1,
		"signature":          len(object) - 1,
	} {
		t.Run(name, func(t *testing.T) {
			hostile := bytes.Clone(object)
			hostile[offset] ^= 1
			if err := senderobject.Verify(binding, checkedSender(t, publicKey), hostile); !errors.Is(err, senderobject.ErrSignature) {
				t.Fatalf("Verify error = %v, want signature rejection", err)
			}
		})
	}

	// A correctly signed old-style preimage must not enable a second verifier path.
	rawPreimage := append([]byte(string(binding.Domain())+"\x00"), contextHash[:]...)
	rawPreimage = append(rawPreimage, object[:prefixEnd]...)
	rawSigned := append(bytes.Clone(object[:prefixEnd]), ed25519.Sign(privateKey, rawPreimage)...)
	if err := senderobject.Verify(binding, checkedSender(t, publicKey), rawSigned); !errors.Is(err, senderobject.ErrSignature) {
		t.Fatalf("raw-object signature error = %v", err)
	}
}

func TestBlockRecordCommitmentRejectsEveryIdentityAxis(t *testing.T) {
	share, file, revision := sequence(1, 16), sequence(21, 16), sequence(41, 16)
	const blockIndex, dataLength = 9, 10
	bind := func(share, file, revision []byte, index uint64, length uint32) senderobject.Binding {
		t.Helper()
		binding, err := senderobject.NewBlockRecordBinding(share, file, revision, index, length)
		if err != nil {
			t.Fatal(err)
		}
		return binding
	}
	binding := bind(share, file, revision, blockIndex, dataLength)
	privateKey := privateKeyForLimit()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	object, err := senderobject.Seal(binding, bytes.Repeat([]byte{0x41}, 32), privateKey,
		sequence(0x61, senderobject.NonceBytes), bytes.Repeat([]byte{0x5a}, dataLength))
	if err != nil {
		t.Fatal(err)
	}
	for name, hostile := range map[string]senderobject.Binding{
		"share":    bind(sequence(2, 16), file, revision, blockIndex, dataLength),
		"file":     bind(share, sequence(22, 16), revision, blockIndex, dataLength),
		"revision": bind(share, file, sequence(42, 16), blockIndex, dataLength),
		"index":    bind(share, file, revision, blockIndex+1, dataLength),
		"length":   bind(share, file, revision, blockIndex, dataLength+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := senderobject.Verify(hostile, checkedSender(t, publicKey), object); !errors.Is(err, senderobject.ErrSignature) {
				t.Fatalf("Verify error = %v, want identity rejection", err)
			}
		})
	}
}
