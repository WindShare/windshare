package commandprojection

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func NormalizeRelayAuthority(raw string) (clievent.RelayAuthority, error) {
	endpoint, err := v2.NormalizeRelayEndpoint(raw)
	if err != nil {
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	return RelayAuthority(endpoint)
}

func RelayAuthority(endpoint v2.RelayEndpoint) (clievent.RelayAuthority, error) {
	// Re-normalization proves that the exported endpoint struct was not assembled
	// or mutated by a caller. Only authority components survive this boundary.
	dial, err := url.Parse(endpoint.DialURL)
	if err != nil || !strings.HasSuffix(dial.Path, v2.WebSocketPath) {
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	base := *dial
	base.Path = strings.TrimSuffix(base.Path, v2.WebSocketPath)
	if base.RawPath != "" {
		if !strings.HasSuffix(base.RawPath, v2.WebSocketPath) {
			return clievent.RelayAuthority{}, ErrInvalidProjection
		}
		base.RawPath = strings.TrimSuffix(base.RawPath, v2.WebSocketPath)
	}
	normalized, err := v2.NormalizeRelayEndpoint(base.String())
	if err != nil || normalized.DialURL != endpoint.DialURL ||
		normalized.IdentityURL != endpoint.IdentityURL || normalized.Identity != endpoint.Identity {
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	parsed := dial
	var scheme clievent.RelayScheme
	switch parsed.Scheme {
	case "ws":
		scheme = clievent.RelayWS
	case "wss":
		scheme = clievent.RelayWSS
	default:
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	port := parsed.Port()
	if port == "" {
		if scheme == clievent.RelayWS {
			port = "80"
		} else {
			port = "443"
		}
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	authority, err := clievent.NewRelayAuthority(scheme, parsed.Hostname(), uint16(value))
	if err != nil {
		return clievent.RelayAuthority{}, ErrInvalidProjection
	}
	return authority, nil
}
