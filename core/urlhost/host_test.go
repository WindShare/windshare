package urlhost_test

import (
	"net/url"
	"testing"

	"github.com/windshare/windshare/core/urlhost"
)

func TestNormalizePreservesURLComponentsAndIsIdempotent(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"https://relay.example:8443/a%2Fb?token=a%2fb+~&flag", "https://relay.example:8443/a%2Fb?token=a%2fb+~&flag"},
		{"https://münchen.de:8443/a%2Fb?token=a%2fb+~&flag", "https://xn--mnchen-3ya.de:8443/a%2Fb?token=a%2fb+~&flag"},
		{"https://xn--mnchen-3ya.de/path#key", "https://xn--mnchen-3ya.de/path#key"},
		{"https://XN--MNCHEN-3YA.de/path", "https://xn--mnchen-3ya.de/path"},
		{"http://[::1]:8080/a", "http://[::1]:8080/a"},
		{"https://ＥＸＡＭＰＬＥ.com", "https://example.com"},
	} {
		t.Run(test.input, func(t *testing.T) {
			parsed, err := url.Parse(test.input)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := urlhost.Normalize(parsed); err != nil {
					t.Fatal(err)
				}
				if got := parsed.String(); got != test.want {
					t.Fatalf("normalized = %q, want %q", got, test.want)
				}
			}
		})
	}
}

func TestNormalizeRejectsErasedDomainsAndInvalidALabels(t *testing.T) {
	for _, host := range []string{"", "\u00ad", "\u200b:8443", "xn--.com", "b\u00fccher.xn--.example", "b\u00fccher.xn--abc-.example", "xn--test.example", "a.xn--a.com", "ü.com：8080", "a\u200db.com"} {
		t.Run(host, func(t *testing.T) {
			parsed := &url.URL{Scheme: "https", Host: host, RawQuery: "token=a%2fb"}
			before := *parsed
			if err := urlhost.Normalize(parsed); err == nil {
				t.Fatal("invalid host accepted")
			}
			if *parsed != before {
				t.Fatal("failed normalization mutated URL")
			}
		})
	}
}
