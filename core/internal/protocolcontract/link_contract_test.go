package protocolcontract

import (
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
