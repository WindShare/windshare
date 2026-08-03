//go:build linux || windows

package outputruntime

import "testing"

func requireDurableFilesystemScenario(t testing.TB) {
	t.Helper()
	if !durableFilesystemScenariosEnabled(testing.Short()) {
		// These fixtures exercise the native durable-output backend and exhaustive
		// sync, recovery, and namespace fault cuts. Their real disk work belongs to
		// ordinary/race/coverage sweeps, not the sub-minute developer loop.
		t.Skip("skipping native durable-filesystem scenario in short mode")
	}
}
