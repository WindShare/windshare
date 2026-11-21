//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestReadInstalledMirrorRejectsFIFOAndLeafSymlink(t *testing.T) {
	config, vector, _ := loadMirrorFixtures(t)
	dir := t.TempDir()

	fifo := filepath.Join(dir, "windshare-network.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstalledMirror(config, fifo, ""); !errors.Is(err, errNonRegularFile) {
		t.Fatalf("FIFO mirror err = %v, want %v", err, errNonRegularFile)
	}

	target := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(target, vector["valid"], 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readInstalledMirror(config, link, ""); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("symlink mirror err = %v, want ELOOP", err)
	}
}
