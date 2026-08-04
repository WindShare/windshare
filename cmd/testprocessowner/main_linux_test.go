//go:build linux

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

const ownerEndpointSuffix = "fd"

func duplicateInheritedEndpoint(source *os.File) (uintptr, error) {
	descriptor, err := unix.Dup(int(source.Fd()))
	runtime.KeepAlive(source)
	if err != nil {
		return 0, err
	}
	// Returning the raw descriptor makes runPlatform the first and only
	// os.File owner, just as it is for a descriptor inherited through exec.
	return uintptr(descriptor), nil
}

func ownedTargetCommand(t *testing.T) (string, []string) {
	t.Helper()
	executable, err := filepath.Abs("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	return executable, []string{"-c", "exit 0"}
}
