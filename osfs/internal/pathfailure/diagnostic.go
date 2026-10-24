package pathfailure

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maximumDiagnosticBytes = 256

// ErrTooLong is projected by osfs as its public path-limit sentinel. Keeping
// one error value across the internal diagnostic and public boundary preserves
// errors.Is identity without coupling this deep module back to its consumer.
var ErrTooLong = errors.New("osfs: output path exceeds the platform limit")

// DiagnosticError keeps native path details out of the machine-readable cause
// chain while preserving a bounded, escaped location for operators.
type DiagnosticError struct {
	operation string
	path      string
	category  error
	cause     error
}

func (failure *DiagnosticError) Error() string {
	detail := "operation failed"
	if failure.category != nil {
		detail = strings.TrimPrefix(failure.category.Error(), "osfs: ")
	}
	return fmt.Sprintf("%s %s: %s", failure.operation, Quote(failure.path), detail)
}

func (failure *DiagnosticError) Unwrap() error { return failure.cause }

func (failure *DiagnosticError) Is(target error) bool {
	return failure.category != nil && errors.Is(failure.category, target)
}

func NewCategorized(operation, path string, category, cause error) error {
	return &DiagnosticError{operation: operation, path: path, category: category, cause: cause}
}

func Filesystem(operation, path string, cause error) error {
	var category error
	if IsTooLong(cause) {
		category = ErrTooLong
	}
	return NewCategorized(operation, path, category, cause)
}

func Quote(path string) string {
	end := min(len(path), maximumDiagnosticBytes)
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
		if len(quoted) <= maximumDiagnosticBytes {
			return quoted
		}
		truncated = true
		end--
		for end > 0 && !utf8.RuneStart(path[end]) {
			end--
		}
	}
}
