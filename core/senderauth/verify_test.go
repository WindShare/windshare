package senderauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
	"testing"
)

type acceptanceCase struct {
	Name         string
	PublicKeyHex string
	MessageHex   string
	SignatureHex string
	Accepted     bool
	KeyAccepted  bool
}

func TestSharedAcceptance(t *testing.T) {
	encoded, err := os.ReadFile("../testvectors/ed25519-acceptance.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct{ Cases []acceptanceCase }
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors.Cases {
		t.Run(vector.Name, func(t *testing.T) {
			publicKey := decodeHex(t, vector.PublicKeyHex)
			message := decodeHex(t, vector.MessageHex)
			signature := decodeHex(t, vector.SignatureHex)
			verifier, err := NewVerifier(publicKey)
			if (err == nil) != vector.KeyAccepted {
				t.Fatalf("key accepted = %v, want %v", err == nil, vector.KeyAccepted)
			}
			if got := Verify(publicKey, message, signature); got != vector.Accepted {
				t.Fatalf("stateless verification = %v, want %v", got, vector.Accepted)
			}
			if got := verifier.Verify(message, signature); got != vector.Accepted {
				t.Fatalf("bound verification = %v, want %v", got, vector.Accepted)
			}
		})
	}
}

func TestVerifierOwnsKeyAndSupportsConcurrentUse(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	message := []byte("sender authentication")
	signature := ed25519.Sign(privateKey, message)
	verifier, err := NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	clear(publicKey)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if !verifier.Verify(message, signature) {
				t.Error("key snapshot changed")
			}
		})
	}
	workers.Wait()
	if new(Verifier).Verify(message, signature) {
		t.Fatal("zero-value verifier accepted")
	}
}

func decodeHex(t *testing.T, value string) []byte {
	t.Helper()
	result, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
