package protocolcontract

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/core/urlhost"
)

type v2RelayEndpointVector struct {
	Name                  string `json:"name"`
	RelayBaseURL          string `json:"relayBaseUrl"`
	Accepted              bool   `json:"accepted"`
	DialEndpoint          string `json:"dialEndpoint,omitempty"`
	RelayIdentityEndpoint string `json:"relayIdentityEndpoint,omitempty"`
	RelayIdentityB64      string `json:"relayIdentityB64,omitempty"`
}

func TestV2RelayEndpointNormalization(t *testing.T) {
	for _, vector := range v2RelayEndpointCases() {
		t.Run(vector.Name, func(t *testing.T) {
			dial, identityEndpoint, err := canonicalV2RelayEndpoints(vector.RelayBaseURL)
			if !vector.Accepted {
				if err == nil {
					t.Fatalf("accepted rejected relay base as %q", dial)
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalize relay base: %v", err)
			}
			if dial != vector.DialEndpoint || identityEndpoint != vector.RelayIdentityEndpoint {
				t.Fatalf("endpoint = (%q, %q), want (%q, %q)", dial, identityEndpoint, vector.DialEndpoint, vector.RelayIdentityEndpoint)
			}
			identity := hash(slices.Concat([]byte(domainRelayIdentity), []byte(identityEndpoint)))
			if b64(identity) != vector.RelayIdentityB64 {
				t.Fatal("relay identity did not bind the query-free canonical endpoint")
			}
		})
	}
}

func TestDescriptorUploadAndDeliveryFramesAreDirectionSpecific(t *testing.T) {
	descriptor := fixed(0x31, 37)
	relaySessionID := fixed(0x71, 8)
	upload := encodeDescriptorUpload(descriptor)
	delivery := encodeDescriptorDelivery(relaySessionID, descriptor)
	if !bytes.Equal(upload[:4], []byte("WS2U")) || !bytes.Equal(delivery[:4], []byte("WS2D")) {
		t.Fatal("descriptor frame magics no longer distinguish upload from delivery")
	}
	if binary.BigEndian.Uint32(upload[8:12]) != uint32(len(descriptor)) || !bytes.Equal(upload[12:], descriptor) {
		t.Fatal("descriptor upload layout changed")
	}
	if !bytes.Equal(delivery[8:16], relaySessionID) ||
		binary.BigEndian.Uint32(delivery[16:20]) != uint32(len(descriptor)) ||
		!bytes.Equal(delivery[20:], descriptor) {
		t.Fatal("descriptor delivery layout changed")
	}
	if len(delivery)-len(upload) != len(relaySessionID) {
		t.Fatal("upload must not carry an unavailable RelaySessionID")
	}
}

func encodeDescriptorUpload(descriptor []byte) []byte {
	return slices.Concat(
		[]byte("WS2U"), []byte{wireVersion, 0, 0, 0}, u32(uint32(len(descriptor))), descriptor,
	)
}

func encodeDescriptorDelivery(relaySessionID, descriptor []byte) []byte {
	return slices.Concat(
		[]byte("WS2D"), []byte{wireVersion, 0, 0, 0}, relaySessionID,
		u32(uint32(len(descriptor))), descriptor,
	)
}

func v2RelayEndpointCases() []v2RelayEndpointVector {
	accepted := func(name, relayBaseURL, dialEndpoint, identityEndpoint string) v2RelayEndpointVector {
		return v2RelayEndpointVector{
			Name: name, RelayBaseURL: relayBaseURL, Accepted: true, DialEndpoint: dialEndpoint,
			RelayIdentityEndpoint: identityEndpoint,
			RelayIdentityB64:      b64(hash(slices.Concat([]byte(domainRelayIdentity), []byte(identityEndpoint)))),
		}
	}
	rejected := func(name, relayBaseURL string) v2RelayEndpointVector {
		return v2RelayEndpointVector{Name: name, RelayBaseURL: relayBaseURL}
	}
	return []v2RelayEndpointVector{
		accepted("https-root", "https://relay.example", "wss://relay.example/v2/ws", "wss://relay.example/v2/ws"),
		accepted("canonicalize-scheme-host-default-port", "HTTPS://RELAY.EXAMPLE:443/base/", "wss://relay.example/base/v2/ws", "wss://relay.example/base/v2/ws"),
		accepted("query-kept-outside-identity", "https://relay.example/base/?token=a%20b", "wss://relay.example/base/v2/ws?token=a%20b", "wss://relay.example/base/v2/ws"),
		accepted("local-ws-port", "http://127.0.0.1:8080/relay", "ws://127.0.0.1:8080/relay/v2/ws", "ws://127.0.0.1:8080/relay/v2/ws"),
		accepted("unicode-latin", "https://münchen.de", "wss://xn--mnchen-3ya.de/v2/ws", "wss://xn--mnchen-3ya.de/v2/ws"),
		accepted("punycode-latin", "https://xn--mnchen-3ya.de", "wss://xn--mnchen-3ya.de/v2/ws", "wss://xn--mnchen-3ya.de/v2/ws"),
		accepted("unicode-uppercase", "https://MÜNCHEN.DE:443/", "wss://xn--mnchen-3ya.de/v2/ws", "wss://xn--mnchen-3ya.de/v2/ws"),
		accepted("unicode-cjk-port-and-credentials", "https://文件.example:8443/base/?token=a%2fb+~&flag", "wss://xn--5nqy36c.example:8443/base/v2/ws?token=a%2fb+~&flag", "wss://xn--5nqy36c.example:8443/base/v2/ws"),
		accepted("unicode-nontransitional", "https://straße.de", "wss://xn--strae-oqa.de/v2/ws", "wss://xn--strae-oqa.de/v2/ws"),
		accepted("unicode-emoji", "https://ex😀.com", "wss://xn--ex-oo82a.com/v2/ws", "wss://xn--ex-oo82a.com/v2/ws"),
		accepted("unicode-fullwidth", "https://ＥＸＡＭＰＬＥ.com", "wss://example.com/v2/ws", "wss://example.com/v2/ws"),
		accepted("unicode-dot", "https://ex。com", "wss://ex.com/v2/ws", "wss://ex.com/v2/ws"),
		accepted("unicode-trailing-dot", "https://ü.com.", "wss://xn--tda.com./v2/ws", "wss://xn--tda.com./v2/ws"),
		rejected("erased-domain", "https://\u00ad"),
		rejected("erased-domain-with-port", "https://\u200b:8443"),
		rejected("invalid-joiner", "https://a\u200db.com"),
		rejected("fullwidth-port-delimiter", "https://ü.com：8080"),
		rejected("fullwidth-path-delimiter", "https://ü.com／evil"),
		rejected("invalid-punycode", "https://xn--test.example"),
		rejected("empty-punycode", "https://xn--.example"),
		rejected("unicode-with-empty-punycode-label", "https://b\u00fccher.xn--.example"),
		rejected("unicode-with-ascii-only-punycode-label", "https://b\u00fccher.xn--abc-.example"),
		rejected("reserved-ascii-label", "https://ab--cd.example"),
		rejected("userinfo", "https://user:pass@relay.example/base"),
		rejected("fragment", "https://relay.example/base#redirect"),
		rejected("dot-segment", "https://relay.example/a/../base"),
		rejected("boundary-whitespace", " https://relay.example"),
		rejected("raw-unicode-query", "https://relay.example/base?token=文件"),
	}
}

func canonicalV2RelayEndpoints(raw string) (string, string, error) {
	if raw == "" || !utf8.ValidString(raw) || hasRelayEndpointBoundaryWhitespace(raw) || strings.ContainsRune(raw, '\\') {
		return "", "", fmt.Errorf("relay base spelling is invalid")
	}
	for index := range len(raw) {
		if raw[index] <= 0x1f || raw[index] == 0x7f {
			return "", "", fmt.Errorf("relay base contains a control byte")
		}
	}
	if strings.ContainsRune(raw, '#') {
		return "", "", fmt.Errorf("v2 relay base must not contain a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" || u.User != nil || u.ForceQuery {
		return "", "", fmt.Errorf("relay base URL is invalid")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", "", fmt.Errorf("relay base scheme is unsupported")
	}
	if err := canonicalizeV2RelayAuthority(u); err != nil {
		return "", "", err
	}
	escapedPath := u.EscapedPath()
	for segment := range strings.SplitSeq(escapedPath, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !utf8.ValidString(decoded) || decoded == "." || decoded == ".." {
			return "", "", fmt.Errorf("relay base path is invalid")
		}
	}
	if !validV2RelayQuery(u.RawQuery) {
		return "", "", fmt.Errorf("relay base query is invalid")
	}
	path := u.Path
	if trimmed, ok := strings.CutSuffix(escapedPath, "/"); ok {
		escapedPath = trimmed
		path = strings.TrimSuffix(path, "/")
	}
	u.Path = path + v2RelayWebSocketPath
	u.RawPath = escapedPath + v2RelayWebSocketPath
	u.Fragment, u.RawFragment = "", ""

	identity := *u
	identity.RawQuery = ""
	identity.ForceQuery = false
	return u.String(), identity.String(), nil
}

func hasRelayEndpointBoundaryWhitespace(raw string) bool {
	firstRune, _ := utf8.DecodeRuneInString(raw)
	lastRune, _ := utf8.DecodeLastRuneInString(raw)
	return isRelayEndpointBoundaryWhitespace(firstRune) || isRelayEndpointBoundaryWhitespace(lastRune)
}

func isRelayEndpointBoundaryWhitespace(character rune) bool {
	return character >= '\u0009' && character <= '\u000d' || character == '\u0020' ||
		character == '\u0085' || character == '\u00a0' || character == '\u1680' ||
		character >= '\u2000' && character <= '\u200a' || character == '\u2028' ||
		character == '\u2029' || character == '\u202f' || character == '\u205f' ||
		character == '\u3000' || character == '\ufeff'
}

func canonicalizeV2RelayAuthority(u *url.URL) error {
	if err := urlhost.Normalize(u); err != nil {
		return fmt.Errorf("relay host is invalid")
	}
	hostname := u.Hostname()
	port := u.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("relay port is invalid")
		}
		port = strconv.FormatUint(value, 10)
	}
	canonicalHost := strings.ToLower(hostname)
	if ip := net.ParseIP(hostname); ip != nil {
		canonicalHost = strings.ToLower(ip.String())
	} else if !validV2RelayDNSName(canonicalHost) || numericRelayHost(canonicalHost) {
		return fmt.Errorf("relay host is invalid")
	}
	if (u.Scheme == "ws" && port == "80") || (u.Scheme == "wss" && port == "443") {
		port = ""
	}
	if strings.ContainsRune(canonicalHost, ':') {
		canonicalHost = "[" + canonicalHost + "]"
	}
	if port != "" {
		canonicalHost += ":" + port
	}
	u.Host = canonicalHost
	return nil
}

func validV2RelayDNSName(hostname string) bool {
	name := strings.TrimSuffix(hostname, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") ||
			strings.HasSuffix(label, "-") ||
			len(label) > 3 && label[2:4] == "--" && !strings.HasPrefix(label, "xn--") {
			return false
		}
		for index := range len(label) {
			character := label[index]
			if !isASCIIAlphaNumeric(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func numericRelayHost(hostname string) bool {
	for _, character := range hostname {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validV2RelayQuery(query string) bool {
	for index := 0; index < len(query); index++ {
		character := query[index]
		if character >= utf8.RuneSelf {
			return false
		}
		if character == '%' {
			if index+2 >= len(query) || !isASCIIHex(query[index+1]) || !isASCIIHex(query[index+2]) {
				return false
			}
			index += 2
			continue
		}
		if !isASCIIAlphaNumeric(character) && !strings.ContainsRune("-._~!$&()*+,;=:@/?", rune(character)) {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func isASCIIHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f' ||
		character >= 'A' && character <= 'F'
}
