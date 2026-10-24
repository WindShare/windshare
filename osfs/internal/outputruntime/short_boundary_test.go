package outputruntime

import "testing"

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
