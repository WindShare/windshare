//go:build linux

package osfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func assertRemountRetirement(t *testing.T, root, result string, delivered bool) {
	t.Helper()
	for _, directory := range []string{root, result} {
		actual, err := os.ReadFile(filepath.Join(directory, "ambient-file"))
		if err != nil || string(actual) != "unrelated user data" {
			t.Fatalf("retirement changed unrelated data in %s: %q %v", directory, actual, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".windshare-output")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("terminal operation retained its private namespace: %v", err)
	}
	entries, err := os.ReadDir(result)
	expected := 1
	if delivered {
		expected++
	}
	if err != nil || len(entries) != expected {
		t.Fatalf("terminal result retained private identity files: %v %v", entries, err)
	}
	if delivered {
		actual, err := os.ReadFile(filepath.Join(result, "file.bin"))
		if err != nil || string(actual) != "datadone" {
			t.Fatalf("retirement changed delivered data: %q %v", actual, err)
		}
	}
}
