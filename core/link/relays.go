package link

import (
	"net/url"
	"strconv"
	"strings"
)

const (
	httpDefaultPort  = "80"
	httpsDefaultPort = "443"
)

// Relay selection belongs to the capability URL, so copying it into another
// frontend cannot redirect an implicit relay to that frontend's deployment.
func resolveRelays(page *url.URL, query url.Values) []string {
	if relays, explicit := query[relayParam]; explicit {
		return relays
	}
	return []string{urlOrigin(page)}
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
