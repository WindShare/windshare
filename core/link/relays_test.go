package link_test

import (
	"errors"
	"net/url"
	"reflect"
	"slices"
	"strings"
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

// Browsers convert a Unicode link host to its ASCII form before using the link
// origin as the implicit relay. The CLI must derive the same relay, or a link
// pasted from a chat that shows the Unicode spelling names an unreachable one.
func TestImplicitRelayResolvesUnicodeHostsLikeBrowser(t *testing.T) {
	for _, test := range []struct {
		host string
		want string
	}{
		{"münchen.de", "https://xn--mnchen-3ya.de"},
		{"MÜNCHEN.DE", "https://xn--mnchen-3ya.de"},
		{"文件.example:8443", "https://xn--5nqy36c.example:8443"},
		{"ex😀.com", "https://xn--ex-oo82a.com"},
		{"ＥＸＡＭＰＬＥ.com", "https://example.com"},
		{"ex。com", "https://ex.com"},
		{"xn--mnchen-3ya.de", "https://xn--mnchen-3ya.de"},
		{"straße.de", "https://xn--strae-oqa.de"},
		{"ü.com.", "https://xn--tda.com."},
		{"ü-.com", "https://xn----dha.com"},
		{"ü.com:8080", "https://xn--tda.com:8080"},
	} {
		t.Run(test.host, func(t *testing.T) {
			full := "https://" + test.host + "/" + testShareID + "#" + testKey
			got, err := link.Parse(full)
			if err != nil || !slices.Equal(got.Relays, []string{test.want}) {
				t.Fatalf("Parse relays=%v error=%v, want [%s]", got.Relays, err, test.want)
			}
			bare, key, err := link.Split(full)
			if err != nil {
				t.Fatal(err)
			}
			merged, err := link.Merge(bare, key)
			if err != nil || !reflect.DeepEqual(merged, got) {
				t.Fatalf("Merge(Split) = %+v, %v; want %+v", merged, err, got)
			}
		})
	}
}

func TestParseRejectsUnicodeHostsBrowsersRefuse(t *testing.T) {
	for _, host := range []string{
		"a\u200db.com", "\u2488.com", "\u00ad", "\u200b:8443", "xn--test.example",
		// Fullwidth look-alikes normalize to URL syntax characters, which a
		// browser refuses instead of reinterpreting as port/path/userinfo.
		"ü.com\uff1a8080", "ü.com\uff0fevil", "ü.com\uff20evil", "ü.com\uff1fx",
		"ü.com\uff03x", "ü.com\uff05x", "ü.com\uff3c", "ü\u3000.com",
	} {
		full := "https://" + host + "/" + testShareID + "#" + testKey
		if _, err := link.Parse(full); !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("Parse host %q error=%v, want ErrMalformedLink", host, err)
		}
		if _, _, err := link.Split(full); !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("Split host %q error=%v, want ErrMalformedLink", host, err)
		}
		if _, err := link.Merge(full, testKey); !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("Merge host %q error=%v, want ErrMalformedLink", host, err)
		}
		if _, err := newLink().URL("https://" + host); !errors.Is(err, link.ErrMalformedLink) {
			t.Fatalf("URL host %q error=%v, want ErrMalformedLink", host, err)
		}
	}
}

func TestUnicodeFrontendKeepsTheSameImplicitRelayAsItsASCIIForm(t *testing.T) {
	capability := newLink("https://xn--mnchen-3ya.de")
	for _, base := range []string{"https://münchen.de", "https://xn--mnchen-3ya.de"} {
		full, err := capability.URL(base)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(full, "?r=") {
			t.Fatalf("%s: relay hint was not elided for the frontend's own origin: %s", base, full)
		}
		parsed, err := link.Parse(full)
		if err != nil || !slices.Equal(parsed.Relays, capability.Relays) {
			t.Fatalf("%s: relays=%v error=%v", base, parsed.Relays, err)
		}
	}
}
