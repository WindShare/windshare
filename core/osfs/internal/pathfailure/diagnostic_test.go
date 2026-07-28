package pathfailure

import (
	"errors"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

func TestDiagnosticPreservesCategoriesWithoutLeakingRawPathText(t *testing.T) {
	cause := errors.New("native failure detail")
	category := errors.New("output collision")
	failure := NewCategorized(
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

	uncategorized := NewCategorized("inspect output", "file.bin", nil, cause)
	if errors.Is(uncategorized, category) || !errors.Is(uncategorized, cause) ||
		!strings.Contains(uncategorized.Error(), "operation failed") {
		t.Fatalf("uncategorized failure acquired a false machine category: %v", uncategorized)
	}

	tooLong := Filesystem("open output", "deep/path", syscall.ENAMETOOLONG)
	if !errors.Is(tooLong, ErrTooLong) || !errors.Is(tooLong, syscall.ENAMETOOLONG) {
		t.Fatalf("native path-limit failure lost classification: %v", tooLong)
	}
	denied := Filesystem("open output", "private/path", syscall.EACCES)
	if errors.Is(denied, ErrTooLong) || !errors.Is(denied, syscall.EACCES) {
		t.Fatalf("non-path-limit failure was misclassified: %v", denied)
	}
}

func TestQuoteIsUTF8SafeAndBoundedAfterEscaping(t *testing.T) {
	for _, test := range []struct {
		name          string
		path          string
		wantTruncated bool
	}{
		{name: "control-characters", path: "folder\nfile\t.bin"},
		{name: "long-ascii", path: strings.Repeat("a", maximumDiagnosticBytes*2), wantTruncated: true},
		{
			name: "escaping-expands-past-bound",
			path: strings.Repeat(`"`, maximumDiagnosticBytes-20), wantTruncated: true,
		},
		{
			name: "multibyte-boundary",
			path: strings.Repeat("界", maximumDiagnosticBytes), wantTruncated: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			quoted := Quote(test.path)
			if len(quoted) > maximumDiagnosticBytes {
				t.Fatalf("quoted diagnostic length = %d, limit %d", len(quoted), maximumDiagnosticBytes)
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
