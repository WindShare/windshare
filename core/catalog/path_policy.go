package catalog

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/windshare/windshare/core/internal/unicode15"
)

const (
	// This is a metadata budget, not a filesystem component limit. It leaves
	// room for UTF-16 native names and NFC expansion without unbounded records.
	MaxNameBytes             = 4 << 10
	MaxPathBytes             = 32 * 1024
	MaxPathDepth             = 256
	reservedOutputRootPrefix = ".wsresume"
)

const PathPolicy = "windshare/path/v3-unicode-" + unicode15.Version

var (
	ErrInvalidName = errors.New("invalid catalog name")
	ErrInvalidPath = errors.New("invalid catalog path")
	ErrPathTooDeep = errors.New("catalog path exceeds maximum depth")
)

// General-category tables advance with the Go toolchain. Freezing the Unicode
// 15 Cf ranges keeps validation aligned with the versioned path identity.
var formatCharacterRanges = [...]struct {
	first rune
	last  rune
}{
	{0x00ad, 0x00ad},
	{0x0600, 0x0605},
	{0x061c, 0x061c},
	{0x06dd, 0x06dd},
	{0x070f, 0x070f},
	{0x0890, 0x0891},
	{0x08e2, 0x08e2},
	{0x180e, 0x180e},
	{0x200b, 0x200f},
	{0x202a, 0x202e},
	{0x2060, 0x2064},
	{0x2066, 0x206f},
	{0xfeff, 0xfeff},
	{0xfff9, 0xfffb},
	{0x110bd, 0x110bd},
	{0x110cd, 0x110cd},
	{0x13430, 0x1343f},
	{0x1bca0, 0x1bca3},
	{0x1d173, 0x1d17a},
	{0xe0001, 0xe0001},
	{0xe0020, 0xe007f},
}

// CanonicalName normalizes Unicode before validation so equality and ordering are
// stable across filesystems that expose different normalization forms.
// Destination naming restrictions belong to materialization; rejecting a readable
// source name here would also prevent lossless archives and compatible-name repair.
func CanonicalName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidName)
	}
	canonical := unicode15.NormalizeNFC(name)
	if canonical == "" || canonical == "." || canonical == ".." {
		return "", fmt.Errorf("%w: empty or relative component", ErrInvalidName)
	}
	if len(canonical) > MaxNameBytes {
		return "", fmt.Errorf("%w: NFC name is %d UTF-8 bytes; limit is %d", ErrInvalidName, len(canonical), MaxNameBytes)
	}
	if strings.ContainsAny(canonical, "/\\") {
		return "", fmt.Errorf("%w: slash and backslash cannot occur inside a path component", ErrInvalidName)
	}
	for _, r := range canonical {
		if isControlOrFormatCharacter(r) {
			return "", fmt.Errorf("%w: unsupported control or format character %U", ErrInvalidName, r)
		}
	}
	return canonical, nil
}

func CanonicalPath(path string) (string, error) {
	if !utf8.ValidString(path) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrInvalidPath)
	}
	canonical := unicode15.NormalizeNFC(path)
	if canonical == "" || !utf8.ValidString(canonical) || len(canonical) > MaxPathBytes {
		return "", fmt.Errorf("%w: empty, invalid UTF-8, or too long", ErrInvalidPath)
	}
	if strings.ContainsRune(canonical, '\\') || strings.HasPrefix(canonical, "/") || strings.HasSuffix(canonical, "/") {
		return "", fmt.Errorf("%w: path must be relative and slash-separated", ErrInvalidPath)
	}
	components := strings.Split(canonical, "/")
	if len(components) > MaxPathDepth {
		return "", fmt.Errorf("%w: got %d components", ErrPathTooDeep, len(components))
	}
	for index, component := range components {
		validated, err := CanonicalName(component)
		if err != nil {
			return "", fmt.Errorf("%w: component %d: %w", ErrInvalidPath, index, err)
		}
		components[index] = validated
	}
	if reservedOutputRootName(components[0]) {
		return "", fmt.Errorf("%w: root component uses the reserved %q prefix", ErrInvalidPath, reservedOutputRootPrefix)
	}
	return strings.Join(components, "/"), nil
}

// SiblingNameKey returns the key under which sibling names collide: two names
// cannot coexist in one directory listing when their keys are equal, because
// receivers may compare them case- or form-insensitively. Computing it once per
// name lets callers detect collisions with a map instead of pairwise folds.
func SiblingNameKey(name string) string {
	return siblingCollisionKey(name)
}

func siblingCollisionKey(name string) string {
	return unicode15.FoldPortableName(name)
}

func reservedOutputRootName(name string) bool {
	return strings.HasPrefix(siblingCollisionKey(name), reservedOutputRootPrefix)
}

func isControlOrFormatCharacter(scalar rune) bool {
	// Joining controls carry ordinary language and emoji semantics; soft hyphen
	// is also textual content, not a path operation. Never strip these scalars.
	const softHyphen, zeroWidthNonJoiner, zeroWidthJoiner = '\u00ad', '\u200c', '\u200d'
	if scalar == softHyphen || scalar == zeroWidthNonJoiner || scalar == zeroWidthJoiner {
		return false
	}
	if scalar <= 0x1f || scalar >= 0x7f && scalar <= 0x9f {
		return true
	}
	low, high := 0, len(formatCharacterRanges)
	for low < high {
		middle := low + (high-low)/2
		candidate := formatCharacterRanges[middle]
		switch {
		case scalar < candidate.first:
			high = middle
		case scalar > candidate.last:
			low = middle + 1
		default:
			return true
		}
	}
	return false
}
