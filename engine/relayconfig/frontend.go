package relayconfig

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

var ErrFrontURL = errors.New("frontend URL must be an absolute HTTP(S) base URL without credentials, query or fragment")

const maximumPort = 65535

// Reject components that capability construction would discard. Accepting them
// would silently change the website the user thought they had configured.
func ValidateFrontURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		strings.ContainsAny(raw, "#\\\t\r\n ") || strings.HasSuffix(u.Host, ":") {
		return ErrFrontURL
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 || value > maximumPort {
			return ErrFrontURL
		}
	}
	return nil
}
