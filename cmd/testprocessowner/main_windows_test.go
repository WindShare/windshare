//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

const ownerEndpointSuffix = "handle"

func ownedTargetCommand(t *testing.T) (string, []string) {
	t.Helper()
	executable, err := filepath.Abs(os.Getenv("ComSpec"))
	if err != nil {
		t.Fatal(err)
	}
	return executable, []string{"/d", "/s", "/c", "exit 0"}
}
