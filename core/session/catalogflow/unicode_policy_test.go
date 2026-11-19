package catalogflow

import (
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestCatalogPagePreservesPostUnicode15Name(t *testing.T) {
	name := "\U00016D63\U00016D67.txt"
	canonical, err := catalog.CanonicalName(name)
	if err != nil || canonical != name {
		t.Fatalf("pinned catalog name = %q, %v", canonical, err)
	}
	entry, err := catalog.NewFileEntry(fileID(t, 4), name, 1, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	page, payload, err := catalog.NewCatalogPage(catalog.CatalogPageSpec{
		ShareInstance: shareInstance(t, 1), DirectoryID: directoryID(t, 2), Generation: generationID(t, 3),
		Entries: []catalog.Entry{entry}, Terminal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := catalog.DecodeCatalogPage(payload.Bytes())
	if err != nil || decoded.Commitment() != page.Commitment() {
		t.Fatalf("catalog canonical bytes changed: %v", err)
	}
	if got := decoded.Entries()[0].Name(); got != name {
		t.Fatalf("decoded name = %q, want %q", got, name)
	}
}
