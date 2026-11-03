//go:build linux || darwin

package outputportable

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDirectorySpellingPrefersExactHardLinkAcrossBatches(t *testing.T) {
	root := t.TempDir()
	anchor := filepath.Join(root, "anchor")
	if err := os.WriteFile(anchor, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Every entry shares the same object; neither the first inode match nor
	// the end of one batch identifies the requested directory entry.
	for index := range nativeSpellingBatchSize + 1 {
		name := fmt.Sprintf("link-%03d", index)
		if err := os.Link(anchor, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	parent := platform.Root().(*directory)
	observed, err := os.Stat(anchor)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		exact, err := directoryEntryMatchesSpelling(parent, entry.Name(), observed)
		if err != nil || !exact {
			t.Fatalf("entry %q: exact=%v err=%v", entry.Name(), exact, err)
		}
	}
	if exact, err := directoryEntryMatchesSpelling(parent, "ANCHOR", observed); err != nil || exact {
		t.Fatalf("another hard link authorized a missing spelling: exact=%v err=%v", exact, err)
	}
}

func TestDirectorySpellingRejectsReplacedObject(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target")
	original, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	observed, err := original.Stat()
	if err != nil {
		t.Fatal(err)
	}
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if exact, err := directoryEntryMatchesSpelling(platform.Root().(*directory), "target", observed); !errors.Is(err, outputcap.ErrUnsafeNamespace) || exact {
		t.Fatalf("replacement inherited entry ownership: exact=%v err=%v", exact, err)
	}
}
