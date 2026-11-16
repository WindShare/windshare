package networkendpoint

import (
	"fmt"
	"strings"
)

const hexNumberPrefix = "0x"

// NumericHostError preserves the host that WHATWG attempts to parse as IPv4
// rather than DNS. Callers can explain the rejection without parsing
// an error message or exposing the URL's credentials.
type NumericHostError struct {
	Host string
}

func (err *NumericHostError) Error() string {
	return fmt.Sprintf("%s: host %q ends in a number", ErrInvalid, err.Host)
}

func (err *NumericHostError) Unwrap() error { return ErrInvalid }

func validateDNSHost(hostname string) error {
	if !validDNSName(hostname) {
		return fmt.Errorf("%w: invalid DNS host", ErrInvalid)
	}
	if endsInNumber(hostname) {
		return &NumericHostError{Host: hostname}
	}
	return nil
}

func validDNSName(hostname string) bool {
	name := strings.TrimSuffix(hostname, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > 63 || strings.HasPrefix(label, "-") ||
			strings.HasSuffix(label, "-") ||
			len(label) > 3 && label[2:4] == "--" && !strings.HasPrefix(label, "xn--") {
			return false
		}
		for index := range len(label) {
			if !asciiAlphaNumeric(label[index]) && label[index] != '-' {
				return false
			}
		}
	}
	return true
}

// endsInNumber applies the WHATWG URL host rule: a name whose last label is a
// decimal or 0x-prefixed hex number is parsed as IPv4, so browsers and the
// relay's URL parser either rewrite it (0x7f.0.0.1 -> 127.0.0.1) or reject it
// (relay.123). Go would instead resolve it through DNS under an identity no
// other runtime produces. Canonical dotted-quad literals never reach this check.
func endsInNumber(hostname string) bool {
	name := strings.TrimSuffix(hostname, ".")
	last := name[strings.LastIndexByte(name, '.')+1:]
	if last == "" {
		return false
	}
	digits := last
	if len(last) >= len(hexNumberPrefix) && strings.EqualFold(last[:len(hexNumberPrefix)], hexNumberPrefix) {
		digits = last[len(hexNumberPrefix):]
		for index := range len(digits) {
			if !asciiHex(digits[index]) {
				return false
			}
		}
		return true
	}
	for index := range len(digits) {
		if digits[index] < '0' || digits[index] > '9' {
			return false
		}
	}
	return true
}
