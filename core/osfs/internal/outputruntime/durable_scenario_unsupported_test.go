//go:build !linux && !windows

package outputruntime

import (
	"runtime"
	"testing"
)

func requireDurableFilesystemScenario(t testing.TB) {
	t.Helper()
	t.Skipf("native durability scenarios are unsupported on %s", runtime.GOOS)
}
