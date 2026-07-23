package catalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProgressSubscriptionReplacesBackpressuredMilestoneWithNewest(t *testing.T) {
	attempt := idValue[ScanAttemptID](1)
	subscription := &scanProgressSubscription{updates: make(chan ScanProgress, 1)}
	subscription.enqueueLatest(ScanProgress{AttemptID: attempt, DiscoveredEntries: 1})
	subscription.enqueueLatest(ScanProgress{
		AttemptID: attempt, DiscoveredEntries: ScanProgressEntryInterval,
	})

	select {
	case progress := <-subscription.updates:
		if progress.AttemptID != attempt || progress.DiscoveredEntries != ScanProgressEntryInterval {
			t.Fatalf("coalesced progress = %+v", progress)
		}
	default:
		t.Fatal("backpressured progress queue lost its newest milestone")
	}
}

func TestFileBackendPublishedGenerationReplaysThroughExistingPreparation(t *testing.T) {
	_, backend, path, meta := durableRecoveryFixture(t)
	defer backend.Destroy()

	directoryBytes, err := readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := decodeNodeRecord(directoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	children, err := os.Open(filepath.Join(path, fileCatalogChildrenName))
	if err != nil {
		t.Fatal(err)
	}
	childBytes, ok, err := readNodeFrame(children)
	closeErr := children.Close()
	if err != nil || !ok || closeErr != nil {
		t.Fatalf("read replay child: found=%v err=%v close=%v", ok, err, closeErr)
	}
	child, err := decodeNodeRecord(childBytes)
	if err != nil {
		t.Fatal(err)
	}

	meter := backendTestMeter(t)
	defer meter.Close()
	transaction := stageBackendGeneration(
		t, backend, meta.share, directory, meta.generation, []NodeRecord{child}, meter,
	)
	fileTransaction := transaction.(*fileCatalogTransaction)
	stagingPath := fileTransaction.path
	first, err := transaction.Prepare(context.Background())
	if err != nil || !first.Existing || first.Directory != meta.committed() {
		t.Fatalf("existing prepare = %+v err=%v", first, err)
	}
	second, err := transaction.Prepare(context.Background())
	if err != nil || second != first {
		t.Fatalf("idempotent prepare = %+v err=%v", second, err)
	}
	committed, err := transaction.Publish(context.Background())
	if err != nil || committed != meta.committed() {
		t.Fatalf("existing publish = %+v err=%v", committed, err)
	}
	if _, err := os.Stat(stagingPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replayed staging path survived publication: %v", err)
	}
	loaded, found, err := backend.LoadDirectory(context.Background(), meta.directory)
	if err != nil || !found || loaded != committed {
		t.Fatalf("published authority changed after replay: found=%v value=%+v err=%v", found, loaded, err)
	}
}

func TestFileBackendPrepareRejectsGenerationWithoutTerminalPage(t *testing.T) {
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: t.TempDir(), ShareInstance: idValue[ShareInstance](11),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy()
	meter := backendTestMeter(t)
	defer meter.Close()
	directoryID := idValue[DirectoryID](12)
	transaction, err := backend.BeginDirectory(
		context.Background(), directoryID, idValue[DirectoryGeneration](13), meter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutDirectory(backendDirectoryRecord(
		t, directoryID, idValue[DirectoryID](10), "incomplete", 1,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Prepare(context.Background()); !errors.Is(err, ErrPageSequence) {
		t.Fatalf("prepare without terminal page = %v", err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestFileBackendTransactionsPropagateBudgetFailuresBeforePublication(t *testing.T) {
	injected := errors.New("durable meter rejected growth")
	share := idValue[ShareInstance](20)
	directoryID := idValue[DirectoryID](21)
	generation := idValue[DirectoryGeneration](22)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](19), "metered", 1)
	child, err := scannedFile(t, 23, "child", 1).nodeRecord(directoryID)
	if err != nil {
		t.Fatal(err)
	}

	for _, failAt := range []int{1, 2} {
		t.Run("directory-consume-"+string(rune('0'+failAt)), func(t *testing.T) {
			backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Destroy()
			meter := &scriptedResourceMeter{failConsumeAt: failAt, err: injected}
			transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
			if err != nil {
				t.Fatal(err)
			}
			path := transaction.(*fileCatalogTransaction).path
			if err := transaction.PutDirectory(directory); !errors.Is(err, injected) {
				t.Fatalf("directory consume %d = %v", failAt, err)
			}
			if err := transaction.Abort(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed directory transaction survived abort: %v", err)
			}
		})
	}

	for _, failAt := range []int{1, 2} {
		t.Run("child-consume-"+string(rune('0'+failAt)), func(t *testing.T) {
			backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Destroy()
			realMeter := backendTestMeter(t)
			defer realMeter.Close()
			transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, realMeter)
			if err != nil {
				t.Fatal(err)
			}
			if err := transaction.PutDirectory(directory); err != nil {
				t.Fatal(err)
			}
			transaction.(*fileCatalogTransaction).meter = &scriptedResourceMeter{
				failConsumeAt: failAt, err: injected,
			}
			if err := transaction.PutChild(child); !errors.Is(err, injected) {
				t.Fatalf("child consume %d = %v", failAt, err)
			}
			if err := transaction.Abort(); err != nil {
				t.Fatal(err)
			}
		})
	}

	page, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: share, DirectoryID: directoryID, Generation: generation,
		Entries: []Entry{child.Entry()}, Terminal: true,
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	pageObject := mustTestPageObject(t, page)
	pageCases := []struct {
		name          string
		failConsumeAt int
		failReleaseAt int
	}{
		{name: "page-memory", failConsumeAt: 1},
		{name: "page-storage", failConsumeAt: 2},
		{name: "object-storage", failConsumeAt: 3},
		{name: "pending-release", failReleaseAt: 1},
	}
	for _, testCase := range pageCases {
		t.Run(testCase.name, func(t *testing.T) {
			backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: t.TempDir(), ShareInstance: share})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Destroy()
			realMeter := backendTestMeter(t)
			defer realMeter.Close()
			transaction := stageFileTransactionThroughChild(
				t, backend, directory, generation, child, realMeter,
			)
			transaction.meter = &scriptedResourceMeter{
				failConsumeAt: testCase.failConsumeAt,
				failReleaseAt: testCase.failReleaseAt,
				err:           injected,
			}
			if err := transaction.PutPage(page, pageObject); !errors.Is(err, injected) {
				t.Fatalf("page meter failure = %v", err)
			}
			if err := transaction.Abort(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func stageFileTransactionThroughChild(
	t *testing.T,
	backend *FileCatalogBackend,
	directory NodeRecord,
	generation DirectoryGeneration,
	child NodeRecord,
	meter ResourceMeter,
) *fileCatalogTransaction {
	t.Helper()
	directoryID, ok := directory.DirectoryID()
	if !ok {
		t.Fatal("fixture is not a directory")
	}
	transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutChild(child); err != nil {
		t.Fatal(err)
	}
	return transaction.(*fileCatalogTransaction)
}

func TestFileBackendLoadsRejectMissingOrMisaddressedObjects(t *testing.T) {
	t.Run("directory metadata identity", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		requested := meta.directory
		meta.directory = idValue[DirectoryID](41)
		writeBackendMeta(t, path, meta)
		if _, _, err := backend.LoadDirectory(context.Background(), requested); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("misaddressed directory metadata = %v", err)
		}
	})

	t.Run("missing page", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, "pages", "00000000.page")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPage(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing addressed page = %v", err)
		}
	})

	t.Run("corrupt page object metadata", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.WriteFile(filepath.Join(path, fileCatalogMetaName), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPageObject(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("page object with corrupt metadata = %v", err)
		}
	})

	t.Run("missing page object", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, fileCatalogObjectsName, "00000000.object")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPageObject(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing addressed page object = %v", err)
		}
	})

	t.Run("empty page object", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.WriteFile(filepath.Join(path, fileCatalogObjectsName, "00000000.object"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPageObject(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("empty addressed page object = %v", err)
		}
	})

	t.Run("missing page behind object", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, "pages", "00000000.page")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPageObject(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("page object without addressed page = %v", err)
		}
	})

	t.Run("misaddressed page behind object", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		other, err := NewCatalogPage(CatalogPageSpec{
			ShareInstance: meta.share, DirectoryID: idValue[DirectoryID](42),
			Generation: meta.generation, Terminal: true,
		}, semanticTestCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encodeCatalogPage(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "pages", "00000000.page"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadPageObject(context.Background(), meta.directory, meta.generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("page object with misaddressed page = %v", err)
		}
	})
}

func TestFileBackendRecoveryRejectsMissingIdentityAndPageSequenceAuthority(t *testing.T) {
	t.Run("missing children authority", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, fileCatalogChildrenName)); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Recover(context.Background()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery without children authority = %v", err)
		}
	})

	t.Run("directory record identity", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		other := backendDirectoryRecord(
			t, idValue[DirectoryID](51), idValue[DirectoryID](49), "other", 2,
		)
		encoded, err := encodeNodeRecord(other)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, fileCatalogDirectoryName), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("metadata %x accepted directory record %x: %v", meta.directory, other.NodeID(), err)
		}
	})

	t.Run("page storage address", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		pagePath := filepath.Join(path, "pages", "00000000.page")
		encoded, err := readCatalogObject(pagePath)
		if err != nil {
			t.Fatal(err)
		}
		original, err := decodeCatalogPage(encoded)
		if err != nil {
			t.Fatal(err)
		}
		misaddressed, err := NewCatalogPage(CatalogPageSpec{
			ShareInstance: original.ShareInstance(), DirectoryID: original.DirectoryID(),
			Generation: original.Generation(), PageIndex: 1, Previous: original.Commitment(),
			Entries: original.Entries(), Terminal: true,
		}, semanticTestCommitter{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err = encodeCatalogPage(misaddressed)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pagePath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		object := mustTestPageObject(t, misaddressed)
		if err := os.WriteFile(
			filepath.Join(path, fileCatalogObjectsName, "00000000.object"), object.Bytes(), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrPageSequence) {
			t.Fatalf("misaddressed stored page = %v", err)
		}
	})
}

func TestFileBackendRecoversEmptyPublishedGeneration(t *testing.T) {
	root := t.TempDir()
	share := idValue[ShareInstance](60)
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	directoryID := idValue[DirectoryID](61)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](59), "empty", 1)
	generation := idValue[DirectoryGeneration](62)
	meter := backendTestMeter(t)
	published := publishBackendGeneration(t, stageBackendGeneration(
		t, backend, share, directory, generation, nil, meter,
	))
	meter.Close()
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Destroy()
	usage, err := recovered.Recover(context.Background())
	if err != nil || usage.Entries != 0 || usage.SpillBytes == 0 {
		t.Fatalf("empty generation recovery usage = %+v err=%v", usage, err)
	}
	loaded, found, err := recovered.LoadDirectory(context.Background(), directoryID)
	if err != nil || !found || loaded != published || loaded.EntryCount() != 0 || loaded.PageCount() != 1 {
		t.Fatalf("empty recovered directory = %+v found=%v err=%v", loaded, found, err)
	}
	page, found, err := recovered.LoadPage(context.Background(), directoryID, generation, 0)
	if err != nil || !found || !page.Terminal() || page.EntryCount() != 0 {
		t.Fatalf("empty recovered page = %+v found=%v err=%v", page, found, err)
	}
}

func TestFileBackendLoadNodePropagatesNamespaceAndObjectFailures(t *testing.T) {
	t.Run("missing committed namespace", func(t *testing.T) {
		_, backend, _, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.RemoveAll(backend.committedDir); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](70)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing committed namespace = %v", err)
		}
	})

	t.Run("missing metadata", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, fileCatalogMetaName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](71)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing node namespace metadata = %v", err)
		}
	})

	t.Run("missing directory record", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, fileCatalogDirectoryName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](72)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing directory record = %v", err)
		}
	})

	t.Run("corrupt directory record", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.WriteFile(filepath.Join(path, fileCatalogDirectoryName), []byte{0xff}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](73)); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("corrupt directory record = %v", err)
		}
	})

	t.Run("missing child stream", func(t *testing.T) {
		_, backend, path, _ := durableRecoveryFixture(t)
		defer backend.Destroy()
		if err := os.Remove(filepath.Join(path, fileCatalogChildrenName)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](74)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing child stream = %v", err)
		}
	})

	t.Run("excluded generation", func(t *testing.T) {
		_, backend, path, meta := durableRecoveryFixture(t)
		defer backend.Destroy()
		encoded, err := readCatalogObject(filepath.Join(path, fileCatalogDirectoryName))
		if err != nil {
			t.Fatal(err)
		}
		directory, err := decodeNodeRecord(encoded)
		if err != nil {
			t.Fatal(err)
		}
		backend.mu.RLock()
		_, found, loadErr := backend.loadNodeLocked(context.Background(), directory.NodeID(), meta.directory)
		backend.mu.RUnlock()
		if loadErr != nil || found {
			t.Fatalf("excluded generation participated in collision lookup: found=%v err=%v", found, loadErr)
		}
	})
}
