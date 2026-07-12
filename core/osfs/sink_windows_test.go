package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const windowsMaxFilenameCodeUnits = 255

func TestSinkPathLimitBoundary(t *testing.T) {
	requestedRoot := filepath.Join(t.TempDir(), "\U0001F600", "out")
	s, err := NewSink(requestedRoot, SinkOptions{})
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	root := s.rootPath
	rootCodeUnits := len(utf16.Encode([]rune(root)))
	// The receiver enforces Win32's UTF-16 limit, so the fixture must use the
	// same unit. A surrogate-pair root keeps this regression independent of the
	// host's temporary-directory naming.
	pad := windowsMaxPath - 1 - rootCodeUnits - 1 // -1 NUL, -1 path separator
	if pad <= 0 || pad > windowsMaxFilenameCodeUnits {
		t.Skipf("temporary root uses %d UTF-16 code units; cannot construct a single-component boundary path", rootCodeUnits)
	}
	ok := strings.Repeat("a", pad)
	if err := s.WriteRange(ok, 0, []byte("x")); err != nil {
		t.Fatalf("path at the MAX_PATH boundary should be allowed: %v", err)
	}
	over := strings.Repeat("b", pad+1)
	if err := s.WriteRange(over, 0, []byte("x")); !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("path above the MAX_PATH boundary: err = %v, want ErrPathTooLong", err)
	}
}

func TestSinkRootCapabilityRejectsJunctionEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	mustWriteFile(t, filepath.Join(outside, "secret.txt"), "secret")
	root := filepath.Join(dir, "out")
	s, err := NewSink(root, SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	junction := filepath.Join(root, "escape")
	makeJunction(t, junction, outside)

	if err := s.EnsureDir("escape/newdir"); err == nil {
		t.Fatal("EnsureDir followed a junction outside the root")
	}
	if err := s.WriteRange("escape/new.txt", 0, []byte("leak")); err == nil {
		t.Fatal("WriteRange followed a junction outside the root")
	}
	if err := s.SetMTime("escape/secret.txt", time.Now().UnixMilli()); err == nil {
		t.Fatal("SetMTime followed a junction outside the root")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside file created through junction: %v", err)
	}
}

func TestOSRootRejectsFinalJunctionWithTrailingSeparator(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(dir, "root")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, filepath.Join(rootPath, "escape"), outside)
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, name := range []string{"escape/", `escape\`} {
		t.Run(name, func(t *testing.T) {
			opened, err := root.Open(name)
			if err == nil {
				_ = opened.Close()
				t.Fatal("os.Root followed a final junction with a trailing separator outside its root")
			}
		})
	}
}

func TestWindowsPathLimitCountsUTF16CodeUnits(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"exact boundary with surrogate pair", strings.Repeat("a", windowsMaxPath-3) + "\U0001F600", false},
		{"one code unit over with surrogate pair", strings.Repeat("a", windowsMaxPath-2) + "\U0001F600", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exceedsPathLimit(tc.path); got != tc.want {
				t.Fatalf("exceedsPathLimit() = %t, want %t", got, tc.want)
			}
		})
	}
}
