package senderobject_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/senderobject"
)

func decoded(value string) []byte {
	result, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return result
}

func sequence(first byte, length int) []byte {
	result := make([]byte, length)
	for index := range result {
		result[index] = first + byte(index)
	}
	return result
}

func TestDescriptorMatchesFrozenSenderObjectVector(t *testing.T) {
	pkHash := decoded("JEKgoDij26RUJt97fcJPxA==")
	shareID := decoded("tVW+68OSeLBZTpU+")
	binding, err := senderobject.NewDescriptorBinding(pkHash, shareID)
	if err != nil {
		t.Fatal(err)
	}
	if got := base64.StdEncoding.EncodeToString(binding.Context()); got != "AiRCoKA4o9ukVCbfe33CT8S1Vb7rw5J4sFlOlT4=" {
		t.Fatalf("descriptor context = %s", got)
	}
	seed := sequence(0x20, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	key := decoded("QJf/rkymcTqujczmpzms5e8PRSI08a/0Fm7ica6GlQ4=")
	nonce := decoded("0NHS09TV1tfY2drb")
	plaintext := decoded("qgABAQICAgNQQEFCQ0RFRkdISUpLTE1OTwRQUFFSU1RVVldYWVpbXF1eXwUaABAAAAYHB1ggKay64UG8yvCyLhqU000LxzYeUm0L/hLIl5S8kyKWbdcIGmVT8QAJeCB3aW5kc2hhcmUvcGF0aC92MS11bmljb2RlLTE1LjAuMA==")
	want := decoded("AgAAAAAAAI/Q0dLT1NXW19jZ2ttZiFtVkBUbMjw7HI/q5B9qTw6bdNmlWAQtMgPxJGkRSmmZjl6KhYM8wRjPSNppp6y8l+oskLhTNjknPbPR41l3KLLCcl8a3QGnBZltpRP2erySHhoULzJzlDcEAkmQJyrASmgNQdVQSipSRia3l7yRnZAeFhColC/WYAO47TBO1eZro4Q2/eBvLZYFXoul3IZ12dbXljAhPMxure5bwD4bJzI+qSRmw1G6aZems+gCERr/qm4+5Vtdpve2Fmf2tbEqKTtppABJrqXNgEaKnQ4=")
	object, err := senderobject.Seal(binding, key, privateKey, nonce, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(object, want) {
		t.Fatal("descriptor bytes diverged from the frozen vector")
	}
	opened, err := senderobject.Open(binding, key, publicKey, object)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("open descriptor: %v", err)
	}
	bootstrapped, err := senderobject.OpenDescriptorBootstrap(binding, key, object, func(candidate []byte) (ed25519.PublicKey, error) {
		if !bytes.Equal(candidate, plaintext) {
			t.Fatal("bootstrap callback saw wrong plaintext")
		}
		return publicKey, nil
	})
	if err != nil || !bytes.Equal(bootstrapped, plaintext) {
		t.Fatalf("bootstrap descriptor: %v", err)
	}
}

func TestSenderObjectRejectsIdentityCiphertextAndSignatureSubstitution(t *testing.T) {
	share := sequence(1, 16)
	file := sequence(21, 16)
	binding, _ := senderobject.NewRevisionBinding(share, file)
	key := bytes.Repeat([]byte{0x41}, 32)
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x51}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	object, err := senderobject.Seal(binding, key, privateKey, bytes.Repeat([]byte{0x61}, senderobject.NonceBytes), []byte{0xa1, 0x00, 0x01})
	if err != nil {
		t.Fatal(err)
	}

	otherFile := bytes.Clone(file)
	otherFile[0] ^= 1
	otherBinding, _ := senderobject.NewRevisionBinding(share, otherFile)
	if _, err := senderobject.Open(otherBinding, key, publicKey, object); !errors.Is(err, senderobject.ErrSignature) {
		t.Fatalf("context substitution error = %v", err)
	}
	ciphertext := bytes.Clone(object)
	ciphertext[senderobject.HeaderBytes+senderobject.NonceBytes] ^= 1
	if _, err := senderobject.Open(binding, key, publicKey, ciphertext); !errors.Is(err, senderobject.ErrSignature) {
		t.Fatalf("ciphertext substitution error = %v", err)
	}
	signature := bytes.Clone(object)
	signature[len(signature)-1] ^= 1
	if _, err := senderobject.Open(binding, key, publicKey, signature); !errors.Is(err, senderobject.ErrSignature) {
		t.Fatalf("signature substitution error = %v", err)
	}
	wrongKey := bytes.Clone(key)
	wrongKey[0] ^= 1
	if _, err := senderobject.Open(binding, wrongKey, publicKey, object); !errors.Is(err, senderobject.ErrAuth) {
		t.Fatalf("wrong AEAD key error = %v", err)
	}
}

func TestBindingsFreezeEveryContextAxisAndLimit(t *testing.T) {
	share := sequence(1, 16)
	directory := sequence(21, 16)
	file := sequence(41, 16)
	revision := sequence(61, 16)
	cases := []struct {
		binding      senderobject.Binding
		domain       senderobject.Domain
		contextBytes int
	}{
		func() struct {
			binding      senderobject.Binding
			domain       senderobject.Domain
			contextBytes int
		} {
			b, _ := senderobject.NewCatalogPageBinding(share, directory, 7)
			return struct {
				binding      senderobject.Binding
				domain       senderobject.Domain
				contextBytes int
			}{b, senderobject.DomainCatalogPage, 36}
		}(),
		func() struct {
			binding      senderobject.Binding
			domain       senderobject.Domain
			contextBytes int
		} {
			b, _ := senderobject.NewDirectoryErrorBinding(share, directory)
			return struct {
				binding      senderobject.Binding
				domain       senderobject.Domain
				contextBytes int
			}{b, senderobject.DomainDirectoryError, 32}
		}(),
		func() struct {
			binding      senderobject.Binding
			domain       senderobject.Domain
			contextBytes int
		} {
			b, _ := senderobject.NewRevisionBinding(share, file)
			return struct {
				binding      senderobject.Binding
				domain       senderobject.Domain
				contextBytes int
			}{b, senderobject.DomainRevision, 32}
		}(),
		func() struct {
			binding      senderobject.Binding
			domain       senderobject.Domain
			contextBytes int
		} {
			b, _ := senderobject.NewBlockRecordBinding(share, file, revision, 9, 10)
			return struct {
				binding      senderobject.Binding
				domain       senderobject.Domain
				contextBytes int
			}{b, senderobject.DomainBlockRecord, 60}
		}(),
		func() struct {
			binding      senderobject.Binding
			domain       senderobject.Domain
			contextBytes int
		} {
			b, _ := senderobject.NewOfflineCommitBinding(share)
			return struct {
				binding      senderobject.Binding
				domain       senderobject.Domain
				contextBytes int
			}{b, senderobject.DomainOfflineCommit, 16}
		}(),
	}
	for _, test := range cases {
		if test.binding.Domain() != test.domain || len(test.binding.Context()) != test.contextBytes || test.binding.MaxBytes() == 0 {
			t.Fatalf("binding %q = context %d, limit %d", test.domain, len(test.binding.Context()), test.binding.MaxBytes())
		}
	}
	if _, err := senderobject.NewRevisionBinding(make([]byte, 16), file); !errors.Is(err, senderobject.ErrBinding) {
		t.Fatalf("zero identity error = %v", err)
	}
	if _, err := senderobject.Seal(cases[2].binding, make([]byte, 32), privateKeyForLimit(), make([]byte, 12), bytes.Repeat([]byte{1}, senderobject.MaxRevisionBytes)); !errors.Is(err, senderobject.ErrTooLarge) {
		t.Fatalf("oversized object error = %v", err)
	}
}

func privateKeyForLimit() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
}
