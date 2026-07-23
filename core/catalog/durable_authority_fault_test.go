package catalog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFileBackendNamespaceFailuresRemainUnpublished(t *testing.T) {
	share := idValue[ShareInstance](80)

	t.Run("constructor namespace", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFileCatalogBackend(FileCatalogBackendConfig{
			Root: filepath.Join(blocker, "catalog"), ShareInstance: share,
		}); err == nil {
			t.Fatal("backend was created below a regular file")
		}
	})

	t.Run("transaction namespace", func(t *testing.T) {
		backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Destroy()
		if err := os.RemoveAll(backend.stagingDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(backend.stagingDir, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		meter := backendTestMeter(t)
		defer meter.Close()
		if _, err := backend.BeginDirectory(
			context.Background(), idValue[DirectoryID](81), idValue[DirectoryGeneration](82), meter,
		); err == nil {
			t.Fatal("transaction started below a regular file")
		}
	})

	t.Run("publication namespace lookup", func(t *testing.T) {
		root := t.TempDir()
		backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Destroy()
		transaction := preparedEmptyFileTransaction(t, backend, share, 83)
		blocker := filepath.Join(root, "publish-blocker")
		if err := os.WriteFile(blocker, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		backend.committedDir = blocker
		if _, err := transaction.Publish(context.Background()); err == nil {
			t.Fatal("publication ignored an invalid committed namespace")
		}
		if transaction.finished {
			t.Fatal("failed namespace lookup finished the transaction")
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("publication directory sync", func(t *testing.T) {
		backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Destroy()
		transaction := preparedEmptyFileTransaction(t, backend, share, 86)
		target := backend.directoryPath(transaction.directory)
		if err := os.RemoveAll(transaction.pagesPath); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Publish(context.Background()); err == nil {
			t.Fatal("publication ignored a missing staged page namespace")
		}
		if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed publication exposed target: %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})
}

func preparedEmptyFileTransaction(
	t *testing.T,
	backend *FileCatalogBackend,
	share ShareInstance,
	seed byte,
) *fileCatalogTransaction {
	t.Helper()
	meter := backendTestMeter(t)
	t.Cleanup(meter.Close)
	directoryID := idValue[DirectoryID](seed)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](seed-1), "prepared", seed)
	transaction := stageBackendGeneration(
		t, backend, share, directory, idValue[DirectoryGeneration](seed+1), nil, meter,
	).(*fileCatalogTransaction)
	if _, err := transaction.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	return transaction
}

func TestFileBackendPrepareRejectsUnreadableExistingAuthority(t *testing.T) {
	t.Run("same generation metadata", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		directory, children := readDurableGenerationRecords(t, path)
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction := stageBackendGeneration(
			t, backend, meta.share, directory, meta.generation, children, meter,
		)
		if err := os.WriteFile(filepath.Join(path, fileCatalogMetaName), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Prepare(context.Background()); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("prepare with corrupt existing metadata = %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("foreign collision authority", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		meter := backendTestMeter(t)
		defer meter.Close()
		directoryID := idValue[DirectoryID](91)
		directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](89), "new", 3)
		transaction := stageBackendGeneration(
			t, backend, meta.share, directory, idValue[DirectoryGeneration](92), nil, meter,
		)
		if err := os.WriteFile(filepath.Join(path, fileCatalogMetaName), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := transaction.Prepare(context.Background()); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("prepare guessed through corrupt collision authority = %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})
}

func readDurableGenerationRecords(t *testing.T, path string) (NodeRecord, []NodeRecord) {
	t.Helper()
	encoded, err := readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := decodeNodeRecord(encoded)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var children []NodeRecord
	for {
		encoded, ok, err := readNodeFrame(file)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return directory, children
		}
		child, err := decodeNodeRecord(encoded)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, child)
	}
}

func TestFileTransactionRejectsPrivateMutationAndMisaddressedFirstPage(t *testing.T) {
	share := idValue[ShareInstance](100)
	directoryID := idValue[DirectoryID](101)
	generation := idValue[DirectoryGeneration](102)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](99), "pages", 1)
	child, err := scannedFile(t, 103, "child", 1).nodeRecord(directoryID)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("private child mutation", func(t *testing.T) {
		backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Destroy()
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction := stageFileTransactionThroughChild(t, backend, directory, generation, child, meter)
		other, err := scannedFile(t, 104, "other", 2).nodeRecord(directoryID)
		if err != nil {
			t.Fatal(err)
		}
		page, err := NewCatalogPage(CatalogPageSpec{
			ShareInstance: share, DirectoryID: directoryID, Generation: generation,
			Entries: []Entry{other.Entry()}, Terminal: true,
		}, semanticTestCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.PutPage(page, mustTestPageObject(t, page)); err == nil {
			t.Fatal("durable page changed its private child record")
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("storage page address", func(t *testing.T) {
		backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Destroy()
		meter := backendTestMeter(t)
		defer meter.Close()
		transaction := stageFileTransactionThroughChild(t, backend, directory, generation, child, meter)
		var previous PageCommitment
		previous[0] = 1
		page, err := NewCatalogPage(CatalogPageSpec{
			ShareInstance: share, DirectoryID: directoryID, Generation: generation,
			PageIndex: 1, Previous: previous, Entries: []Entry{child.Entry()}, Terminal: true,
		}, semanticTestCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.PutPage(page, mustTestPageObject(t, page)); !errors.Is(err, ErrPageSequence) {
			t.Fatalf("misaddressed first durable page = %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestFileTransactionStageNeverOverwritesExistingObject(t *testing.T) {
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: t.TempDir(), ShareInstance: idValue[ShareInstance](110),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy()
	meter := &scriptedResourceMeter{}
	transaction, err := backend.BeginDirectory(
		context.Background(), idValue[DirectoryID](111), idValue[DirectoryGeneration](112), meter,
	)
	if err != nil {
		t.Fatal(err)
	}
	fileTransaction := transaction.(*fileCatalogTransaction)
	path := filepath.Join(fileTransaction.path, "immutable")
	if err := fileTransaction.stage(path, []byte("first"), FileFaultStageDirectory); err != nil {
		t.Fatal(err)
	}
	if err := fileTransaction.stage(path, []byte("second"), FileFaultStageDirectory); !errors.Is(err, os.ErrExist) {
		t.Fatalf("duplicate staged object = %v", err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, []byte("first")) {
		t.Fatalf("duplicate stage overwrote object: %q", stored)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableRecoveryCancellationAndTruncatedFrameAreExplicit(t *testing.T) {
	t.Run("validation cancellation", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		meta, directoryBytes, err := backend.readCommittedIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		children, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
		if err != nil {
			t.Fatal(err)
		}
		defer children.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := validateCommittedContents(ctx, path, meta, directoryBytes, children); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled committed validation = %v", err)
		}
	})

	t.Run("collision lookup cancellation", func(t *testing.T) {
		_, backend, _, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		backend.mu.RLock()
		_, _, err := backend.loadNodeLocked(ctx, idValue[NodeID](120), DirectoryID{})
		backend.mu.RUnlock()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled collision lookup = %v", err)
		}
	})

	t.Run("truncated node payload", func(t *testing.T) {
		encoded, found, err := readNodeFrame(bytes.NewReader([]byte{0, 0, 0, 2, 1}))
		if !errors.Is(err, io.ErrUnexpectedEOF) || found || encoded != nil {
			t.Fatalf("truncated node frame = %x found=%v err=%v", encoded, found, err)
		}
	})
}

func TestNilScanProgressObserverReturnsBoundaryError(t *testing.T) {
	var observer ScanProgressObserverFunc
	if err := observer.ObserveScanProgress(context.Background(), ScanProgress{}); err == nil {
		t.Fatal("nil progress observer succeeded")
	}
}
