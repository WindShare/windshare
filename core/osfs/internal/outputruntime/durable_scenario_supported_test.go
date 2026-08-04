//go:build linux || windows

package outputruntime

import "testing"

func requireDurableFilesystemScenario(t testing.TB) {
	t.Helper()
	if !durableFilesystemScenariosEnabled(testing.Short()) {
		// Real native certification, sync, and placement guards are owned by
		// stable TestLong entrypoints. Daily short coverage uses the portable
		// capability model so runtime policy is not coupled to disk latency.
		t.Skip("skipping named native durability scenario in short mode")
	}
}
