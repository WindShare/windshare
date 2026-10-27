//go:build !linux && !windows && !darwin

package testoutputroot

import (
	"runtime"
	"testing"
)

func newNativePlacement(t testing.TB) string {
	t.Helper()
	t.Fatalf("native durable-output test roots are unsupported on %s", runtime.GOOS)
	return ""
}
