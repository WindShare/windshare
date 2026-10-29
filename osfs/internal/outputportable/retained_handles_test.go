package outputportable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

// Publication must consume the existing data object even after its caller has
// released the directory used to create it.
func TestRetainedOriginsSurviveClosingTheirSourceDirectories(t *testing.T) {
	platform, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	root := platform.Root()
	leaf, err := root.CreateDirectory("nested", true)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	candidate, err := leaf.CreateDirectory("candidate", true)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	retained, err := candidate.Duplicate()
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	source, err := leaf.CreateFile("source", false, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(candidate.Close(), leaf.Close()); err != nil {
		t.Fatal(err)
	}
	installed, err := root.InstallDirectoryNoReplace(retained, "installed")
	if err != nil {
		t.Fatal(err)
	}
	defer installed.Close()
	if same, err := installed.SameDirectory(retained); err != nil || !same {
		t.Fatalf("installation copied the directory: same=%t err=%v", same, err)
	}
	publisher := root.(interface {
		PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
	})
	if outcome, err := publisher.PublishFileNoReplace(source, "published"); err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish from retained parent: outcome=%v err=%v", outcome, err)
	}
	final, err := root.OpenObservedFile("published", false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if same, err := final.SameFile(source); err != nil || !same {
		t.Fatalf("publication copied the file: same=%t err=%v", same, err)
	}
}

func TestPinnedRemovalRejectsReplacedEntries(t *testing.T) {
	for _, isDirectory := range []bool{false, true} {
		name := "file"
		if isDirectory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			platform, err := Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			root := platform.Root()
			create := func() {
				t.Helper()
				if isDirectory {
					child, err := root.CreateDirectory("entry", false)
					if err != nil {
						t.Fatal(err)
					}
					if err := child.Close(); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(filepath.Join(path, "entry"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			create()
			pinned, err := root.OpenEntry("entry")
			if err != nil {
				t.Fatal(err)
			}
			defer pinned.Close()
			if err := os.Rename(filepath.Join(path, "entry"), filepath.Join(path, "displaced")); err != nil {
				t.Fatal(err)
			}
			create()
			if err := root.RemoveEntry("entry", pinned); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("replacement inherited removal authority: %v", err)
			}
			if _, err := os.Stat(filepath.Join(path, "entry")); err != nil {
				t.Fatalf("replacement was removed: %v", err)
			}
			if err := root.RemoveEntry("displaced", pinned); err != nil {
				t.Fatalf("original entry could not be removed: %v", err)
			}
		})
	}
}
