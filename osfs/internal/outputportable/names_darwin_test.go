//go:build darwin

package outputportable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestNativeEntrySpellingUsesDirectoryEntryForHardLinks(t *testing.T) {
	for _, test := range []struct {
		name, anchor, actual, requested string
		sameDirectory, exact            bool
	}{
		{name: "same directory", anchor: "partial", actual: "download.bin", requested: "download.bin", sameDirectory: true, exact: true},
		{name: "different directories", anchor: "partial", actual: "download.bin", requested: "download.bin", exact: true},
		{name: "case alias", anchor: "report.txt", actual: "Report.txt", requested: "report.txt"},
		{name: "normalized name", anchor: "partial", actual: "e\u0301", requested: "é", exact: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			anchor := filepath.Join(root, test.anchor)
			if test.sameDirectory {
				anchor = filepath.Join(target, test.anchor)
			}
			if err := os.WriteFile(anchor, []byte("owned data"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(anchor, filepath.Join(target, test.actual)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(target, test.requested)); os.IsNotExist(err) {
				t.Skip("destination has case- or normalization-sensitive lookup")
			} else if err != nil {
				t.Fatal(err)
			}
			platform, err := Open(target, false)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()

			// Darwin caches a name on the shared vnode. Concurrent lookups of
			// another hard link must not decide this directory entry's spelling.
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				for {
					select {
					case <-stop:
						done <- nil
						return
					default:
					}
					handle, err := os.Open(anchor)
					if err == nil {
						err = handle.Close()
					}
					if err != nil {
						done <- err
						return
					}
				}
			}()
			defer func() {
				close(stop)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			const spellingObservations = 256
			for attempt := range spellingObservations {
				kind, exact, err := platform.Root().ClassifyExactEntry(test.requested)
				if err != nil || kind != outputcap.EntryRegularFile || exact != test.exact {
					t.Fatalf("observation %d of %q (stored %q, other link %q): kind=%v exact=%v err=%v",
						attempt, test.requested, test.actual, test.anchor, kind, exact, err)
				}
			}
		})
	}
}

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
