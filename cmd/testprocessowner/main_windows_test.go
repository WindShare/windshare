//go:build windows

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/windows"
)

const ownerEndpointSuffix = "handle"

func duplicateInheritedEndpoint(source *os.File) (uintptr, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		process,
		windows.Handle(source.Fd()),
		process,
		&duplicate,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	)
	runtime.KeepAlive(source)
	if err != nil {
		return 0, err
	}
	// Returning the raw handle makes runPlatform the first and only os.File
	// owner, matching the independent handle created in an executed child.
	return uintptr(duplicate), nil
}

func ownedTargetCommand(t *testing.T) (string, []string) {
	t.Helper()
	executable, err := filepath.Abs(os.Getenv("ComSpec"))
	if err != nil {
		t.Fatal(err)
	}
	return executable, []string{"/d", "/s", "/c", "exit 0"}
}
