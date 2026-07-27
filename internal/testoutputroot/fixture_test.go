//go:build linux || windows

package testoutputroot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestNewReturnsAbsentProductionCreationContract(t *testing.T) {
	fixture := New(t)
	if !fixture.CreateRoot {
		t.Fatal("fixture did not require production root creation")
	}
	if filepath.Base(fixture.RootPath) != outputDirectoryName {
		t.Fatalf("fixture output leaf = %q", filepath.Base(fixture.RootPath))
	}
	if _, err := os.Lstat(fixture.RootPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("fixture output root exists before production creation: %v", err)
	}
	placement, err := os.Stat(filepath.Dir(fixture.RootPath))
	if err != nil || !placement.IsDir() {
		t.Fatalf("fixture placement = (%v, %v), want directory", placement, err)
	}
}
