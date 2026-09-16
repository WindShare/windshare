package link_test

import (
	"net/url"
	"reflect"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/link"
)

func TestRelayHintOmissionPreservesEndpointSelection(t *testing.T) {
	for _, test := range []struct {
		name  string
		front string
		relay string
		omit  bool
	}{
		{"secure websocket", "https://relay.example", "wss://relay.example", true},
		{"secure HTTP", "https://relay.example", "https://relay.example/", true},
		{"local websocket", "http://localhost:5173", "ws://localhost:5173", true},
		{"default secure port", "https://RELAY.example:443", "wss://relay.example:0443/", true},
		{"default plain port", "http://relay.example:80", "ws://RELAY.example", true},
		{"frontend subpath", "https://relay.example/app", "wss://relay.example", true},
		{"IPv6", "http://[::1]:8484/app", "ws://[::1]:8484/", true},
		{"other host", "https://app.example", "wss://relay.example", false},
		{"other port", "https://relay.example", "wss://relay.example:8443", false},
		{"other security", "https://relay.example", "ws://relay.example", false},
		{"relay subpath", "https://relay.example/app", "wss://relay.example/app", false},
		{"relay query credentials", "https://relay.example", "wss://relay.example?token=abc", false},
		{"explicit empty query", "https://relay.example", "wss://relay.example?", false},
		{"encoded root path", "https://relay.example", "wss://relay.example/%2F", false},
		{"double slash", "https://relay.example", "wss://relay.example//", false},
		{"userinfo", "https://relay.example", "wss://user@relay.example", false},
		{"fragment", "https://relay.example", "wss://relay.example#", false},
		{"unsupported scheme", "https://relay.example", "ftp://relay.example", false},
		{"malformed URL", "https://relay.example", "wss://relay.example:bad", false},
		{"relative hint", "https://relay.example", "relay.example", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			capability := newLink(test.relay)
			full, err := capability.URL(test.front)
			if err != nil {
				t.Fatal(err)
			}
			page, err := url.Parse(full)
			if err != nil {
				t.Fatal(err)
			}
			if page.Query().Has("r") == test.omit {
				t.Fatalf("relay omission=%v, URL=%s", test.omit, full)
			}
			bare, key, err := capability.SplitURL(test.front)
			if err != nil {
				t.Fatal(err)
			}
			splitBare, splitKey, err := link.Split(full)
			if err != nil || bare != splitBare || key != splitKey {
				t.Fatalf("split representations disagree: %v", err)
			}
			parsed, err := link.Parse(full)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := link.Merge(bare, key)
			if err != nil || !reflect.DeepEqual(merged, parsed) {
				t.Fatalf("split-key relay resolution differs: %v", err)
			}
			if !slices.Equal(capability.Relays, []string{test.relay}) {
				t.Fatal("URL construction mutated the sender's relay configuration")
			}
			if !test.omit && !slices.Equal(parsed.Relays, capability.Relays) {
				t.Fatalf("explicit endpoint changed: %v", parsed.Relays)
			}
		})
	}
}

func TestImplicitRelayUsesOnlyTheCapabilityOrigin(t *testing.T) {
	for _, test := range []struct {
		base string
		want string
	}{
		{"https://Relay.example:443/app", "https://relay.example"},
		{"http://localhost:080/app", "http://localhost"},
		{"http://localhost:5173/app", "http://localhost:5173"},
		{"https://[::1]:8443/app", "https://[::1]:8443"},
	} {
		t.Run(test.base, func(t *testing.T) {
			raw := test.base + "/" + testShareID + "?trace=1#" + testKey
			got, err := link.Parse(raw)
			if err != nil || !slices.Equal(got.Relays, []string{test.want}) {
				t.Fatalf("resolved relays=%v error=%v", got.Relays, err)
			}
		})
	}
}

func TestExplicitRelayHintsAreNeverPartiallyElided(t *testing.T) {
	for _, relays := range [][]string{
		{"wss://relay.example", "wss://other.example"},
		{"wss://other.example", "wss://relay.example"},
		{""},
	} {
		capability := newLink(relays...)
		full, err := capability.URL("https://relay.example")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := link.Parse(full)
		if err != nil || !slices.Equal(parsed.Relays, relays) {
			t.Fatalf("explicit relay list changed: got=%v want=%v error=%v", parsed.Relays, relays, err)
		}
	}
}
