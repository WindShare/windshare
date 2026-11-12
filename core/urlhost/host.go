// Package urlhost normalizes URL domain names without acquiring network capabilities.
package urlhost

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

var ErrInvalidHost = errors.New("invalid URL host")

const (
	punycodePrefix            = "xn--"
	forbiddenDomainCodePoints = " #/:<>?@[\\]^|%"
)

// URL lookup uses compatibility mapping without DNS registration's hyphen or
// length restrictions. Relay endpoints apply their DNS limits after conversion.
// Go's BidiRule additionally refuses mixed-direction names some browsers accept.
var lookupProfile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.CheckJoiners(true),
	idna.BidiRule(),
	idna.VerifyDNSLength(false),
)

// Normalize gives Unicode and Punycode spellings one domain representation.
// It changes only the host: paths and query credentials retain their exact bytes.
// ASCII hosts without A-labels, including IP literals, need no IDNA processing.
func Normalize(u *url.URL) error {
	hostname := u.Hostname()
	if hostname == "" {
		return ErrInvalidHost
	}
	if !needsIDNA(hostname) {
		return nil
	}
	// A trailing Punycode delimiter represents only ASCII (or an empty label).
	// Reject it even beside a Unicode label, before conversion can erase it.
	for label := range strings.SplitSeq(hostname, ".") {
		if len(label) >= len(punycodePrefix) && strings.EqualFold(label[:len(punycodePrefix)], punycodePrefix) &&
			strings.HasSuffix(label, "-") {
			return ErrInvalidHost
		}
	}
	converted, err := lookupProfile.ToASCII(hostname)
	if err != nil {
		return err
	}
	// An A-label must encode an actual Unicode label. The library accepts an
	// empty Punycode suffix and can erase it; accepting that would rename a host.
	if isASCII(hostname) && !strings.EqualFold(hostname, converted) {
		return ErrInvalidHost
	}
	// Ignored characters can erase the entire domain; compatibility characters
	// can become URL delimiters. Neither may change the URL's authority structure.
	if converted == "" || strings.ContainsFunc(converted, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || strings.ContainsRune(forbiddenDomainCodePoints, r)
	}) {
		return ErrInvalidHost
	}
	if port := u.Port(); port != "" {
		converted += ":" + port
	}
	u.Host = converted
	return nil
}

func isASCII(value string) bool {
	for index := range len(value) {
		if value[index] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func needsIDNA(host string) bool {
	for index := range len(host) {
		if host[index] >= utf8.RuneSelf {
			return true
		}
		if (index == 0 || host[index-1] == '.') &&
			len(host)-index >= len(punycodePrefix) &&
			strings.EqualFold(host[index:index+len(punycodePrefix)], punycodePrefix) {
			return true
		}
	}
	return false
}
