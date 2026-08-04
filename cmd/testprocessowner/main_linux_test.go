//go:build linux

package main

import (
	"path/filepath"
	"testing"
)

const ownerEndpointSuffix = "fd"

func ownedTargetCommand(t *testing.T) (string, []string) {
	t.Helper()
	executable, err := filepath.Abs("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	return executable, []string{"-c", "exit 0"}
}
