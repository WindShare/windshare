//go:build !windows

package osfs

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

func denyCatalogRead(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, info.Mode().Perm()); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if probe, err := os.Open(path); err == nil {
		_ = probe.Close()
		t.Skip("current privileges bypass read permissions")
	} else if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("read-denial fixture: %v", err)
	}
}
