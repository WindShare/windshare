package relay

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/relay/protocol"
)

const (
	maxRelayDNSLabelBytes = 63
	maxRelayDNSNameBytes  = 253
)

func relayWebSocketURL(relayURL, shareID string) (*url.URL, error) {
	if err := protocol.ValidateShareID(shareID); err != nil {
		return nil, err
	}
	if !validRelayURLSpelling(relayURL) {
		return nil, &relayURLSyntaxError{}
	}
	u, err := url.Parse(relayURL)
	if err != nil || u.Host == "" || u.Opaque != "" {
		// Parser causes retain the complete credential-bearing URL. The wrapper
		// preserves errors.Is/As behavior without making that input printable.
		return nil, &relayURLSyntaxError{cause: err}
	}
	if hasDotPathSegment(u.EscapedPath()) {
		// WHATWG removes dot segments while net/url preserves them. Rejecting the
		// ambiguous spelling prevents one capability hint from selecting two paths.
		return nil, &relayURLSyntaxError{}
	}

	switch strings.ToLower(u.Scheme) {
	case "ws", "http":
		u.Scheme = "ws"
	case "wss", "https":
		u.Scheme = "wss"
	default:
		return nil, errors.New("relay: unsupported relay URL scheme")
	}
	canonicalizeRelayUserinfo(u)
	if err := canonicalizeRelayAuthority(u); err != nil {
		return nil, &relayURLSyntaxError{cause: err}
	}

	pathSuffix := "/" + protocol.ProtocolVersion + "/ws/" + shareID
	path := u.Path
	escapedPath := u.EscapedPath()
	// Only a literal trailing slash is a deployment separator. An escaped %2F
	// belongs to the final segment and must survive endpoint construction.
	if trimmed, ok := strings.CutSuffix(escapedPath, "/"); ok {
		escapedPath = trimmed
		path, _ = strings.CutSuffix(path, "/")
	}
	u.Path = path + pathSuffix
	u.RawPath = escapedPath + pathSuffix
	u.Fragment = ""
	u.RawFragment = ""
	return u, nil
}

func validRelayURLSpelling(raw string) bool {
	if raw == "" || hasRelayURLBoundaryWhitespace(raw) || !utf8.ValidString(raw) || strings.ContainsRune(raw, '\\') {
		return false
	}
	for index := range len(raw) {
		if raw[index] <= 0x1f || raw[index] == 0x7f {
			return false
		}
	}
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || colon+3 >= len(raw) || raw[colon:colon+3] != "://" {
		return false
	}
	for index := range colon {
		character := raw[index]
		if index == 0 {
			if !isASCIIAlpha(character) {
				return false
			}
			continue
		}
		if !(isASCIIAlpha(character) ||
			character >= '0' && character <= '9' || character == '+' || character == '.' || character == '-') {
			return false
		}
	}
	authorityStart := colon + 3
	relativeAuthorityEnd := strings.IndexAny(raw[authorityStart:], "/?#")
	if relativeAuthorityEnd == 0 {
		return false
	}
	authorityEnd := len(raw)
	if relativeAuthorityEnd >= 0 {
		authorityEnd = authorityStart + relativeAuthorityEnd
	}
	return validRawRelayUserinfo(raw, authorityStart, authorityEnd) &&
		validRawRelayHost(raw, authorityStart, authorityEnd) &&
		validRawRelayPath(raw, authorityEnd) && validRawRelayQuery(raw)
}

// URL parsers disagree on their ambient trim sets (notably U+0085 and
// U+FEFF). The wire contract owns one explicit set so a discarded prefix or
// fragment suffix cannot make the runtimes interpret different URL text.
func hasRelayURLBoundaryWhitespace(raw string) bool {
	first, _ := utf8.DecodeRuneInString(raw)
	last, _ := utf8.DecodeLastRuneInString(raw)
	return isRelayURLBoundaryWhitespace(first) || isRelayURLBoundaryWhitespace(last)
}

func isRelayURLBoundaryWhitespace(character rune) bool {
	return character >= '\u0009' && character <= '\u000d' ||
		character == '\u0020' || character == '\u0085' || character == '\u00a0' ||
		character == '\u1680' || character >= '\u2000' && character <= '\u200a' ||
		character == '\u2028' || character == '\u2029' || character == '\u202f' ||
		character == '\u205f' || character == '\u3000' || character == '\ufeff'
}

// net/url decodes userinfo percent escapes and emits a new uppercase spelling,
// while WHATWG preserves the original escape spelling. Rejecting that lossy
// boundary and their different punctuation tables keeps the derived endpoint
// byte-identical without weakening credential redaction.
func validRawRelayUserinfo(raw string, authorityStart, authorityEnd int) bool {
	authority := raw[authorityStart:authorityEnd]
	separator := strings.LastIndexByte(authority, '@')
	if separator < 0 {
		return true
	}
	for index := range separator {
		character := authority[index]
		if !(isASCIIAlpha(character) || character >= '0' && character <= '9' ||
			strings.ContainsRune("-._~:@", rune(character))) {
			return false
		}
	}
	return true
}

func validRawRelayHost(raw string, authorityStart, authorityEnd int) bool {
	authority := raw[authorityStart:authorityEnd]
	if separator := strings.LastIndexByte(authority, '@'); separator >= 0 {
		authority = authority[separator+1:]
	}
	if strings.HasPrefix(authority, "[") {
		// The parser remains responsible for IPv6 and port syntax, but a bracketed
		// literal is the only spelling that may carry multiple colons.
		return true
	}
	return strings.Count(authority, ":") <= 1 && !strings.ContainsAny(authority, "[]")
}

// Go and WHATWG use different path percent-encode sets. Restricting raw ASCII
// to the shared literal subset avoids silently deriving two endpoint spellings;
// valid percent escapes and Unicode scalar text retain their explicit spelling.
func validRawRelayPath(raw string, authorityEnd int) bool {
	if authorityEnd >= len(raw) || raw[authorityEnd] != '/' {
		return true
	}
	pathEnd := strings.IndexAny(raw[authorityEnd:], "?#")
	if pathEnd < 0 {
		pathEnd = len(raw)
	} else {
		pathEnd += authorityEnd
	}
	for index := authorityEnd; index < pathEnd; index++ {
		character := raw[index]
		if character >= utf8.RuneSelf {
			continue
		}
		if character == '%' {
			if index+2 >= pathEnd || !isASCIIHex(raw[index+1]) || !isASCIIHex(raw[index+2]) {
				return false
			}
			index += 2
			continue
		}
		if !(isASCIIAlpha(character) || character >= '0' && character <= '9' ||
			strings.ContainsRune("-._~$&+,/:;=@", rune(character))) {
			return false
		}
	}
	return true
}

func isASCIIAlpha(character byte) bool {
	return character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
}

// net/url emits RawQuery verbatim while WHATWG percent-encodes unsafe text.
// Requiring URI-safe spelling keeps credentials byte-identical across dialers.
func validRawRelayQuery(raw string) bool {
	start := strings.IndexByte(raw, '?')
	if start < 0 {
		return true
	}
	end := strings.IndexByte(raw[start+1:], '#')
	if end < 0 {
		end = len(raw)
	} else {
		end += start + 1
	}
	for index := start + 1; index < end; index++ {
		character := raw[index]
		if character == '%' {
			if index+2 >= end || !isASCIIHex(raw[index+1]) || !isASCIIHex(raw[index+2]) {
				return false
			}
			index += 2
			continue
		}
		if !(isASCIIAlpha(character) || character >= '0' && character <= '9' ||
			strings.ContainsRune("-._~!$&()*+,;=:@/?", rune(character))) {
			return false
		}
	}
	return true
}

func isASCIIHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'A' && character <= 'F' ||
		character >= 'a' && character <= 'f'
}

func hasDotPathSegment(escapedPath string) bool {
	for segment := range strings.SplitSeq(escapedPath, "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil || !utf8.ValidString(decoded) || decoded == "." || decoded == ".." {
			return true
		}
	}
	return false
}

func canonicalizeRelayUserinfo(u *url.URL) {
	if u.User == nil {
		return
	}
	username := u.User.Username()
	password, hasPassword := u.User.Password()
	switch {
	case username == "" && (!hasPassword || password == ""):
		// WHATWG removes an empty credential tuple entirely.
		u.User = nil
	case hasPassword && password == "":
		// WHATWG also removes the delimiter for an empty password.
		u.User = url.User(username)
	}
}

func canonicalizeRelayAuthority(u *url.URL) error {
	hostname := u.Hostname()
	if hostname == "" || strings.ContainsRune(hostname, '%') || !isASCII(hostname) {
		return errors.New("relay host is invalid")
	}
	port := u.Port()
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return errors.New("relay port is invalid")
		}
		port = strconv.FormatUint(value, 10)
	}

	canonicalHost := ""
	if ip := net.ParseIP(hostname); ip != nil {
		if strings.ContainsRune(hostname, ':') && ip.To4() != nil {
			// net.IP serializes mapped IPv6 as IPv4, while WHATWG preserves an
			// IPv6 literal. Reject the dual spelling instead of dialing another host.
			return errors.New("relay host uses an IPv4-mapped IPv6 spelling")
		}
		canonicalHost = strings.ToLower(ip.String())
	} else {
		canonicalHost = strings.ToLower(hostname)
		if !validRelayDNSName(canonicalHost) {
			return errors.New("relay host is invalid")
		}
		if numericHost(canonicalHost) {
			return errors.New("relay host uses a non-canonical numeric spelling")
		}
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

func isASCII(value string) bool {
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Independent IDNA implementations drift with their Unicode data and have
// selected different authorities for the same hint. Relay deployment names
// therefore use an explicit ASCII DNS repertoire. The xn-- namespace stays
// reserved until the protocol owns one versioned IDNA implementation.
func validRelayDNSName(hostname string) bool {
	name := strings.TrimSuffix(hostname, ".")
	if name == "" || len(name) > maxRelayDNSNameBytes {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > maxRelayDNSLabelBytes ||
			strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") ||
			strings.HasPrefix(label, "xn--") ||
			len(label) > 3 && label[2:4] == "--" {
			return false
		}
		for index := range len(label) {
			character := label[index]
			if !(isASCIIAlpha(character) || character >= '0' && character <= '9' || character == '-') {
				return false
			}
		}
	}
	return true
}

func numericHost(hostname string) bool {
	lower := strings.ToLower(hostname)
	if strings.HasPrefix(lower, "0x") || strings.Contains(lower, ".0x") {
		return true
	}
	for _, character := range lower {
		if character != '.' && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}
