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

func durableFilesystemScenariosEnabled(short bool) bool {
	return !short
}

func TestDurableFilesystemScenarioShortBoundary(t *testing.T) {
	t.Parallel()
	if durableFilesystemScenariosEnabled(true) {
		t.Fatal("short mode enabled native durable-filesystem scenarios")
	}
	if !durableFilesystemScenariosEnabled(false) {
		t.Fatal("ordinary mode disabled native durable-filesystem scenarios")
	}
}
