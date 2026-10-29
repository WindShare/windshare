//go:build darwin

package outputportable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestNativeDirectorySpellingAcceptsAPFSNormalization(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "e\u0301"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "é")); os.IsNotExist(err) {
		t.Skip("destination has normalization-sensitive lookup")
	} else if err != nil {
		t.Fatal(err)
	}
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if kind, exact, err := platform.Root().ClassifyExactEntry("é"); err != nil || !exact || kind != outputcap.EntryDirectory {
		t.Fatalf("canonical catalog spelling: %v, %v, %v", kind, exact, err)
	}
}
