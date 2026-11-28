package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
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

func TestSelectedUnresolvableSymbolicLinkIsAnUnsupportedSource(t *testing.T) {
	root := t.TempDir()
	loopA := filepath.Join(root, "loop-a")
	loopB := filepath.Join(root, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths: []string{loopA}, SyntheticRoot: catalog.DirectoryID{1},
	})
	if source != nil || !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("selected looping symbolic link = %v, %v", source, err)
	}
	path, pathOK := errors.AsType[*catalog.SelectionError](err)
	failure, failureOK := errors.AsType[*catalog.SourceError](err)
	if !pathOK || path.Label != loopA || !failureOK || failure.Failure != catalog.SourceFailureUnsupported {
		t.Fatalf("unsupported selection lost its identity: %+v, %+v", path, failure)
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
