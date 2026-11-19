package senderobject_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/windshare/windshare/core/senderauth"
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
	seed := sequence(0x20, ed25519.SeedSize)
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	key := decoded("QJf/rkymcTqujczmpzms5e8PRSI08a/0Fm7ica6GlQ4=")
	nonce := decoded("0NHS09TV1tfY2drb")
	plaintext := decoded("qgABAQICAgNQQEFCQ0RFRkdISUpLTE1OTwRQUFFSU1RVVldYWVpbXF1eXwUaABAAAAYHB1ggKay64UG8yvCyLhqU000LxzYeUm0L/hLIl5S8kyKWbdcIGmVT8QAJeCB3aW5kc2hhcmUvcGF0aC92MS11bmljb2RlLTE1LjAuMA==")
	want := decoded("AwAAAAAAAI/Q0dLT1NXW19jZ2ttZiFtVkBUbMjw7HI/q5B9qTw6bdNmlWAQtMgPxJGkRSmmZjl6KhYM8wRjPSNppp6y8l+oskLhTNjknPbPR41l3KLLCcl8a3QGnBZltpRP2erySHhoULzJzlDcEAkmQJyrASmgNQdVQSipSRia3l7yRnZAeFhColC/WYAO47TBOn5u8MGIKWZC/L/pzet2u2qONbgraaKsFf/7sOCS0maBxagI8gLbTq0LV4gEWiLKOZaaFp5fLFOcFOtJApli8+KMutAhBTnazobRt+9RotAI=")
	object, err := senderobject.SealDescriptor(pkHash, shareID, key, privateKey, nonce, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(object, want) {
		t.Fatal("descriptor bytes diverged from the frozen vector")
	}
	bootstrapped, err := senderobject.OpenDescriptorBootstrap(pkHash, shareID, key, object, func(candidate []byte) (*senderauth.Verifier, error) {
		if !bytes.Equal(candidate, plaintext) {
			t.Fatal("bootstrap callback saw wrong plaintext")
		}
		return checkedSender(t, publicKey), nil
	})
	if err != nil || !bytes.Equal(bootstrapped, plaintext) {
		t.Fatalf("bootstrap descriptor: %v", err)
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
