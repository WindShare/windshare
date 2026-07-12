package protocol_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type relayEndpointVectorFile struct {
	Version     int                      `json:"version"`
	Kind        string                   `json:"kind"`
	Description string                   `json:"description"`
	Cases       []relayEndpointCase      `json:"cases"`
	ASCIIMatrix relayEndpointASCIIMatrix `json:"asciiMatrix"`
}

type relayEndpointASCIIMatrix struct {
	First    byte                        `json:"first"`
	Last     byte                        `json:"last"`
	Path     relayEndpointASCIIComponent `json:"path"`
	Query    relayEndpointASCIIComponent `json:"query"`
	Userinfo relayEndpointASCIIComponent `json:"userinfo"`
}

type relayEndpointASCIIComponent struct {
	Skip         string            `json:"skip"`
	Alphanumeric bool              `json:"alphanumeric"`
	Literal      string            `json:"literal"`
	Escaped      map[string]string `json:"escaped,omitempty"`
}

type relayEndpointCase struct {
	Name         string `json:"name"`
	RelayURL     string `json:"relayUrl"`
	Accepted     bool   `json:"accepted"`
	WebSocketURL string `json:"webSocketUrl,omitempty"`
}

func buildRelayEndpointVectorFile() relayEndpointVectorFile {
	accepted := func(name, relayURL, webSocketURL string) relayEndpointCase {
		return relayEndpointCase{
			Name: name, RelayURL: relayURL, Accepted: true, WebSocketURL: webSocketURL,
		}
	}
	rejected := func(name, relayURL string) relayEndpointCase {
		return relayEndpointCase{Name: name, RelayURL: relayURL}
	}
	labelAtLimit := strings.Repeat("a", 63)
	nameAtLimit := strings.Join([]string{
		labelAtLimit, labelAtLimit, labelAtLimit, strings.Repeat("b", 61),
	}, ".")
	nameAboveLimit := nameAtLimit + "b"

	return relayEndpointVectorFile{
		Version:     vectorEnvelopeVersion,
		Kind:        "relay-endpoint",
		Description: "Shared Go/browser relay endpoint normalization for shareId AAAAAAAAAAAA. Both runtimes must either produce the exact webSocketUrl or reject before dialing. The contract rejects parser auto-repairs or lossy canonicalization whose authority, path, or credential spelling differs between net/url and WHATWG URL. Relay hosts use an explicit ASCII DNS/IP repertoire and reserve xn-- because independently versioned IDNA tables do not select a stable authority. URL-boundary whitespace also uses an explicit shared set. asciiMatrix exhaustively pins every printable ASCII byte that can occur as path, query, or userinfo data; grammar delimiters listed in skip are covered by the fixed cases instead.",
		ASCIIMatrix: relayEndpointASCIIMatrix{
			First: ' ',
			Last:  '~',
			Path: relayEndpointASCIIComponent{
				Skip: "?#", Alphanumeric: true, Literal: "-._~$&+,/:;=@",
			},
			Query: relayEndpointASCIIComponent{
				Skip: "#", Alphanumeric: true, Literal: "-._~!$&()*+,;=:@/?",
			},
			Userinfo: relayEndpointASCIIComponent{
				Skip: "/?#", Alphanumeric: true, Literal: "-._~",
				Escaped: map[string]string{"@": "%40", ":": "%3A"},
			},
		},
		Cases: []relayEndpointCase{
			accepted("https-root", "https://relay.example", "wss://relay.example/v1/ws/AAAAAAAAAAAA"),
			accepted("http-subpath-query-fragment", "http://relay.example/base/?token=secret#fragment", "ws://relay.example/base/v1/ws/AAAAAAAAAAAA?token=secret"),
			accepted("userinfo", "wss://user:pass@relay.example/base?token=secret#fragment", "wss://user:pass@relay.example/base/v1/ws/AAAAAAAAAAAA?token=secret"),
			accepted("empty-userinfo-normalized", "wss://@relay.example/base", "wss://relay.example/base/v1/ws/AAAAAAAAAAAA"),
			accepted("empty-password-normalized", "wss://user:@relay.example/base", "wss://user@relay.example/base/v1/ws/AAAAAAAAAAAA"),
			accepted("at-sign-userinfo-normalized", "wss://user@tenant@relay.example/base", "wss://user%40tenant@relay.example/base/v1/ws/AAAAAAAAAAAA"),
			accepted("percent-encoded-query", "https://relay.example/base?token=a%20b", "wss://relay.example/base/v1/ws/AAAAAAAAAAAA?token=a%20b"),
			accepted("canonicalize-scheme-host-default-port", "HTTPS://RELAY.EXAMPLE:443/base", "wss://relay.example/base/v1/ws/AAAAAAAAAAAA"),
			accepted("dns-label-at-limit", "https://"+labelAtLimit+".example/base", "wss://"+labelAtLimit+".example/base/v1/ws/AAAAAAAAAAAA"),
			accepted("dns-name-at-limit", "https://"+nameAtLimit+"/base", "wss://"+nameAtLimit+"/base/v1/ws/AAAAAAAAAAAA"),
			accepted("fully-qualified-dns-name", "https://relay.example./base", "wss://relay.example./base/v1/ws/AAAAAAAAAAAA"),
			accepted("non-default-port", "ws://relay.example:8080/base", "ws://relay.example:8080/base/v1/ws/AAAAAAAAAAAA"),
			accepted("escaped-path-separator-preserved", "https://relay.example/base%2Ftenant", "wss://relay.example/base%2Ftenant/v1/ws/AAAAAAAAAAAA"),
			accepted("unicode-path", "https://relay.example/文件", "wss://relay.example/%E6%96%87%E4%BB%B6/v1/ws/AAAAAAAAAAAA"),
			accepted("internal-next-line-path", "https://relay.example/base\u0085segment", "wss://relay.example/base%C2%85segment/v1/ws/AAAAAAAAAAAA"),
			accepted("internal-byte-order-mark-path", "https://relay.example/base\ufeffsegment", "wss://relay.example/base%EF%BB%BFsegment/v1/ws/AAAAAAAAAAAA"),
			accepted("ipv6-canonicalized", "https://[2001:0DB8::0001]:443/base", "wss://[2001:db8::1]/base/v1/ws/AAAAAAAAAAAA"),
			rejected("unsupported-scheme", "ftp://relay.example"),
			rejected("relative", "/relay.example"),
			rejected("missing-host", "https:///base"),
			rejected("leading-whitespace", " https://relay.example"),
			rejected("trailing-whitespace", "https://relay.example "),
			rejected("leading-next-line", "\u0085https://relay.example"),
			rejected("trailing-next-line", "https://relay.example\u0085"),
			rejected("fragment-trailing-next-line", "https://relay.example#discarded\u0085"),
			rejected("leading-byte-order-mark", "\ufeffhttps://relay.example"),
			rejected("trailing-byte-order-mark", "https://relay.example\ufeff"),
			rejected("fragment-trailing-byte-order-mark", "https://relay.example#discarded\ufeff"),
			rejected("backslash-path", `https://relay.example/base\segment`),
			rejected("invalid-percent", "https://relay.example/%zz"),
			rejected("invalid-utf8-percent", "https://relay.example/%FF"),
			rejected("raw-query-space", "https://relay.example/base?token=a b"),
			rejected("raw-query-unicode", "https://relay.example/base?token=文件"),
			rejected("raw-unicode-userinfo", "https://user:密码@relay.example/base"),
			rejected("percent-encoded-userinfo", "https://us%2Fer@relay.example/base"),
			rejected("whatwg-repaired-userinfo", "https://u[s@relay.example/base"),
			rejected("percent-encoded-host", "https://%65xample.com/base"),
			rejected("unicode-host", "https://例え.テスト/base"),
			rejected("idna-case-mapping-host", "https://FAẞ.DE/base"),
			rejected("reserved-punycode-host", "https://xn--r8jz45g.xn--zckzah/base"),
			rejected("dns-label-above-limit", "https://"+labelAtLimit+"a.example/base"),
			rejected("dns-name-above-limit", "https://"+nameAboveLimit+"/base"),
			rejected("empty-dns-label", "https://relay..example/base"),
			rejected("underscore-host", "https://relay_internal.example/base"),
			rejected("leading-hyphen-host", "https://-relay.example/base"),
			rejected("hyphen-third-fourth-host", "https://re--lay.example/base"),
			rejected("literal-dot-segment", "https://relay.example/a/../b"),
			rejected("escaped-dot-segment", "https://relay.example/a/%2e%2e/b"),
			rejected("noncanonical-short-ipv4", "https://127.1"),
			rejected("noncanonical-integer-ipv4", "https://2130706433"),
			rejected("unicode-repaired-ipv4", "https://１２７.１/base"),
			rejected("ipv4-mapped-ipv6", "https://[::ffff:192.0.2.1]/base"),
			rejected("unbracketed-ipv6-with-port", "ws://2001:db8::1:8080/base"),
			rejected("out-of-range-port", "https://relay.example:70000"),
			rejected("control-character", "https://relay.example/\npath"),
		},
	}
}

func TestRelayEndpointVectorFileUpToDate(t *testing.T) {
	path := filepath.Join(vectorsDir, "relay-endpoint.json")
	want := renderVectorFile(t, buildRelayEndpointVectorFile())
	if *updateVectors {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test ./relay/protocol -update first): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it with go test ./relay/protocol -update and review the diff", path)
	}
}
