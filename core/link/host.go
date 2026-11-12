package link

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

var errForbiddenHostCodePoint = errors.New("forbidden domain code point")

// forbiddenDomainCodePoints are the WHATWG "forbidden domain code points"
// (forbidden host code points plus "%"); C0 controls and DEL are checked
// separately.
const forbiddenDomainCodePoints = " #/:<>?@[\\]^|%"

// whatwgHostProfile is the UTS #46 processing the WHATWG URL parser applies to
// special-scheme hosts (domain to ASCII with beStrict=false).
//
// BidiRule is stricter than some WHATWG implementations: Go rejects hosts such
// as "aא.com" that Node and ada accept. Failing closed here only means the CLI
// refuses a link instead of guessing a relay.
var whatwgHostProfile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.CheckJoiners(true),
	idna.BidiRule(),
	idna.VerifyDNSLength(false),
)

// asciiHost rewrites a Unicode host to the ASCII form a browser resolves it to.
// net/url keeps such a host verbatim and percent-encodes it when serialized, so
// a link pasted in its Unicode spelling would name a relay the browser never
// contacts and the relay origin derived from it could not be reached.
func asciiHost(u *url.URL) error {
	hostname := u.Hostname()
	if isASCII(hostname) {
		return nil
	}
	converted, err := whatwgHostProfile.ToASCII(hostname)
	if err != nil {
		return err
	}
	// Normalization can map non-ASCII look-alikes (fullwidth ":" "/" "@" "?"
	// "#" "%") onto syntax characters; browsers reject the whole host then, so
	// they must not turn into a port, path, or escaped relay origin here.
	if strings.ContainsFunc(converted, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || strings.ContainsRune(forbiddenDomainCodePoints, r)
	}) {
		return errForbiddenHostCodePoint
	}
	if port := u.Port(); port != "" {
		converted = net.JoinHostPort(converted, port)
	}
	u.Host = converted
	return nil
}

func isASCII(s string) bool {
	for index := range len(s) {
		if s[index] >= 0x80 {
			return false
		}
	}
	return true
}
