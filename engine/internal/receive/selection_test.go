package receive

import (
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"testing"
)

func TestSelectionRulesKeepWholeShareAndPathIntentDistinct(t *testing.T) {
	wholeShare, err := selectionRules(nil)
	if err != nil || wholeShare.Mode() != transfer.SelectionByNodeID || !wholeShare.DefaultSelected() {
		t.Fatalf("whole-share rules mode=%d default=%v error=%v", wholeShare.Mode(), wholeShare.DefaultSelected(), err)
	}
	paths, err := selectionRules([]string{"tree/b.txt", "tree/a.txt"})
	if err != nil || paths.Mode() != transfer.SelectionByCatalogPath || paths.DefaultSelected() ||
		!paths.FileSelectedAt(catalog.FileID{}, "tree/a.txt", false) ||
		paths.FileSelectedAt(catalog.FileID{}, "tree/c.txt", false) {
		t.Fatalf("path rules mode=%d default=%v error=%v", paths.Mode(), paths.DefaultSelected(), err)
	}
	if _, err := selectionRules([]string{"../escape"}); err == nil {
		t.Fatal("non-canonical path selection was accepted")
	}
}
