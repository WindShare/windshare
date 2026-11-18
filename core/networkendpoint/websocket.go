package networkendpoint

import (
	"net/url"
	"strings"
)

// BaseFromWebSocketURL converts a full endpoint back to a candidate base.
// The escaped mount and routing query participate in dialing and must round-trip.
func BaseFromWebSocketURL(endpoint url.URL) (url.URL, bool) {
	path := strings.TrimSuffix(endpoint.EscapedPath(), "/")
	mount, ok := strings.CutSuffix(path, WebSocketPath)
	if !ok {
		return url.URL{}, false
	}
	if strings.HasSuffix(mount, "/") {
		// NormalizeRelay trims one optional slash from a base before dialing.
		mount += "/"
	}
	decoded, err := url.PathUnescape(mount)
	if err != nil {
		return url.URL{}, false
	}
	endpoint.Path, endpoint.RawPath = decoded, mount
	return endpoint, true
}

// RelayBaseHint is a display address. Routing queries never enter diagnostics.
type RelayBaseHint struct{ address string }

func (hint RelayBaseHint) URL() string { return hint.address }

// WebSocketBaseHint can explain a failed route or a discovery mismatch. A suffix
// alone says nothing about whether the original candidate is a working mount.
func WebSocketBaseHint(address string) (RelayBaseHint, bool) {
	parsed, err := url.Parse(address)
	if err != nil {
		return RelayBaseHint{}, false
	}
	base, ok := BaseFromWebSocketURL(*parsed)
	if !ok {
		return RelayBaseHint{}, false
	}
	if _, err := NormalizeRelay(address); err != nil {
		return RelayBaseHint{}, false
	}
	base.RawQuery = ""
	return RelayBaseHint{address: base.String()}, true
}
