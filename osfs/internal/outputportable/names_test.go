package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestLocatorIdentityDoesNotInferNativeEquivalence(t *testing.T) {
	platform := new(Platform)
	for _, pair := range [][2]string{{"İ.txt", "i.txt"}, {"Makefile", "makefile"}, {"ς", "σ"}} {
		left, leftErr := platform.CanonicalLocatorKey(pair[0])
		right, rightErr := platform.CanonicalLocatorKey(pair[1])
		if leftErr != nil || rightErr != nil || left == right {
			t.Fatalf("%q and %q merged: %q/%q, %v/%v", pair[0], pair[1], left, right, leftErr, rightErr)
		}
	}
	if _, err := platform.CanonicalLocatorKey("../escape"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe path: %v", err)
	}
	// A snapshot observes native spellings, including names outside catalog
	// policy. Unrelated entries must not prevent admitting a valid output.
	for _, spelling := range []string{"K", "e\u0301", "trailing."} {
		if key, err := platform.CanonicalComponentKey(spelling); err != nil || key != spelling {
			t.Fatalf("snapshot spelling %q: %q, %v", spelling, key, err)
		}
	}
}

func TestDirectoryClassificationUsesResolvedNativeSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	directory := platform.Root().(*directory)
	if kind, exact, err := directory.ClassifyExactEntry("Folder"); err != nil || !exact || kind != outputcap.EntryDirectory {
		t.Fatalf("exact directory: %v, %v, %v", kind, exact, err)
	}
	_, lookupErr := os.Stat(filepath.Join(root, "folder"))
	kind, exact, err := directory.ClassifyExactEntry("folder")
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case errors.Is(lookupErr, fs.ErrNotExist):
		if kind != outputcap.EntryAbsent || !exact {
			t.Fatalf("case-sensitive absence: %v, %v", kind, exact)
		}
		if err := os.Mkdir(filepath.Join(root, "folder"), 0o700); err != nil {
			t.Fatal(err)
		}
		if kind, exact, err := directory.ClassifyExactEntry("folder"); err != nil || !exact || kind != outputcap.EntryDirectory {
			t.Fatalf("distinct lowercase directory: %v, %v, %v", kind, exact, err)
		}
	case lookupErr == nil:
		if kind != outputcap.EntryDirectory || exact {
			t.Fatalf("case-insensitive alias was considered exact: %v, %v", kind, exact)
		}
	default:
		t.Fatal(lookupErr)
	}
}

func TestNativePublicationStillRejectsCaseAlias(t *testing.T) {
	root := t.TempDir()
	final := filepath.Join(root, "Report.txt")
	if err := os.WriteFile(final, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "report.txt")); errors.Is(err, fs.ErrNotExist) {
		t.Skip("destination has case-sensitive lookup")
	} else if err != nil {
		t.Fatal(err)
	}
	platform, err := Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	directory := platform.Root().(*directory)
	if kind, exact, err := directory.ClassifyExactEntry("report.txt"); err != nil || exact || kind != outputcap.EntryRegularFile {
		t.Fatalf("case alias of a file: %v, %v, %v", kind, exact, err)
	}
	stage, err := directory.CreateFile("stage", true, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if outcome, err := directory.PublishFileNoReplace(stage, "report.txt"); err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("native collision: %v, %v", outcome, err)
	}
	if actual, err := os.ReadFile(final); err != nil || string(actual) != "foreign" {
		t.Fatalf("foreign file changed: %q, %v", actual, err)
	}
}
