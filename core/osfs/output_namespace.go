package osfs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maximumPathDiagnosticBytes = 256
	dirPerm                    = 0o755
	filePerm                   = 0o644
)

var (
	ErrOutOfRange  = errors.New("osfs: byte range is out of bounds")
	ErrPathEscape  = errors.New("osfs: path escapes the output root")
	ErrPathTooLong = errors.New("osfs: output path exceeds the platform limit")
)

// pathDiagnosticError keeps native path details out of the machine-readable
// cause chain while preserving a bounded, escaped location for operators.
type pathDiagnosticError struct {
	operation string
	path      string
	category  error
	cause     error
}

func (failure *pathDiagnosticError) Error() string {
	detail := "operation failed"
	if failure.category != nil {
		detail = strings.TrimPrefix(failure.category.Error(), "osfs: ")
	}
	return fmt.Sprintf("%s %s: %s", failure.operation, quotePathForDiagnostic(failure.path), detail)
}

func (failure *pathDiagnosticError) Unwrap() error { return failure.cause }

func (failure *pathDiagnosticError) Is(target error) bool {
	return failure.category != nil && errors.Is(failure.category, target)
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

func quotePathForDiagnostic(path string) string {
	end := min(len(path), maximumPathDiagnosticBytes)
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
		if len(quoted) <= maximumPathDiagnosticBytes {
			return quoted
		}
		truncated = true
		end--
		for end > 0 && !utf8.RuneStart(path[end]) {
			end--
		}
	}
}
