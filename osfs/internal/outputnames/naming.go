// Package outputnames separates source identity from destination spelling.
package outputnames

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
)

const (
	MaximumAttempts       = 16
	tokenBytes            = 16
	nameSuffix            = ".windshare-"
	defaultPrefix         = "entry"
	maximumExtensionBytes = 32
)

var ErrInvalidMapping = errors.New("output name mapping is invalid")

// PortableName governs generated names only. Original names are tried through
// the destination's own namespace, including on filesystems with fewer restrictions.
func PortableName(name string) bool {
	canonical, err := catalog.CanonicalName(name)
	if err != nil || canonical != name || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") ||
		strings.ContainsAny(name, `<>"|?*`) {
		return false
	}
	stem, _, _ := strings.Cut(name, ".")
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	for _, prefix := range []string{"COM", "LPT"} {
		if suffix, ok := strings.CutPrefix(stem, prefix); ok &&
			(len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' ||
				suffix == "¹" || suffix == "²" || suffix == "³") {
			return false
		}
	}
	return true
}

// Candidate keeps a safe extension so a repaired document still opens normally.
// The operation and complete logical path distinguish equal-looking source names.
func Candidate(operationID []byte, logicalPath string, attempt uint32) (string, error) {
	canonical, err := catalog.CanonicalPath(logicalPath)
	if len(operationID) != catalog.IdentityBytes || err != nil || canonical != logicalPath || attempt >= MaximumAttempts {
		return "", ErrInvalidMapping
	}
	parts := strings.Split(logicalPath, "/")
	name := parts[len(parts)-1]
	stem, extension := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		candidate := name[dot:]
		if len(candidate) <= maximumExtensionBytes && PortableName("file"+candidate) {
			stem, extension = name[:dot], candidate
		}
	}
	var readable strings.Builder
	separator := false
	for _, scalar := range stem {
		if unicode.IsLetter(scalar) || unicode.IsNumber(scalar) || unicode.IsMark(scalar) {
			if separator && readable.Len() > 0 {
				readable.WriteByte('-')
			}
			readable.WriteRune(scalar)
			separator = false
		} else {
			separator = true
		}
	}
	prefix := readable.String()
	if prefix == "" {
		prefix = defaultPrefix
	}
	identity := sha256.New()
	_, _ = identity.Write([]byte("windshare/native-compatible-name/v1\x00"))
	_, _ = identity.Write(operationID)
	_, _ = fmt.Fprintf(identity, "\x00%s\x00%d", logicalPath, attempt)
	token := hex.EncodeToString(identity.Sum(nil)[:tokenBytes])
	budget := catalog.MaxNameBytes - len(nameSuffix) - len(token) - len(extension)
	prefix = truncateUTF8(prefix, budget)
	name = prefix + nameSuffix + token + extension
	if !PortableName(name) {
		prefix = truncateUTF8(defaultPrefix+"-"+prefix, budget)
		name = prefix + nameSuffix + token + extension
	}
	if !PortableName(name) {
		return "", ErrInvalidMapping
	}
	return name, nil
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
