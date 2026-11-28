package urlhost

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	httpDefaultPort  = 80
	httpsDefaultPort = 443
)

var ErrInvalidOrigin = errors.New("invalid URL origin")

// Origin returns the canonical ASCII origin ("scheme://host[:port]") for an
// HTTP(S) or WebSocket URL, matching browser origin serialization: lowercase
// scheme and ASCII host, IDNA Punycode for Unicode domains, WHATWG IPv6
// formatting, and default ports (80/443) omitted. It does not mutate u.
func Origin(u *url.URL) (string, error) {
	if u == nil || u.Opaque != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return "", ErrInvalidOrigin
	}
	scheme := strings.ToLower(u.Scheme)
	var defaultPort uint64
	switch scheme {
	case "http", "ws":
		defaultPort = httpDefaultPort
	case "https", "wss":
		defaultPort = httpsDefaultPort
	default:
		return "", ErrInvalidOrigin
	}
	// Normalize operates in place; work on a shallow copy so Origin stays pure.
	normalized := *u
	if err := Normalize(&normalized); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidOrigin, err)
	}
	port, err := normalizeOriginPort(normalized.Port(), defaultPort)
	if err != nil {
		return "", err
	}
	host, err := normalizeOriginHost(normalized.Hostname(), strings.HasPrefix(normalized.Host, "["))
	if err != nil {
		return "", err
	}
	if port != "" {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}

func normalizeOriginPort(raw string, defaultPort uint64) (string, error) {
	if raw == "" {
		return "", nil
	}
	value, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || value == 0 {
		return "", ErrInvalidOrigin
	}
	if value == defaultPort {
		return "", nil
	}
	return strconv.FormatUint(value, 10), nil
}

func normalizeOriginHost(hostname string, bracketed bool) (string, error) {
	if ip, err := netip.ParseAddr(hostname); err == nil {
		if ip.Zone() != "" || bracketed != ip.Is6() {
			return "", ErrInvalidOrigin
		}
		if ip.Is6() {
			return "[" + formatWHATWGIPv6(ip) + "]", nil
		}
		return ip.String(), nil
	}
	if bracketed || hostname == "" || strings.ContainsFunc(hostname, isForbiddenHostRune) {
		return "", ErrInvalidOrigin
	}
	return strings.ToLower(hostname), nil
}

func isForbiddenHostRune(r rune) bool {
	return r <= 0x20 || r == 0x7f || strings.ContainsRune(forbiddenDomainCodePoints, r)
}

// WHATWG serializes IPv4-mapped IPv6 addresses as pure hex groups rather than
// RFC 5952 dotted-quad notation (e.g. ::ffff:c000:201 instead of ::ffff:192.0.2.1).
func formatWHATWGIPv6(ip netip.Addr) string {
	if ip.Is4In6() {
		addr := ip.As4()
		return fmt.Sprintf("::ffff:%x:%x", uint16(addr[0])<<8|uint16(addr[1]), uint16(addr[2])<<8|uint16(addr[3]))
	}
	return ip.String()
}
