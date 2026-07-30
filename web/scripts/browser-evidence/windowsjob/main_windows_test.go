//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommandSuperviseHonorsFreshStatusPreflight(t *testing.T) {
	t.Parallel()
	statusPath := filepath.Join(t.TempDir(), "status.json")
	requestPath := filepath.Join(t.TempDir(), "request.bin")
	controlPath := filepath.Join(t.TempDir(), "control.bin")
	const sentinel = "preexisting-status"
	if err := os.WriteFile(statusPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	var framed bytes.Buffer
	if err := writeCanonicalFrame(&framed, validStartRequest(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestPath, framed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	err := runCommand(
		[]string{
			commandSupervise,
			"--status", statusPath,
			"--request", requestPath,
			"--control", controlPath,
		},
		bytes.NewReader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("runCommand preflight error = %v", err)
	}
	encoded, readErr := os.ReadFile(statusPath)
	if readErr != nil || string(encoded) != sentinel {
		t.Fatalf("preexisting status changed: %q, %v", encoded, readErr)
	}
}
