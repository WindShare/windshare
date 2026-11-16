package networkendpoint

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/urlhost"
)

const (
	WebSocketPath       = "/v2/ws"
	ShareRouteQuery     = "share_id"
	relayIdentityDomain = "windshare/v2 relay-identity\x00"
)

var ErrInvalid = errors.New("invalid network endpoint")

// ErrWhitespace identifies rejected spacing at the validation source,
// so callers do not mistake otherwise valid path contents for the failure cause.
var ErrWhitespace = fmt.Errorf("%w: relay whitespace", ErrInvalid)

type Relay struct {
	DialURL     string
	IdentityURL string
	Identity    [sha256.Size]byte
}

// NormalizeRelay is the single production implementation of the
// endpoint bytes signed by registration and stop. Query credentials remain on
// DialURL but are deliberately excluded from the long-lived relay identity.
func NormalizeRelay(raw string) (Relay, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsRune(raw, '\\') {
		return Relay{}, fmt.Errorf("%w: relay base spelling", ErrInvalid)
	}
	if hasBoundaryWhitespace(raw) {
		return Relay{}, ErrWhitespace
	}
	for index := range len(raw) {
		if raw[index] <= 0x1f || raw[index] == 0x7f {
			if boundaryWhitespace(rune(raw[index])) {
				return Relay{}, ErrWhitespace
			}
			return Relay{}, fmt.Errorf("%w: relay base control byte", ErrInvalid)
		}
	}
	if strings.ContainsRune(raw, '#') {
		return Relay{}, fmt.Errorf("%w: relay fragment", ErrInvalid)
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.ForceQuery {
		return Relay{}, fmt.Errorf("%w: relay base URL", ErrInvalid)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "ws":
		parsed.Scheme = "ws"
	case "https", "wss":
		parsed.Scheme = "wss"
	default:
		return Relay{}, fmt.Errorf("%w: relay scheme", ErrInvalid)
	}
	if err := canonicalizeRelayAuthority(parsed); err != nil {
		return Relay{}, err
	}
	escapedPath := parsed.EscapedPath()
	for segment := range strings.SplitSeq(escapedPath, "/") {
		decoded, decodeErr := url.PathUnescape(segment)
		if decodeErr != nil || !utf8.ValidString(decoded) || decoded == "." || decoded == ".." {
			return Relay{}, fmt.Errorf("%w: relay base path", ErrInvalid)
		}
	}
	if !validRelayQuery(parsed.RawQuery) {
		return Relay{}, fmt.Errorf("%w: relay query", ErrInvalid)
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
	return Relay{DialURL: parsed.String(), IdentityURL: identityASCII, Identity: digest}, nil
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
	if err := urlhost.Normalize(parsed); err != nil {
		return fmt.Errorf("%w: relay host", ErrInvalid)
	}
	hostname := parsed.Hostname()
	port := parsed.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("%w: relay port", ErrInvalid)
		}
		port = strconv.FormatUint(value, 10)
	}
	host := strings.ToLower(hostname)
	if ip, err := netip.ParseAddr(hostname); err == nil && ip.Zone() == "" {
		host = ip.Unmap().String()
	} else if strings.HasPrefix(parsed.Host, "[") {
		return fmt.Errorf("%w: bracketed relay host must be an IP literal", ErrInvalid)
	} else if err := validateDNSHost(host); err != nil {
		return err
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

func asciiAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func asciiHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f' ||
		character >= 'A' && character <= 'F'
}
