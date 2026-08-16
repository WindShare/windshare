package liveshare

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestSenderSelectedRootSummaryFreezesAlreadyOpenedFileFact(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "selected.bin")
	original := []byte("opened-file-fact")
	if err := os.WriteFile(filename, original, 0o600); err != nil {
		t.Fatal(err)
	}
	sender := prepareDisplaySender(t, []string{filename})

	summary := sender.SelectedRootSummary()
	selected, ok := summary.SingleRoot()
	if !ok || summary.SelectedCount() != 1 || selected.Name() != filepath.Base(filename) ||
		selected.Kind() != SelectedRootKindFile {
		t.Fatalf("single-file summary = count %d, root %#v, single %v", summary.SelectedCount(), selected, ok)
	}
	if size, known := selected.FileSize(); !known || size != uint64(len(original)) {
		t.Fatalf("single-file size = %d, known %v", size, known)
	}

	if err := os.WriteFile(filename, append(original, original...), 0o600); err != nil {
		t.Fatal(err)
	}
	summary.selectedCount = 99
	summary.single.name = "mutated-copy"
	if err := sender.Close(); err != nil {
		t.Fatal(err)
	}
	frozen := sender.SelectedRootSummary()
	frozenRoot, ok := frozen.SingleRoot()
	if !ok || frozen.SelectedCount() != 1 || frozenRoot.Name() != filepath.Base(filename) {
		t.Fatalf("returned summary mutated sender fact: count %d, root %#v", frozen.SelectedCount(), frozenRoot)
	}
	if size, known := frozenRoot.FileSize(); !known || size != uint64(len(original)) {
		t.Fatalf("summary restated a changed file: size %d, known %v", size, known)
	}
}

func TestSenderSelectedRootSummaryDoesNotScanDirectoryDescendants(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "selected-tree")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "secret-descendant.txt"), []byte("descendant"), 0o600); err != nil {
		t.Fatal(err)
	}
	sender := prepareDisplaySender(t, []string{directory})
	t.Cleanup(func() { _ = sender.Close() })

	summary := sender.SelectedRootSummary()
	selected, ok := summary.SingleRoot()
	if !ok || selected.Name() != filepath.Base(directory) || selected.Kind() != SelectedRootKindDirectory {
		t.Fatalf("single-directory summary = %#v, single %v", selected, ok)
	}
	if size, known := selected.FileSize(); known || size != 0 {
		t.Fatalf("directory summary exposed a descendant-derived size: %d, known %v", size, known)
	}
	records := sender.selectedSource.SelectedRoots()
	directoryID, ok := records[0].DirectoryID()
	if !ok {
		t.Fatal("selected directory lost directory identity")
	}
	if _, found, err := sender.catalogStore.Directory(context.Background(), directoryID); err != nil || found {
		t.Fatalf("display summary scanned selected directory: found %v, err %v", found, err)
	}
}

func TestSenderSelectedRootSummaryExposesOnlyCountForMultipleRoots(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.bin")
	second := filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0o700); err != nil {
		t.Fatal(err)
	}
	sender := prepareDisplaySender(t, []string{first, second})
	t.Cleanup(func() { _ = sender.Close() })

	summary := sender.SelectedRootSummary()
	if summary.SelectedCount() != 2 {
		t.Fatalf("multiple-root count = %d", summary.SelectedCount())
	}
	if selected, ok := summary.SingleRoot(); ok || selected != (SelectedRootDisplay{}) {
		t.Fatalf("multiple-root summary exposed a selected root: %#v, single %v", selected, ok)
	}
	if got := (*PreparedSender)(nil).SelectedRootSummary(); got.SelectedCount() != 0 {
		t.Fatalf("nil sender summary = %#v", got)
	}
}

func TestNewSelectedRootSummaryValidation(t *testing.T) {
	if _, err := newSelectedRootSummary(nil); err == nil {
		t.Fatal("expected error for nil selected roots")
	}
	if _, err := newSelectedRootSummary([]catalog.NodeRecord{}); err == nil {
		t.Fatal("expected error for empty selected roots")
	}
	if _, err := newSelectedRootSummary([]catalog.NodeRecord{{}}); err == nil {
		t.Fatal("expected error for empty node record")
	}
}

func prepareDisplaySender(t *testing.T, paths []string) *PreparedSender {
	t.Helper()
	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths: paths, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
		CatalogStorage: CatalogStorageFactoryFunc(func(context.Context, catalog.ShareInstance) (catalog.CatalogBackend, error) {
			return catalog.NewMemoryCatalogBackend(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sender
}
