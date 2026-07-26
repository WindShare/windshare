//go:build linux

package testoutputroot

import (
	"os"
	"testing"
)

func newCertifiedPlacement(t testing.TB) string {
	t.Helper()
	placement := t.TempDir()
	// The production root creator certifies the nearest existing ancestor. Make
	// that test-owned authority private instead of relying on host umask policy.
	if err := os.Chmod(placement, 0o700); err != nil {
		t.Fatalf("protect Linux durable-output placement: %v", err)
	}
	return placement
}
