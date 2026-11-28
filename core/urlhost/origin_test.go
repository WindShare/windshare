package urlhost_test

import (
	"errors"
	"net/url"
	"testing"

	"github.com/windshare/windshare/core/urlhost"
)

func TestOriginMatchesBrowserASCIISerializationWithoutMutatingURL(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{"https://ws.example/", "https://ws.example"},
		{"HTTPS://WS.EXAMPLE:443/share", "https://ws.example"},
		{"https://ws.example:0443/share", "https://ws.example"},
		{"http://WS.Example:80/app", "http://ws.example"},
		{"http://ws.example:080/app", "http://ws.example"},
		{"https://ws.example:08443/app", "https://ws.example:8443"},
		{"https://ws.example:80/app", "https://ws.example:80"},
		{"http://ws.example:443/app", "http://ws.example:443"},
		{"wss://Relay.Example:443/v2/ws", "wss://relay.example"},
		{"ws://Relay.Example:80/v2/ws", "ws://relay.example"},
		{"wss://Relay.Example:8443/v2/ws", "wss://relay.example:8443"},
		{"http://192.0.2.1:8080/app", "http://192.0.2.1:8080"},
		{"http://192.0.2.1:80/app", "http://192.0.2.1"},
		{"https://[2001:db8::1]:8443/", "https://[2001:db8::1]:8443"},
		{"https://[2001:0db8:0000:0000::0001]:443/", "https://[2001:db8::1]"},
		{"http://[0:0:0:0:0:0:0:1]:8080/", "http://[::1]:8080"},
		{"https://[::ffff:192.0.2.1]:443/", "https://[::ffff:c000:201]"},
		{"https://[::ffff:c000:201]:443/", "https://[::ffff:c000:201]"},
		{"https://文件.example/share/", "https://xn--5nqy36c.example"},
		{"https://文件.EXAMPLE:443/share/", "https://xn--5nqy36c.example"},
		{"https://XN--5NQY36C.EXAMPLE:443/", "https://xn--5nqy36c.example"},
		{"https://bücher.example:8443/x", "https://xn--bcher-kva.example:8443"},
		{"https://ex。com/", "https://ex.com"},
		{"https://文件.example./", "https://xn--5nqy36c.example."},
		{"https://WS.EXAMPLE./", "https://ws.example."},
		{"https://user:pass@WS.Example:443/private?token=secret#frag", "https://ws.example"},
	} {
		t.Run(test.input, func(t *testing.T) {
			parsed, err := url.Parse(test.input)
			if err != nil {
				t.Fatal(err)
			}
			before := *parsed
			got, err := urlhost.Origin(parsed)
			if err != nil {
				t.Fatalf("Origin(%q) error = %v, want %q", test.input, err, test.want)
			}
			if got != test.want {
				t.Fatalf("Origin(%q) = %q, want %q", test.input, got, test.want)
			}
			if *parsed != before {
				t.Fatalf("Origin(%q) mutated input URL: got %+v, want %+v", test.input, *parsed, before)
			}
		})
	}
}

func TestOriginRejectsUnsupportedSchemesAndMalformedAuthorities(t *testing.T) {
	if _, err := urlhost.Origin(nil); !errors.Is(err, urlhost.ErrInvalidOrigin) {
		t.Fatalf("Origin(nil) err = %v, want ErrInvalidOrigin", err)
	}
	for _, raw := range []string{
		"",
		"ftp://ws.example/",
		"file:///etc/hosts",
		"mailto:user@ws.example",
		"https:///path",
		"https://ws.example:",
		"https://[::1]:",
		"https://ws.example:0",
		"https://ws.example:65536",
		"https://ws.example:bad",
		"https://xn--test.example",
		"https://\u00ad",
		"http://[192.0.2.1]:8080",
		"https://[ws.example]",
		"https://[fe80::1%25eth0]",
	} {
		t.Run(raw, func(t *testing.T) {
			parsed, err := url.Parse(raw)
			if err != nil {
				return
			}
			if got, err := urlhost.Origin(parsed); !errors.Is(err, urlhost.ErrInvalidOrigin) {
				t.Fatalf("Origin(%q) = (%q, %v), want ErrInvalidOrigin", raw, got, err)
			}
		})
	}
	for _, direct := range []*url.URL{
		{Scheme: "https", Host: "::1"},
		{Scheme: "https", Host: "a b.example"},
		{Scheme: "https", Host: "a:80:90"},
		{Scheme: "https", Host: "ws.example:bad"},
	} {
		if got, err := urlhost.Origin(direct); !errors.Is(err, urlhost.ErrInvalidOrigin) {
			t.Fatalf("Origin(%+v) = (%q, %v), want ErrInvalidOrigin", direct, got, err)
		}
	}
}
