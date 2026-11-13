package v2

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/windshare/windshare/core/networkendpoint"
)

const WebSocketPath = networkendpoint.WebSocketPath
const ShareRouteQuery = networkendpoint.ShareRouteQuery

var ErrRelayEndpointWhitespace = fmt.Errorf("%w: %w", ErrIdentity, networkendpoint.ErrWhitespace)

type RelayEndpoint struct {
	DialURL     string
	IdentityURL string
	Identity    RelayIdentity
}

func NormalizeRelayEndpoint(raw string) (RelayEndpoint, error) {
	endpoint, err := networkendpoint.NormalizeRelay(raw)
	if errors.Is(err, networkendpoint.ErrWhitespace) {
		return RelayEndpoint{}, ErrRelayEndpointWhitespace
	}
	if err != nil {
		return RelayEndpoint{}, fmt.Errorf("%w: %w", ErrIdentity, err)
	}
	return RelayEndpoint{DialURL: endpoint.DialURL, IdentityURL: endpoint.IdentityURL, Identity: RelayIdentity(endpoint.Identity)}, nil
}

// RelayEndpointForShare adds the public upgrade-routing hint without changing
// the endpoint identity authenticated by REGISTER, RESUME and STOP.
func RelayEndpointForShare(raw string, shareID ShareID) (RelayEndpoint, error) {
	endpoint, err := NormalizeRelayEndpoint(raw)
	if err != nil {
		return RelayEndpoint{}, err
	}
	if !nonzero(shareID[:]) {
		return RelayEndpoint{}, fmt.Errorf("%w: relay routing share ID", ErrIdentity)
	}
	_, query, hasQuery := strings.Cut(endpoint.DialURL, "?")
	for pair := range strings.SplitSeq(query, "&") {
		key, _, _ := strings.Cut(pair, "=")
		decoded, err := url.QueryUnescape(key)
		if err != nil || decoded == ShareRouteQuery {
			// Even a matching value would make a deployment address share-specific.
			return RelayEndpoint{}, fmt.Errorf("%w: relay base reserves %s for share routing", ErrIdentity, ShareRouteQuery)
		}
	}
	separator := "?"
	if hasQuery {
		separator = "&"
	}
	// Re-encoding admission parameters can change signed query credentials.
	endpoint.DialURL += separator + ShareRouteQuery + "=" + base64.RawURLEncoding.EncodeToString(shareID[:])
	return endpoint, nil
}
