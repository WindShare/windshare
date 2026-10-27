//go:build darwin

package testoutputroot

import "testing"

func newNativePlacement(t testing.TB) string {
	t.Helper()
	return t.TempDir()
}
