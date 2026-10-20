package relayconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFrontendValidationPreservesWebsiteAddress(t *testing.T) {
	for _, raw := range []string{"https://front.example", "https://front.example/app/", "http://localhost:38384", "http://[::1]:38384"} {
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
	} {
		if !errors.Is(ValidateFrontURL(raw), ErrFrontURL) {
			t.Fatalf("invalid frontend accepted: %q", raw)
		}
		// Omission is useful for relay-only exports; a nonempty malformed address
		// must be rejected at import, before it can replace usable defaults.
		if raw != "" {
			encoded, _ := json.Marshal(raw)
			input := `{"front_url":` + string(encoded) + `,"profiles":[{"url":"https://relay.example"}]}`
			if _, err := Decode(strings.NewReader(input)); !errors.Is(err, ErrConfig) {
				t.Fatalf("import accepted invalid frontend: %q", raw)
			}
		}
	}
}
