package link

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/nodedirectory"
	"github.com/windshare/windshare/core/urlhost"
)

const (
	httpDefaultPort   = "80"
	httpsDefaultPort  = "443"
	nodeParam         = "n"
	MaximumRelayHints = 8
)

// Relay selection belongs to the capability URL, so copying it into another
// frontend cannot redirect an implicit relay to that frontend's deployment.
func resolveRelays(page *url.URL, query url.Values) []string {
	if relays, explicit := query[relayParam]; explicit {
		for i, relay := range relays {
			if !strings.Contains(relay, "://") {
				relays[i] = "https://" + relay
			}
		}
		return relays
	}
	if _, aliases := query[nodeParam]; aliases {
		return nil
	}
	return []string{urlOrigin(page)}
}

func parseRelayHints(page *url.URL, query url.Values) ([]string, []string, error) {
	aliases := query[nodeParam]
	if err := validateHints(query[relayParam], aliases); err != nil {
		return nil, nil, err
	}
	return resolveRelays(page, query), aliases, nil
}

func validateHints(relays, aliases []string) error {
	if len(relays)+len(aliases) > MaximumRelayHints {
		return ErrMalformedLink
	}
	seen := map[string]bool{}
	for _, alias := range aliases {
		if !nodedirectory.ValidAlias(alias) || seen[alias] {
			return ErrMalformedLink
		}
		seen[alias] = true
	}
	if slices.Contains(relays, "") {
		return ErrMalformedLink
	}
	return nil
}

func relayHints(page *url.URL, capability Link) (url.Values, error) {
	if err := validateHints(capability.Relays, capability.NodeAliases); err != nil {
		return nil, err
	}
	relays := capability.Relays
	if len(capability.NodeAliases) == 0 {
		relays = explicitRelays(page, relays)
	}
	query := url.Values{}
	for _, alias := range capability.NodeAliases {
		query.Add(nodeParam, alias)
	}
	for _, relay := range relays {
		address, err := url.Parse(relay)
		if err == nil && address.Scheme == "https" && address.Host != "" && address.User == nil &&
			address.RawQuery == "" && !address.ForceQuery && address.Fragment == "" && (address.Path == "" || address.Path == "/") {
			relay = address.Host
		}
		query.Add(relayParam, relay)
	}
	return query, nil
}

func explicitRelays(page *url.URL, relays []string) []string {
	if len(relays) != 1 {
		return relays
	}
	relay, err := url.Parse(relays[0])
	if err != nil || relay.Host == "" || relay.User != nil ||
		relay.RawQuery != "" || relay.ForceQuery || strings.ContainsRune(relays[0], '#') ||
		(relay.EscapedPath() != "" && relay.EscapedPath() != "/") {
		return relays
	}
	switch relay.Scheme {
	case "ws":
		relay.Scheme = "http"
	case "wss":
		relay.Scheme = "https"
	case "http", "https":
	default:
		return relays
	}
	if err := urlhost.Normalize(relay); err != nil {
		return relays
	}
	// Only the root endpoint is implicit. Paths and query credentials can select
	// a different service even when its host and port match the frontend.
	if urlOrigin(relay) == urlOrigin(page) {
		return nil
	}
	return relays
}

func urlOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.ContainsRune(host, ':') {
		host = "[" + host + "]"
	}
	port := u.Port()
	if value, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(value, 10)
	}
	if (scheme == "http" || scheme == "ws") && port == httpDefaultPort ||
		(scheme == "https" || scheme == "wss") && port == httpsDefaultPort {
		port = ""
	}
	if port != "" {
		host += ":" + port
	}
	origin := url.URL{Scheme: scheme, Host: host}
	return origin.String()
}
