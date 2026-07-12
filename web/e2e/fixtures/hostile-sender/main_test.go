package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestSealHostileManifestAuthenticatesInvalidPathWithFreshNonce(t *testing.T) {
	t.Parallel()

	secret := bytes.Repeat([]byte{0x5a}, link.ReadSecretBytes)
	key, err := hkdf.Key(sha256.New, secret, nil, manifestKeyLabel, manifestKeyBytes)
	if err != nil {
		t.Fatalf("derive manifest key: %v", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("create manifest cipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("create manifest AEAD: %v", err)
	}
	firstNonce := bytes.Repeat([]byte{0x11}, aead.NonceSize())
	hostileNonce := bytes.Repeat([]byte{0x22}, aead.NonceSize())
	sealed, err := sealHostileManifest(
		secret,
		bytes.NewReader(append(firstNonce, hostileNonce...)),
	)
	if err != nil {
		t.Fatalf("seal hostile manifest: %v", err)
	}
	if len(sealed) < len(hostileNonce) {
		t.Fatalf("sealed manifest is too short: got %d bytes", len(sealed))
	}
	if !bytes.Equal(sealed[:len(hostileNonce)], hostileNonce) {
		t.Fatalf("final manifest reused the placeholder nonce: got %x", sealed[:len(hostileNonce)])
	}

	plain, err := aead.Open(
		nil,
		sealed[:aead.NonceSize()],
		sealed[aead.NonceSize():],
		[]byte{link.SuiteAESGCM},
	)
	if err != nil {
		t.Fatalf("authenticate hostile manifest: %v", err)
	}
	if bytes.Count(plain, []byte(hostilePath)) != 1 {
		t.Fatalf("authenticated plaintext does not contain exactly one hostile path")
	}
	if bytes.Contains(plain, []byte(placeholderPath)) {
		t.Fatalf("authenticated plaintext retained the placeholder path")
	}
}
