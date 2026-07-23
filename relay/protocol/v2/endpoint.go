package v2

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	WebSocketPath       = "/v2/ws"
	relayIdentityDomain = "windshare/v2 relay-identity\x00"
)

type RelayEndpoint struct {
	DialURL     string
	IdentityURL string
	Identity    RelayIdentity
}

// NormalizeRelayEndpoint is the single production implementation of the
// endpoint bytes signed by registration and stop. Query credentials remain on
// DialURL but are deliberately excluded from the long-lived relay identity.
func NormalizeRelayEndpoint(raw string) (RelayEndpoint, error) {
	if raw == "" || !utf8.ValidString(raw) || hasBoundaryWhitespace(raw) || strings.ContainsRune(raw, '\\') {
		return RelayEndpoint{}, fmt.Errorf("%w: relay base spelling", ErrIdentity)
	}
	for index := range len(raw) {
		if raw[index] <= 0x1f || raw[index] == 0x7f {
			return RelayEndpoint{}, fmt.Errorf("%w: relay base control byte", ErrIdentity)
		}
	}
	if strings.ContainsRune(raw, '#') {
		return RelayEndpoint{}, fmt.Errorf("%w: relay fragment", ErrIdentity)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.ForceQuery {
		return RelayEndpoint{}, fmt.Errorf("%w: relay base URL", ErrIdentity)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "ws":
		parsed.Scheme = "ws"
	case "https", "wss":
		parsed.Scheme = "wss"
	default:
		return RelayEndpoint{}, fmt.Errorf("%w: relay scheme", ErrIdentity)
	}
	if err := canonicalizeRelayAuthority(parsed); err != nil {
		return RelayEndpoint{}, err
	}
	escapedPath := parsed.EscapedPath()
	for segment := range strings.SplitSeq(escapedPath, "/") {
		decoded, decodeErr := url.PathUnescape(segment)
		if decodeErr != nil || !utf8.ValidString(decoded) || decoded == "." || decoded == ".." {
			return RelayEndpoint{}, fmt.Errorf("%w: relay base path", ErrIdentity)
		}
	}
	if !validRelayQuery(parsed.RawQuery) {
		return RelayEndpoint{}, fmt.Errorf("%w: relay query", ErrIdentity)
	}
	path := parsed.Path
	if trimmedEscapedPath, ok := strings.CutSuffix(escapedPath, "/"); ok {
		escapedPath = trimmedEscapedPath
		path = strings.TrimSuffix(path, "/")
	}
	parsed.Path = path + WebSocketPath
	parsed.RawPath = escapedPath + WebSocketPath
	parsed.Fragment, parsed.RawFragment = "", ""

	identityURL := *parsed
	identityURL.RawQuery = ""
	identityURL.ForceQuery = false
	identityASCII := identityURL.String()
	digest := sha256.Sum256(append([]byte(relayIdentityDomain), identityASCII...))
	return RelayEndpoint{DialURL: parsed.String(), IdentityURL: identityASCII, Identity: RelayIdentity(digest)}, nil
}

func hasBoundaryWhitespace(raw string) bool {
	first, _ := utf8.DecodeRuneInString(raw)
	last, _ := utf8.DecodeLastRuneInString(raw)
	return boundaryWhitespace(first) || boundaryWhitespace(last)
}

func boundaryWhitespace(character rune) bool {
	return character >= '\u0009' && character <= '\u000d' || character == '\u0020' ||
		character == '\u0085' || character == '\u00a0' || character == '\u1680' ||
		character >= '\u2000' && character <= '\u200a' || character == '\u2028' ||
		character == '\u2029' || character == '\u202f' || character == '\u205f' ||
		character == '\u3000' || character == '\ufeff'
}

func canonicalizeRelayAuthority(parsed *url.URL) error {
	hostname := parsed.Hostname()
	if hostname == "" || !asciiOnly(hostname) || strings.ContainsRune(hostname, '%') {
		return fmt.Errorf("%w: relay host", ErrIdentity)
	}
	port := parsed.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("%w: relay port", ErrIdentity)
		}
		port = strconv.FormatUint(value, 10)
	}
	host := strings.ToLower(hostname)
	if ip := net.ParseIP(hostname); ip != nil {
		host = strings.ToLower(ip.String())
	} else if !validRelayDNSName(host) || numericHost(host) {
		return fmt.Errorf("%w: relay host", ErrIdentity)
	}
	if parsed.Scheme == "ws" && port == "80" || parsed.Scheme == "wss" && port == "443" {
		port = ""
	}
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	parsed.Host = host
	return nil
}

func validRelayDNSName(hostname string) bool {
	name := strings.TrimSuffix(hostname, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") ||
			strings.HasSuffix(label, "-") || strings.HasPrefix(label, "xn--") ||
			len(label) > 3 && label[2:4] == "--" {
			return false
		}
		for index := range len(label) {
			if !asciiAlphaNumeric(label[index]) && label[index] != '-' {
				return false
			}
		}
	}
	return true
}

func numericHost(hostname string) bool {
	for _, character := range hostname {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validRelayQuery(query string) bool {
	for index := 0; index < len(query); index++ {
		character := query[index]
		if character >= utf8.RuneSelf {
			return false
		}
		if character == '%' {
			if index+2 >= len(query) || !asciiHex(query[index+1]) || !asciiHex(query[index+2]) {
				return false
			}
			index += 2
			continue
		}
		if !asciiAlphaNumeric(character) && !strings.ContainsRune("-._~!$&()*+,;=:@/?", rune(character)) {
			return false
		}
	}
	return true
}

func asciiOnly(value string) bool {
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func asciiHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f' ||
		character >= 'A' && character <= 'F'
}
