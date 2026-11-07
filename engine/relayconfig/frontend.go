package relayconfig

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

var ErrFrontURL = errors.New("invalid frontend URL")

const maximumPort = 65535

// Reject literal whitespace rather than rewriting the saved address: capability
// construction trims some copied characters and encodes others into the path.
// Explicitly escaped path characters remain part of the configured address.
func ValidateFrontURL(raw string) error {
	if strings.IndexFunc(raw, forbiddenFrontURLRune) >= 0 {
		return fmt.Errorf("%w: contains whitespace or invisible spacing; remove it from the address", ErrFrontURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		strings.ContainsAny(raw, `#\`) || strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("%w: expected an absolute HTTP(S) base URL without credentials, query or fragment", ErrFrontURL)
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 || value > maximumPort {
			return fmt.Errorf("%w: port must be between 1 and %d", ErrFrontURL, maximumPort)
		}
	}
	return nil
}

func forbiddenFrontURLRune(r rune) bool {
	// Copied-link parsing trims BOM even though Unicode does not classify it as whitespace.
	return unicode.IsSpace(r) || r == '\ufeff'
}
