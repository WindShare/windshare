package keyderiv_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/internal/keyderiv"
)

var secretA = []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestSuite02FrozenKeys(t *testing.T) {
	share := mustB64("QEFCQ0RFRkdISUpLTE1OTw==")
	pkHash := mustB64("JEKgoDij26RUJt97fcJPxA==")
	descriptor, err := keyderiv.V2DescriptorKey(secretA, pkHash)
	if err != nil || !bytes.Equal(descriptor, mustB64("QJf/rkymcTqujczmpzms5e8PRSI08a/0Fm7ica6GlQ4=")) {
		t.Fatalf("descriptor key = %x, %v", descriptor, err)
	}
	sessionAuth, err := keyderiv.V2SessionAuthKey(secretA, share)
	if err != nil || !bytes.Equal(sessionAuth, mustB64("fcXpUVk4IKHiN4FgGwKacIO4MsROiFOwlELBMu4c+X0=")) {
		t.Fatalf("session-auth key = %x, %v", sessionAuth, err)
	}
}

func TestSuite02RejectsEveryWrongWidth(t *testing.T) {
	secret := make([]byte, keyderiv.V2ReadSecretBytes)
	pkHash := make([]byte, keyderiv.V2PKHashBytes)
	share := make([]byte, keyderiv.V2IdentityBytes)
	tests := []func() error{
		func() error { _, err := keyderiv.V2DescriptorKey(secret[:15], pkHash); return err },
		func() error { _, err := keyderiv.V2DescriptorKey(secret, pkHash[:15]); return err },
		func() error { _, err := keyderiv.V2SessionAuthKey(secret[:15], share); return err },
		func() error { _, err := keyderiv.V2SessionAuthKey(secret, share[:15]); return err },
	}
	for index, test := range tests {
		if err := test(); !errors.Is(err, keyderiv.ErrV2KeyMaterial) {
			t.Fatalf("wrong-width case %d error = %v", index, err)
		}
	}
}

func TestSuite02DomainSeparationAndDeterminism(t *testing.T) {
	sharedContext := mustB64("QEFCQ0RFRkdISUpLTE1OTw==")
	descriptor, err := keyderiv.V2DescriptorKey(secretA, sharedContext)
	if err != nil {
		t.Fatalf("V2DescriptorKey: %v", err)
	}
	sessionAuth, err := keyderiv.V2SessionAuthKey(secretA, sharedContext)
	if err != nil {
		t.Fatalf("V2SessionAuthKey: %v", err)
	}
	if len(descriptor) != keyderiv.KeyBytes || len(sessionAuth) != keyderiv.KeyBytes {
		t.Fatalf("unexpected derived key lengths: descriptor=%d sessionAuth=%d want=%d", len(descriptor), len(sessionAuth), keyderiv.KeyBytes)
	}
	if bytes.Equal(descriptor, sessionAuth) {
		t.Fatal("distinct suite-0x02 labels derived identical keys for the same context")
	}
	repeated, err := keyderiv.V2DescriptorKey(secretA, sharedContext)
	if err != nil || !bytes.Equal(descriptor, repeated) {
		t.Fatalf("repeated derivation differed: %x vs %x (%v)", descriptor, repeated, err)
	}
}
