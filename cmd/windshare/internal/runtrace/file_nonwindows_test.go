//go:build !windows

package runtrace

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateOwnerOnlyFilePreservesNonWindowsExclusiveModeContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.ndjson")
	file, err := createOwnerOnlyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("durable trace\n")
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != ownerOnlyFileMode {
		t.Fatalf("trace mode = %o, want %o", got, ownerOnlyFileMode)
	}
	if existing, err := createOwnerOnlyFile(path); !errors.Is(err, fs.ErrExist) || existing != nil {
		t.Fatalf("exclusive reopen = file %v, error %v", existing, err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, payload) {
		t.Fatalf("exclusive collision changed trace = %q, want %q", contents, payload)
	}
}
