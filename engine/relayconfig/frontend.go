package relayconfig

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/windshare/windshare/core/link"
)

var ErrFrontURL = errors.New("frontend URL must be an absolute HTTP(S) base URL without credentials, query or fragment")

const maximumPort = 65535

// Reject components that capability construction would discard or percent-encode
// into the path. Accepting them would silently change the website the user
// thought they had configured.
func ValidateFrontURL(raw string) error {
	// Share links trim copied whitespace, then JoinPath the share id. A saved
	// frontend that still carries JS-trim characters (BOM, NBSP, U+2028, …)
	// would publish a different website than the one stored here; U+0085 is
	// not trimmed but becomes /app%C2%85/… in the capability URL.
	if raw != link.TrimCopiedText(raw) || strings.IndexFunc(raw, forbiddenFrontURLRune) >= 0 {
		return ErrFrontURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		strings.ContainsAny(raw, `#\`) || strings.HasSuffix(u.Host, ":") {
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

// Controls and Unicode spaces anywhere in the string, including past TrimCopiedText's
// ends, are percent-encoded into the path by url.JoinPath.
func forbiddenFrontURLRune(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u0085', '\u2028', '\u2029', '\ufeff':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}
