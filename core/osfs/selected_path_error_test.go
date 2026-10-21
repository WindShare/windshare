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
	selected, ok := errors.AsType[*SelectedPathError](err)
	if !ok || selected.Path != missing {
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

func TestSelectedSymbolicLinkIsAnUnsupportedSource(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(root, "link")
	if err := os.Symlink(target, selected); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	source, err := NewSelectedFileSource(context.Background(), SelectedCatalogSourceConfig{
		Paths: []string{selected}, SyntheticRoot: catalog.DirectoryID{1},
	})
	if source != nil || !errors.Is(err, content.ErrUnsupportedStability) {
		t.Fatalf("selected symbolic link = %v, %v", source, err)
	}
	path, pathOK := errors.AsType[*SelectedPathError](err)
	failure, failureOK := errors.AsType[*catalog.SourceError](err)
	if !pathOK || path.Path != selected || !failureOK || failure.Failure != catalog.SourceFailureUnsupported {
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
