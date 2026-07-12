package manifest

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	// PathPolicyVersion changes whenever canonicalization, folding, or the
	// reserved-name set changes. Receivers in other languages use this value to
	// select matching tables rather than approximating Go's behavior.
	PathPolicyVersion = "windshare/path/v1-unicode-15.0.0"

	// PathPolicyUnicodeVersion deliberately pins both x/text normalization and
	// full case folding. Tests compare it with the linked tables so a dependency
	// upgrade cannot silently change the wire-visible collision policy.
	PathPolicyUnicodeVersion = "15.0.0"

	windowsIllegalPathChars = "<>:\"|?*\\"
	resumeJournalPrefix     = ".wsresume"
	maxPathDiagnosticBytes  = 256
)

// PathPolicy is the immutable, versioned path contract shared by manifest
// construction, validation, filesystem adapters, and independent clients.
// It has no configurable knobs because two peers must never disagree about
// whether a manifest can be materialized safely.
type PathPolicy struct{}

// CurrentPathPolicy returns the only policy understood by this pre-v1 build.
// Returning a value keeps callers from mutating process-global policy state.
func CurrentPathPolicy() PathPolicy { return PathPolicy{} }

func (PathPolicy) Version() string        { return PathPolicyVersion }
func (PathPolicy) UnicodeVersion() string { return PathPolicyUnicodeVersion }

var windowsReservedNames = func() map[string]struct{} {
	names := []string{"con", "prn", "aux", "nul", "conin$", "conout$"}
	for i := 1; i <= 9; i++ {
		names = append(names, fmt.Sprintf("com%d", i), fmt.Sprintf("lpt%d", i))
	}
	// Win32 treats superscript one, two, and three as device-number aliases,
	// even though they are not ASCII digits.
	for _, suffix := range []string{"¹", "²", "³"} {
		names = append(names, "com"+suffix, "lpt"+suffix)
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}()

// Canonicalize performs the sole permitted transformation, NFC normalization.
// Every other unsafe or ambiguous form is rejected so the manifest never
// claims a name different from the object selected by the sender.
func (p PathPolicy) Canonicalize(path string) (string, error) {
	if !utf8.ValidString(path) {
		return "", fmt.Errorf("%w: path contains invalid UTF-8", ErrInvalidPath)
	}
	canonical := norm.NFC.String(path)
	if err := p.Validate(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

// Validate requires an already canonical manifest path. Sender and receiver
// deliberately share this exact function: construction bugs and hostile
// manifests must reach the same rejection boundary.
func (PathPolicy) Validate(path string) error {
	if path == "" {
		return fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%w: path contains invalid UTF-8", ErrInvalidPath)
	}
	if !norm.NFC.IsNormalString(path) {
		return fmt.Errorf("%w: %s is not NFC-normalized", ErrInvalidPath, QuotePathForDiagnostic(path))
	}

	firstSegment := ""
	for start := 0; ; {
		end := len(path)
		more := false
		if slash := strings.IndexByte(path[start:], '/'); slash >= 0 {
			end = start + slash
			more = true
		}
		segment := path[start:end]
		if err := validateSegment(segment); err != nil {
			return fmt.Errorf("%w (path %s)", err, QuotePathForDiagnostic(path))
		}
		if start == 0 {
			firstSegment = segment
		}
		if !more {
			break
		}
		start = end + 1
	}

	// Journals live directly under the output root. Folding the first segment
	// closes case-insensitive aliases without needlessly reserving the same name
	// in nested directories that cannot collide with the journal.
	if strings.HasPrefix(pathCollisionKey(firstSegment), resumeJournalPrefix) {
		return fmt.Errorf("%w: %s uses reserved prefix %q", ErrInvalidPath, QuotePathForDiagnostic(path), resumeJournalPrefix)
	}
	return nil
}

// CollisionKey returns the stable cross-platform identity used to reject
// names that would alias on normalization- or case-insensitive filesystems.
func (p PathPolicy) CollisionKey(path string) (string, error) {
	if err := p.Validate(path); err != nil {
		return "", err
	}
	return pathCollisionKey(path), nil
}

// CanonicalPath and ValidatePath keep the concise domain API used throughout
// the core while routing every operation through the versioned policy.
func CanonicalPath(path string) (string, error) {
	return CurrentPathPolicy().Canonicalize(path)
}

func ValidatePath(path string) error {
	return CurrentPathPolicy().Validate(path)
}

func validateSegment(segment string) error {
	switch segment {
	case "":
		return fmt.Errorf("%w: empty path segment", ErrInvalidPath)
	case ".", "..":
		return fmt.Errorf("%w: relative segment %s", ErrInvalidPath, QuotePathForDiagnostic(segment))
	}

	for _, r := range segment {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return fmt.Errorf("%w: segment %s contains control or format character %U", ErrInvalidPath, QuotePathForDiagnostic(segment), r)
		}
		if strings.ContainsRune(windowsIllegalPathChars, r) {
			return fmt.Errorf("%w: segment %s contains illegal character %q", ErrInvalidPath, QuotePathForDiagnostic(segment), r)
		}
	}
	if strings.HasSuffix(segment, " ") || strings.HasSuffix(segment, ".") {
		return fmt.Errorf("%w: segment %s ends in a space or dot", ErrInvalidPath, QuotePathForDiagnostic(segment))
	}
	if isWindowsReservedName(segment) {
		return fmt.Errorf("%w: segment %s is a reserved Windows device name", ErrInvalidPath, QuotePathForDiagnostic(segment))
	}
	return nil
}

// QuotePathForDiagnostic keeps hostile authenticated metadata from becoming a
// second unbounded allocation or terminal write while retaining enough prefix
// context to locate an ordinary path error. Filesystem adapters use the same
// renderer so a rejected manifest path remains bounded through every wrapping
// layer.
func QuotePathForDiagnostic(path string) string {
	end := min(len(path), maxPathDiagnosticBytes)
	for end < len(path) && end > 0 && !utf8.RuneStart(path[end]) {
		end--
	}
	truncated := end < len(path)
	for {
		candidate := path[:end]
		if truncated {
			candidate += "…"
		}
		quoted := strconv.Quote(candidate)
		if len(quoted) <= maxPathDiagnosticBytes {
			return quoted
		}
		// Quoting control or invalid bytes can expand one input byte fourfold.
		// Reduce the prefix against the rendered budget so the renderer itself,
		// not merely its input slice, remains a hard diagnostic boundary.
		truncated = true
		if end == 0 {
			return strconv.Quote("…")
		}
		end--
		for end > 0 && !utf8.RuneStart(path[end]) {
			end--
		}
	}
}

// Windows resolves device names from the stem before the first dot and after
// trimming trailing spaces. Applying the OS rule here prevents a safe-looking
// extension from bypassing the policy.
func isWindowsReservedName(segment string) bool {
	stem := segment
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.TrimRight(stem, " ")
	_, reserved := windowsReservedNames[strings.ToLower(stem)]
	return reserved
}

func pathCollisionKey(path string) string {
	return norm.NFC.String(cases.Fold().String(path))
}

// foldPath is intentionally kept package-private for Manifest.Validate, which
// has already validated each path before computing collection-level aliases.
func foldPath(path string) string { return pathCollisionKey(path) }
