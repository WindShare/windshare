package relayconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestFrontendValidationPreservesWebsiteAddress(t *testing.T) {
	for _, raw := range []string{
		"https://front.example", "https://front.example/app/", "http://localhost:38384", "http://[::1]:38384",
		"https://front.example/\u5206\u4eab", "https://front.example/my%C2%A0app", "https://front.example/app%C2%85",
	} {
		if err := ValidateFrontURL(raw); err != nil {
			t.Fatalf("valid frontend %q: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"", "front.example", "/app", "//front.example", "wss://front.example", "https:///app",
		"https://user:password@front.example", "https://front.example?route=other", "https://front.example?",
		"https://front.example#key", "https://front.example#", " https://front.example", "https://front.example/has space",
		"https://front.example:0", "https://front.example:65536", "https://front.example:", "https://front.example:nope",
		"https://front.example\\other", "https://front.example/%invalid",
		// Copied-text whitespace and controls that capability construction trims
		// or percent-encodes into the path (see link.TrimCopiedText).
		"https://front.example/app\u0085",
		"https://front.example/app/\u0085",
		"https://front.example/app\ufeff",
		"\ufeffhttps://front.example/app",
		"https://front.example/app\u00a0",
		"https://front.example/app\u3000",
		"https://front.example/app\u2028",
		"https://front.example/app\u2029",
		"https://front.example/my\u00a0app",
	} {
		if !errors.Is(ValidateFrontURL(raw), ErrFrontURL) {
			t.Fatalf("invalid frontend accepted: %q", raw)
		}
		// Omission is useful for relay-only exports; a nonempty malformed address
		// must be rejected at import, before it can replace usable defaults.
		if raw != "" {
			encoded, _ := json.Marshal(raw)
			input := `{"front_url":` + string(encoded) + `,"profiles":[{"url":"https://relay.example"}]}`
			if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) || !errors.Is(err, ErrFrontURL) {
				t.Fatalf("import lost frontend validation cause for %q: %v", raw, err)
			}
		}
	}
}

// A saved FrontURL that still carries U+0085 publishes /app%C2%85/<share-id>
// instead of /app/<share-id>. ValidateFrontURL must reject it before Save.
func TestFrontendValidationRejectsNELBeforeShareLinksEncodeIt(t *testing.T) {
	pkHash := make([]byte, link.PKHashBytes)
	readSecret := make([]byte, link.ReadSecretBytes)
	for i := range pkHash {
		pkHash[i] = byte(i + 1)
	}
	for i := range readSecret {
		readSecret[i] = byte(i + 50)
	}
	shareID, err := link.ShareIDForSenderKeyHash(pkHash)
	if err != nil {
		t.Fatal(err)
	}
	capability := link.Link{
		Suite:      link.SuiteSenderAuthenticated,
		ShareID:    shareID,
		PKHash:     pkHash,
		ReadSecret: readSecret,
		Relays:     []string{"https://relay.example"},
	}

	const dirty = "https://front.example/app\u0085"
	if ValidateFrontURL(dirty) == nil {
		t.Fatal("ValidateFrontURL accepted a FrontURL ending in U+0085")
	}
	published, err := capability.URL(dirty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(published, "/app%C2%85/") {
		t.Fatalf("U+0085 FrontURL did not encode into the share path: %q", published)
	}
	clean, err := capability.URL("https://front.example/app")
	if err != nil {
		t.Fatal(err)
	}
	if published == clean {
		t.Fatal("U+0085 FrontURL unexpectedly matched the clean share URL")
	}
}
