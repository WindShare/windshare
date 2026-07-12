package osfs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/windshare/windshare/core/manifest"
)

// Stable categories let callers react without parsing platform-dependent path
// errors. Diagnostic wrappers below keep those categories and native causes
// machine-readable without rendering untrusted error text.
var (
	ErrDrift            = errors.New("osfs: source changed; create a new share")
	ErrNotInSnapshot    = errors.New("osfs: path is not in the snapshot")
	ErrOutOfRange       = errors.New("osfs: byte range is out of bounds")
	ErrPathEscape       = errors.New("osfs: path escapes the output root")
	ErrPathTooLong      = errors.New("osfs: output path exceeds the platform limit")
	ErrAlreadyExists    = errors.New("osfs: output file already exists")
	ErrOwnedFileMissing = errors.New("osfs: an output owned by this transaction is missing")
	ErrOwnershipRecord  = errors.New("osfs: could not persist output ownership")
)

// pathDiagnosticError separates the machine-readable cause chain from the
// terminal-facing message. OS errors commonly retain the complete path and may
// also carry implementation-specific sensitive text; interpolating them with
// %w would defeat the bounded path renderer at the next wrapping layer.
type pathDiagnosticError struct {
	operation string
	path      string
	category  error
	cause     error
}

func (e *pathDiagnosticError) Error() string {
	detail := "operation failed"
	if e.category != nil {
		detail = strings.TrimPrefix(e.category.Error(), "osfs: ")
	}
	return fmt.Sprintf("%s %s: %s", e.operation, manifest.QuotePathForDiagnostic(e.path), detail)
}

func (e *pathDiagnosticError) Unwrap() error { return e.cause }

func (e *pathDiagnosticError) Is(target error) bool {
	return e.category != nil && errors.Is(e.category, target)
}

func pathFailure(operation, path string, cause error) error {
	return &pathDiagnosticError{operation: operation, path: path, cause: cause}
}

func categorizedPathFailure(operation, path string, category, cause error) error {
	return &pathDiagnosticError{operation: operation, path: path, category: category, cause: cause}
}

func filesystemPathFailure(operation, path string, cause error) error {
	var category error
	if isPathTooLongError(cause) {
		category = ErrPathTooLong
	}
	return categorizedPathFailure(operation, path, category, cause)
}
