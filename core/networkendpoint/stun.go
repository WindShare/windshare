package networkendpoint

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/urlhost"
)

// NormalizeSTUN accepts the UDP endpoint contract shared by native and browser ICE.
// It only validates deployment data; it never resolves or probes the address.
func NormalizeSTUN(raw string) (string, error) {
	if !strings.HasPrefix(raw, "stun:") || strings.ContainsAny(raw, "/?#@\\ \t\r\n") {
		return "", fmt.Errorf("%w: STUN spelling", ErrInvalid)
	}
	parsed, err := url.Parse("stun://" + strings.TrimPrefix(raw, "stun:"))
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return "", fmt.Errorf("%w: STUN authority", ErrInvalid)
	}
	if strings.ContainsRune(parsed.Hostname(), ':') && !strings.HasPrefix(parsed.Host, "[") {
		return "", fmt.Errorf("%w: STUN IPv6 brackets", ErrInvalid)
	}
	number, err := strconv.ParseUint(parsed.Port(), 10, 16)
	if err != nil || number == 0 {
		return "", fmt.Errorf("%w: STUN port", ErrInvalid)
	}
	if err := urlhost.Normalize(parsed); err != nil {
		return "", fmt.Errorf("%w: STUN host", ErrInvalid)
	}
	host := strings.ToLower(parsed.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
		host = ip.Unmap().String()
	} else if strings.HasPrefix(parsed.Host, "[") {
		return "", fmt.Errorf("%w: bracketed STUN host must be an IP literal", ErrInvalid)
	} else if !validRelayDNSName(host) || numericHost(host) {
		return "", fmt.Errorf("%w: STUN host", ErrInvalid)
	}
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	return "stun:" + host + ":" + strconv.FormatUint(number, 10), nil
}
