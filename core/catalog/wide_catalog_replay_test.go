package catalog

import "testing"

func TestExtremeWidthCatalogSpillAndReplay(t *testing.T) {
	requireCatalogScaleScenario(t)
	const semanticWidth = 10_003
	metrics := exerciseCatalogWidth(t, t.TempDir(), semanticWidth, catalogSortRunBytes)
	if metrics.entries != semanticWidth || metrics.pages < 2 || metrics.sortBytesWritten == 0 {
		t.Fatalf("extreme-width evidence = %+v", metrics)
	}
}
