//go:build !linux && !windows

package testoutputroot

import (
	"runtime"
	"testing"
)

func newCertifiedPlacement(t testing.TB) string {
	t.Helper()
	t.Fatalf("certified durable-output test roots are unsupported on %s", runtime.GOOS)
	return ""
}
