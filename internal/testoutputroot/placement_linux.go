//go:build linux

package testoutputroot

import (
	"fmt"
	"os"
	"testing"
)

const linuxPlacementPattern = ".windshare-output-test-*"

type linuxPlacementHost struct {
	homeDirectory   func() (string, error)
	createDirectory func(string, string) (string, error)
	protect         func(string, os.FileMode) error
}

func newCertifiedPlacement(t testing.TB) string {
	t.Helper()
	placement, err := provisionLinuxPlacement(linuxPlacementHost{
		homeDirectory:   os.UserHomeDir,
		createDirectory: os.MkdirTemp,
		protect:         os.Chmod,
	})
	if placement != "" {
		t.Cleanup(func() {
			if cleanupErr := os.RemoveAll(placement); cleanupErr != nil {
				t.Errorf("remove Linux durable-output placement: %v", cleanupErr)
			}
		})
	}
	if err != nil {
		t.Fatalf("prepare Linux durable-output placement: %v", err)
	}
	return placement
}

func provisionLinuxPlacement(host linuxPlacementHost) (string, error) {
	home, err := host.homeDirectory()
	if err != nil {
		return "", fmt.Errorf("resolve receiver home: %w", err)
	}
	placement, err := host.createDirectory(home, linuxPlacementPattern)
	if err != nil {
		return "", fmt.Errorf("create receiver-owned placement: %w", err)
	}
	// A home child avoids the shared rename authority of /tmp while preserving
	// the complete production ancestry check exercised by the caller.
	if err := host.protect(placement, 0o700); err != nil {
		return placement, fmt.Errorf("protect receiver-owned placement: %w", err)
	}
	return placement, nil
}
