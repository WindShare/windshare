package osfs

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

func TestOutputPathDiagnosticPreservesCategoriesWithoutLeakingRawPathText(t *testing.T) {
	cause := errors.New("native failure detail")
	category := errors.New("output collision")
	failure := categorizedPathFailure(
		"create output", "private\nname", category, cause,
	)
	if !errors.Is(failure, category) || !errors.Is(failure, cause) {
		t.Fatalf("categorized failure lost category or cause: %v", failure)
	}
	if strings.Contains(failure.Error(), "\n") ||
		!strings.Contains(failure.Error(), `"private\nname"`) ||
		!strings.Contains(failure.Error(), "output collision") {
		t.Fatalf("categorized diagnostic is not bounded escaped text: %q", failure.Error())
	}

	uncategorized := categorizedPathFailure("inspect output", "file.bin", nil, cause)
	if errors.Is(uncategorized, category) || !errors.Is(uncategorized, cause) ||
		!strings.Contains(uncategorized.Error(), "operation failed") {
		t.Fatalf("uncategorized failure acquired a false machine category: %v", uncategorized)
	}

	tooLong := filesystemPathFailure("open output", "deep/path", syscall.ENAMETOOLONG)
	if !errors.Is(tooLong, ErrPathTooLong) || !errors.Is(tooLong, syscall.ENAMETOOLONG) {
		t.Fatalf("native path-limit failure lost classification: %v", tooLong)
	}
	denied := filesystemPathFailure("open output", "private/path", syscall.EACCES)
	if errors.Is(denied, ErrPathTooLong) || !errors.Is(denied, syscall.EACCES) {
		t.Fatalf("non-path-limit failure was misclassified: %v", denied)
	}
}

func TestOutputPathDiagnosticIsUTF8SafeAndBoundedAfterEscaping(t *testing.T) {
	for _, test := range []struct {
		name          string
		path          string
		wantTruncated bool
	}{
		{name: "control-characters", path: "folder\nfile\t.bin"},
		{name: "long-ascii", path: strings.Repeat("a", maximumPathDiagnosticBytes*2), wantTruncated: true},
		{
			name: "escaping-expands-past-bound",
			path: strings.Repeat(`"`, maximumPathDiagnosticBytes-20), wantTruncated: true,
		},
		{
			name: "multibyte-boundary",
			path: strings.Repeat("界", maximumPathDiagnosticBytes), wantTruncated: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			quoted := quotePathForDiagnostic(test.path)
			if len(quoted) > maximumPathDiagnosticBytes {
				t.Fatalf("quoted diagnostic length = %d, limit %d", len(quoted), maximumPathDiagnosticBytes)
			}
			decoded, err := strconv.Unquote(quoted)
			if err != nil || !utf8.ValidString(decoded) {
				t.Fatalf("quoted diagnostic = %q, unquote error = %v", quoted, err)
			}
			truncated := strings.HasSuffix(decoded, "…")
			if truncated != test.wantTruncated {
				t.Fatalf("diagnostic truncated=%t, want %t: %q", truncated, test.wantTruncated, decoded)
			}
			if strings.ContainsAny(quoted, "\n\r\t") {
				t.Fatalf("diagnostic contains raw control text: %q", quoted)
			}
		})
	}
}
