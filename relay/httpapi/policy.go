package httpapi

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// normalizeOrigins validates deployment policy before serving. Ignoring a typo
// would leave an apparently healthy relay that rejects every intended browser.
func normalizeOrigins(origins []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		normalized, ok := normalizeOrigin(origin)
		if !ok {
			return nil, fmt.Errorf("httpapi: invalid allowed origin %q", origin)
		}
		set[normalized] = struct{}{}
	}
	return set, nil
}

func normalizeOrigin(origin string) (string, bool) {
	origin = strings.TrimSpace(origin)
	if origin == "" || strings.ContainsAny(origin, "?#") {
		return "", false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.ForceQuery ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", false
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", false
	}
	if ip := net.ParseIP(hostname); ip != nil {
		hostname = ip.String()
	} else if strings.Contains(hostname, ":") || isNumericHost(hostname) {
		return "", false
	}
	port := parsed.Port()
	if port == "" && strings.HasSuffix(parsed.Host, ":") {
		return "", false
	}
	if port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return "", false
		}
		port = strconv.FormatUint(value, 10)
	}
	if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, true
}

func isNumericHost(host string) bool {
	if host == "" {
		return false
	}
	for _, character := range host {
		if (character < '0' || character > '9') && character != '.' {
			return false
		}
	}
	return true
}

func originAllowed(origin string, allowed map[string]struct{}, allowLocalhost bool) bool {
	if origin == "" {
		return true
	}
	normalized, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	if _, ok := allowed[normalized]; ok {
		return true
	}
	if allowLocalhost {
		parsed, _ := url.Parse(normalized)
		switch parsed.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return true
		}
	}
	return false
}

func remoteIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}
