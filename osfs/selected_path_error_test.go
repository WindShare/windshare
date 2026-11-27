package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSelectionFailureIdentifiesFailedRootAndReleasesEarlierRoots(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	if err := os.Mkdir(first, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing-parent", "missing-file")
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths: []string{first, missing}, SyntheticRoot: catalog.DirectoryID{1},
	})
	if source != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selection = %v, %v", source, err)
	}
	selected, ok := errors.AsType[*catalog.SelectionError](err)
	if !ok || selected.Label != missing {
		t.Fatalf("failed root = %+v, want %q", selected, missing)
	}
	failure, ok := errors.AsType[*catalog.SourceError](err)
	if !ok || failure.Failure != catalog.SourceFailureMissing {
		t.Fatalf("source classification = %+v", failure)
	}
	if err := os.Remove(first); err != nil {
		t.Fatalf("earlier selected root was not released: %v", err)
	}
}

func TestSelectedDanglingLinkIsAMissingSource(t *testing.T) {
	root := t.TempDir()
	selected := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "target"), selected); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths: []string{selected}, SyntheticRoot: catalog.DirectoryID{1},
	})
	if source != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected dangling link = %v, %v", source, err)
	}
	path, pathOK := errors.AsType[*catalog.SelectionError](err)
	failure, failureOK := errors.AsType[*catalog.SourceError](err)
	if !pathOK || path.Label != selected || !failureOK || failure.Failure != catalog.SourceFailureMissing {
		t.Fatalf("missing link target lost its identity: %+v, %+v", path, failure)
	}
}

func TestSourceInterruptionPreservesCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		err := classifySourceError("open stable file", catalog.SourceReference{}, cause)
		if _, classified := errors.AsType[*catalog.SourceError](err); !errors.Is(err, cause) || classified {
			t.Fatalf("interruption classified as a source fault: %v", err)
		}
	}
}
