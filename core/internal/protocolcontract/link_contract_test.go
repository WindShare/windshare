package protocolcontract

import (
	"strconv"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func capabilityLinkCases(t *testing.T, f *fixture) []any {
	t.Helper()
	var cases []any
	for _, test := range []struct {
		name   string
		front  string
		relays []string
	}{
		{"implicit-origin", "https://share.example/app", nil},
		{"same-origin-websocket", "https://share.example", []string{"wss://share.example"}},
		{"default-port", "https://SHARE.example:443/app", []string{"wss://share.example:0443/"}},
		{"local-port", "http://localhost:8484/app", []string{"ws://localhost:8484"}},
		{"ipv6", "http://[::1]:8484", []string{"ws://[::1]:8484"}},
		{"other-origin", "https://share.example", []string{"wss://relay.example"}},
		{"relay-subpath", "https://share.example/app", []string{"wss://share.example/app"}},
		{"relay-query", "https://share.example", []string{"wss://share.example?token=abc"}},
		{"multiple-relays", "https://share.example", []string{"wss://share.example", "wss://relay.example"}},
	} {
		capability, err := link.NewSenderAuthenticated(f.readSecret, f.edPublic, test.relays)
		if err != nil {
			t.Fatal(err)
		}
		full, err := capability.URL(test.front)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := link.Parse(full)
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, map[string]any{
			"name": test.name, "url": full, "relays": parsed.Relays,
		})
	}
	return cases
}

// Copied capabilities reach the CLI and the browser through the same clipboard.
// Both runtimes must accept and reject each spelling identically.
func copiedCapabilityLinkCases(t *testing.T, f *fixture) []any {
	t.Helper()
	capability, err := link.NewSenderAuthenticated(f.readSecret, f.edPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	bare, key, err := capability.SplitURL("https://share.example/app")
	if err != nil {
		t.Fatal(err)
	}
	full := bare + "#" + key
	encodedKey := "%" + strconv.FormatInt(int64(key[0]), 16) + key[1:]
	var cases []any
	for _, test := range []struct{ name, url string }{
		{"c0-control-boundaries", "\x00" + full + "\x1f"},
		{"wrapped-route", bare[:len(bare)-4] + "\r\n" + bare[len(bare)-4:] + "#" + key},
		{"wrapped-key", bare + "#" + key[:22] + "\n" + key[22:]},
		{"single-dot-segment", bare + "/.#" + key},
		{"encoded-single-dot-segment", bare + "/%2E/#" + key},
		{"double-dot-segment", bare + "/x/.%2e#" + key},
		{"double-dot-removes-route", bare + "/..#" + key},
		{"encoded-slash-after-route", bare + "%2F#" + key},
		{"repeated-fragment-delimiter", bare + "##" + key},
		{"url-inside-fragment", bare + "#https://other.example/#" + key},
		{"percent-encoded-key", bare + "#" + encodedKey},
		{"space-prefixed-key", bare + "# " + key},
		{"two-links-joined-by-newline", full + "\nhttps://other.example/" + bare[len(bare)-16:] + "#" + key},
	} {
		parsed, err := link.Parse(test.url)
		result := map[string]any{"name": test.name, "url": test.url, "accepted": err == nil}
		if err == nil {
			result["relays"] = parsed.Relays
		}
		cases = append(cases, result)
	}
	return cases
}
