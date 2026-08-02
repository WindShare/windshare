package catalog

import "testing"

func requireCatalogScaleScenario(t *testing.T) {
	t.Helper()
	if !catalogScaleScenariosEnabled(testing.Short()) {
		// These cases intentionally materialize thousands of spill records and
		// durable pages. Ordinary/race/coverage sweeps retain that scale evidence;
		// the short loop keeps the smaller semantic and fault contracts.
		t.Skip("skipping large on-disk catalog scenario in short mode")
	}
}

func catalogScaleScenariosEnabled(short bool) bool {
	return !short
}

func TestCatalogScaleScenarioShortBoundary(t *testing.T) {
	t.Parallel()
	if catalogScaleScenariosEnabled(true) {
		t.Fatal("short mode enabled large on-disk catalog scenarios")
	}
	if !catalogScaleScenariosEnabled(false) {
		t.Fatal("ordinary mode disabled large on-disk catalog scenarios")
	}
}
