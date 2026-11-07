package catalogflow

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestCatalogPagePreservesPostUnicode15Name(t *testing.T) {
	name := "\U00016D63\U00016D67.txt"
	canonical, err := catalog.CanonicalName(name)
	if err != nil || canonical != name {
		t.Fatalf("pinned catalog name = %q, %v", canonical, err)
	}
	snapshot := onePageSnapshot(t, shareInstance(t, 1), directoryID(t, 2), 3, name)
	page, _ := snapshot.Page(0)
	encoded, err := encodeCatalogPage(page)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeCatalogPage(encoded, testCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Entries()[0].Name(); got != name {
		t.Fatalf("decoded name = %q, want %q", got, name)
	}
	reencoded, err := encodeCatalogPage(decoded)
	if err != nil || !bytes.Equal(reencoded, encoded) {
		t.Fatalf("catalog canonical bytes changed: %v", err)
	}
}
