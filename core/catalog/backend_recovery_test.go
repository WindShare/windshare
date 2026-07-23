package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func backendTestMeter(t *testing.T) *attemptResourceMeter {
	t.Helper()
	meter, err := newAttemptResourceMeter(BudgetHierarchy{
		Process: generousBudget(t, "backend-process"),
		Share:   generousBudget(t, "backend-share"),
		Session: generousBudget(t, "backend-session"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return meter
}

func backendDirectoryRecord(t *testing.T, directory, parent DirectoryID, name string, identity byte) NodeRecord {
	t.Helper()
	locator, err := NewLocator(0, name)
	if err != nil {
		t.Fatal(err)
	}
	source, err := NewSourceIdentity([]byte{identity})
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewDirectoryNodeRecord(directory, parent, name, locator, source, ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func stageBackendGeneration(
	t *testing.T,
	backend CatalogBackend,
	share ShareInstance,
	directory NodeRecord,
	generation DirectoryGeneration,
	children []NodeRecord,
	meter ResourceMeter,
) BackendTransaction {
	t.Helper()
	directoryID, ok := directory.DirectoryID()
	if !ok {
		t.Fatal("backend fixture directory is not a directory")
	}
	transaction, err := backend.BeginDirectory(context.Background(), directoryID, generation, meter)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutDirectory(directory); err != nil {
		t.Fatal(err)
	}
	entries := make([]Entry, len(children))
	for index, child := range children {
		if err := transaction.PutChild(child); err != nil {
			t.Fatal(err)
		}
		entries[index] = child.Entry()
	}
	page, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: share, DirectoryID: directoryID, Generation: generation,
		Entries: entries, Terminal: true,
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutPage(page, mustTestPageObject(t, page)); err != nil {
		t.Fatal(err)
	}
	return transaction
}

func publishBackendGeneration(t *testing.T, transaction BackendTransaction) CommittedDirectory {
	t.Helper()
	if _, err := transaction.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed, err := transaction.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return committed
}

func TestMemoryBackendAtomicRaceRecoveryAndCorruptReplay(t *testing.T) {
	share := idValue[ShareInstance](1)
	backend := NewMemoryCatalogBackend()
	meter := backendTestMeter(t)
	defer meter.Close()
	directoryID := idValue[DirectoryID](31)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](30), "memory", 1)
	child, err := scannedFile(t, 32, "child", 4).nodeRecord(directoryID)
	if err != nil {
		t.Fatal(err)
	}
	generation := idValue[DirectoryGeneration](33)
	committed := publishBackendGeneration(t, stageBackendGeneration(
		t, backend, share, directory, generation, []NodeRecord{child}, meter,
	))
	usage, err := backend.Recover(context.Background())
	if err != nil || usage.Entries != 1 || usage.MemoryBytes == 0 {
		t.Fatalf("memory recovery usage = %+v err=%v", usage, err)
	}
	loaded, ok, err := backend.LoadDirectory(context.Background(), directoryID)
	if err != nil || !ok || loaded != committed {
		t.Fatalf("memory directory = %+v ok=%v err=%v", loaded, ok, err)
	}
	if _, ok, err := backend.LoadPage(context.Background(), directoryID, idValue[DirectoryGeneration](99), 0); err != nil || ok {
		t.Fatalf("wrong-generation page = ok=%v err=%v", ok, err)
	}
	if _, ok, err := backend.LoadPage(context.Background(), directoryID, generation, 9); err != nil || ok {
		t.Fatalf("unknown page = ok=%v err=%v", ok, err)
	}
	if _, ok, err := backend.LoadNode(context.Background(), idValue[NodeID](99)); err != nil || ok {
		t.Fatalf("unknown node = ok=%v err=%v", ok, err)
	}

	backend.mu.Lock()
	savedDirectory := backend.directories[directoryID]
	corruptDirectory := savedDirectory
	corruptDirectory.pages = clonePageMap(savedDirectory.pages)
	corruptDirectory.pages[0] = []byte{0xff}
	backend.directories[directoryID] = corruptDirectory
	backend.mu.Unlock()
	if _, _, err := backend.LoadPage(context.Background(), directoryID, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt memory page = %v", err)
	}
	backend.mu.Lock()
	backend.directories[directoryID] = savedDirectory
	savedNode := backend.nodes[child.NodeID()]
	backend.nodes[child.NodeID()] = []byte{0xff}
	backend.mu.Unlock()
	if _, _, err := backend.LoadNode(context.Background(), child.NodeID()); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt memory node = %v", err)
	}
	backend.mu.Lock()
	backend.nodes[child.NodeID()] = savedNode
	backend.mu.Unlock()

	replayMeter := backendTestMeter(t)
	defer replayMeter.Close()
	replay := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{child}, replayMeter)
	preparation, err := replay.Prepare(context.Background())
	if err != nil || !preparation.Existing {
		t.Fatalf("memory replay prepare = %+v err=%v", preparation, err)
	}
	if replayed, err := replay.Publish(context.Background()); err != nil || replayed != committed {
		t.Fatalf("memory replay publish = %+v err=%v", replayed, err)
	}

	conflictChild, _ := scannedFile(t, 34, "changed", 5).nodeRecord(directoryID)
	conflictMeter := backendTestMeter(t)
	defer conflictMeter.Close()
	conflict := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{conflictChild}, conflictMeter)
	if _, err := conflict.Prepare(context.Background()); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("memory existing conflict = %v", err)
	}
	_ = conflict.Abort()

	testMemoryPublishRaces(t, backend, share)
	testMemoryForeignNodeCollision(t, backend, share, child)

	backend.mu.Lock()
	first := backend.directories[directoryID]
	first.usage = ResourceUsage{SpillBytes: math.MaxUint64}
	backend.directories[directoryID] = first
	backend.directories[idValue[DirectoryID](35)] = memoryDirectory{usage: ResourceUsage{SpillBytes: 1}}
	backend.mu.Unlock()
	if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("overflowing recovered usage = %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadPage(context.Background(), directoryID, generation, 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory page load = %v", err)
	}
	if _, _, err := backend.LoadNode(context.Background(), child.NodeID()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory node load = %v", err)
	}
}

func testMemoryPublishRaces(t *testing.T, backend *MemoryCatalogBackend, share ShareInstance) {
	t.Helper()
	for _, different := range []bool{false, true} {
		directoryID := idValue[DirectoryID](byte(40 + boolIndex(different)))
		directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](39), "race", 2)
		generation := idValue[DirectoryGeneration](byte(42 + boolIndex(different)))
		firstChild, _ := scannedFile(t, 44, "same", 1).nodeRecord(directoryID)
		secondChild := firstChild
		if different {
			secondChild, _ = scannedFile(t, 45, "different", 2).nodeRecord(directoryID)
		}
		firstMeter := backendTestMeter(t)
		secondMeter := backendTestMeter(t)
		first := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{firstChild}, firstMeter)
		second := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{secondChild}, secondMeter)
		if _, err := first.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Publish(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err := first.Publish(context.Background())
		if different && !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("memory publish race conflict = %v", err)
		}
		if !different && err != nil {
			t.Fatalf("memory idempotent publish race = %v", err)
		}
		_ = first.Abort()
		firstMeter.Close()
		secondMeter.Close()
	}
}

func testMemoryForeignNodeCollision(t *testing.T, backend *MemoryCatalogBackend, share ShareInstance, existing NodeRecord) {
	t.Helper()
	directoryID := idValue[DirectoryID](50)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](49), "foreign", 3)
	child := existing
	child.parent = directoryID
	child.name = "foreign-name"
	meter := backendTestMeter(t)
	defer meter.Close()
	transaction := stageBackendGeneration(
		t, backend, share, directory, idValue[DirectoryGeneration](51), []NodeRecord{child}, meter,
	)
	if _, err := transaction.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Publish(context.Background()); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("memory foreign node collision = %v", err)
	}
	_ = transaction.Abort()
}

func boolIndex(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func TestBudgetReservationsRollbackRetainAndRejectInvalidTransitions(t *testing.T) {
	limits := BudgetLimits{ActiveScans: 4, ScanWork: 4, Entries: 4, MemoryBytes: 4, SpillBytes: 4}
	process, _ := NewBudgetAccount("process", limits)
	share, _ := NewBudgetAccount("share", BudgetLimits{ActiveScans: 1, ScanWork: 1, Entries: 1, MemoryBytes: 1, SpillBytes: 1})
	session, _ := NewBudgetAccount("session", limits)
	if _, ok := addUsage(ResourceUsage{MemoryBytes: math.MaxUint64}, ResourceUsage{MemoryBytes: 1}); ok {
		t.Fatal("overflowing resource usage was accepted")
	}
	var nilAccount *BudgetAccount
	if err := nilAccount.add(ResourceUsage{}); err == nil {
		t.Fatal("nil budget account accepted usage")
	}
	if (*BudgetReservation)(nil).active() {
		t.Fatal("nil reservation reported active")
	}
	if err := (*BudgetReservation)(nil).Grow(ResourceUsage{}); err == nil {
		t.Fatal("nil reservation grew")
	}
	if err := (*BudgetReservation)(nil).Shrink(ResourceUsage{}); err == nil {
		t.Fatal("nil reservation shrank")
	}
	if err := (*BudgetReservation)(nil).keep(ResourceUsage{}); err == nil {
		t.Fatal("nil reservation retained usage")
	}
	if err := (*BudgetReservation)(nil).dropAccount(process); err == nil {
		t.Fatal("nil reservation dropped an account")
	}
	if _, err := reserveAccounts(nil, ResourceUsage{}); err == nil {
		t.Fatal("empty hierarchy was accepted")
	}

	reservation, err := ReserveHierarchy(BudgetHierarchy{Process: process, Share: share, Session: session}, ResourceUsage{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Grow(ResourceUsage{MemoryBytes: 2}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("hierarchy overflow = %v", err)
	}
	if process.Snapshot().Used != (ResourceUsage{}) || session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("failed hierarchical growth was not rolled back atomically")
	}
	if err := reservation.Grow(ResourceUsage{MemoryBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Shrink(ResourceUsage{MemoryBytes: 2}); err == nil {
		t.Fatal("oversized shrink succeeded")
	}
	if err := reservation.keep(ResourceUsage{MemoryBytes: 2}); err == nil {
		t.Fatal("oversized retained usage succeeded")
	}
	other, _ := NewBudgetAccount("other", limits)
	if err := reservation.dropAccount(other); err == nil {
		t.Fatal("foreign account was dropped")
	}
	reservation.Release()
	if reservation.active() {
		t.Fatal("released reservation remained active")
	}
	if err := reservation.Grow(ResourceUsage{}); err == nil {
		t.Fatal("released reservation grew")
	}
	if err := reservation.Shrink(ResourceUsage{}); err == nil {
		t.Fatal("released reservation shrank")
	}
	if err := reservation.keep(ResourceUsage{}); err == nil {
		t.Fatal("released reservation retained usage")
	}
	if err := reservation.dropAccount(process); err == nil {
		t.Fatal("released reservation dropped an account")
	}
	reservation.Release()
	(*BudgetReservation)(nil).Release()
}

func TestFileSpillPropagatesFilesystemFailuresAndCommitCloseRace(t *testing.T) {
	share := idValue[ShareInstance](61)
	attempt := idValue[ScanAttemptID](62)
	blockingFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	brokenFactory := NewFileSpillFactory(filepath.Join(blockingFile, "spill"))
	if err := brokenFactory.Recover(context.Background(), share); err == nil {
		t.Fatal("spill recovery ignored an unusable parent")
	}
	if _, err := brokenFactory.NewWorkspace(context.Background(), SpillRequest{ShareInstance: share, AttemptID: attempt}); err == nil {
		t.Fatal("spill workspace ignored an unusable parent")
	}

	factory := NewFileSpillFactory(t.TempDir())
	if err := factory.Recover(context.Background(), share); err != nil {
		t.Fatal(err)
	}
	workspaceValue, err := factory.NewWorkspace(context.Background(), SpillRequest{ShareInstance: share, AttemptID: attempt})
	if err != nil {
		t.Fatal(err)
	}
	workspace := workspaceValue.(*fileSpillWorkspace)
	writerValue, err := workspace.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writer := writerValue.(*fileSpillWriter)
	if _, err := writer.Write([]byte("pending")); err != nil {
		t.Fatal(err)
	}
	// Model the synchronization point after Close marks the workspace closed.
	// Keeping the writer handle open is intentional: on Windows the filesystem
	// removal phase cannot complete until Commit observes the closed state.
	workspace.mu.Lock()
	workspace.closed = true
	workspace.mu.Unlock()
	if _, err := writer.Commit(); err == nil {
		t.Fatal("writer committed after workspace closure")
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}

	workspaceValue, err = factory.NewWorkspace(context.Background(), SpillRequest{ShareInstance: share, AttemptID: idValue[ScanAttemptID](63)})
	if err != nil {
		t.Fatal(err)
	}
	workspace = workspaceValue.(*fileSpillWorkspace)
	if err := os.RemoveAll(workspace.path); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Create(context.Background()); err == nil {
		t.Fatal("spill writer was created in a missing workspace")
	}

	abortFile, err := os.CreateTemp(t.TempDir(), "abort-")
	if err != nil {
		t.Fatal(err)
	}
	nonEmpty := filepath.Join(t.TempDir(), "non-empty")
	if err := os.Mkdir(nonEmpty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manualWriter := &fileSpillWriter{file: abortFile, path: nonEmpty}
	if err := manualWriter.Abort(); err == nil {
		t.Fatal("spill abort hid its cleanup failure")
	}

	objectWorkspace := &fileSpillWorkspace{objects: make(map[*fileSpillObject]struct{})}
	missingObject := &fileSpillObject{workspace: objectWorkspace, path: filepath.Join(t.TempDir(), "missing")}
	if _, err := missingObject.Open(context.Background()); err == nil {
		t.Fatal("missing spill object opened")
	}
	failingObject := &fileSpillObject{workspace: objectWorkspace, path: nonEmpty}
	objectWorkspace.objects[failingObject] = struct{}{}
	if err := failingObject.Remove(); err == nil {
		t.Fatal("spill object cleanup hid a non-empty directory")
	}
}

func TestFileBackendPublishRacesForeignCollisionsAndClosedPublication(t *testing.T) {
	root := t.TempDir()
	share := idValue[ShareInstance](70)
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Destroy()

	for _, different := range []bool{false, true} {
		directoryID := idValue[DirectoryID](byte(71 + boolIndex(different)))
		directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](70), "race", 1)
		generation := idValue[DirectoryGeneration](byte(73 + boolIndex(different)))
		childSeed := byte(75 + 2*boolIndex(different))
		firstChild, _ := scannedFile(t, childSeed, "same", 1).nodeRecord(directoryID)
		secondChild := firstChild
		if different {
			secondChild, _ = scannedFile(t, childSeed+1, "different", 2).nodeRecord(directoryID)
		}
		firstMeter := backendTestMeter(t)
		secondMeter := backendTestMeter(t)
		first := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{firstChild}, firstMeter)
		second := stageBackendGeneration(t, backend, share, directory, generation, []NodeRecord{secondChild}, secondMeter)
		if _, err := first.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Prepare(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Publish(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err = first.Publish(context.Background())
		if different && !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("file publish race conflict = %v", err)
		}
		if !different && err != nil {
			t.Fatalf("file idempotent publish race = %v", err)
		}
		_ = first.Abort()
		firstMeter.Close()
		secondMeter.Close()
	}

	baseDirectoryID := idValue[DirectoryID](80)
	baseDirectory := backendDirectoryRecord(t, baseDirectoryID, idValue[DirectoryID](79), "base", 2)
	baseChild, _ := scannedFile(t, 81, "base-child", 1).nodeRecord(baseDirectoryID)
	baseMeter := backendTestMeter(t)
	publishBackendGeneration(t, stageBackendGeneration(
		t, backend, share, baseDirectory, idValue[DirectoryGeneration](82), []NodeRecord{baseChild}, baseMeter,
	))
	baseMeter.Close()

	foreignDirectoryID := idValue[DirectoryID](83)
	foreignDirectory := backendDirectoryRecord(t, foreignDirectoryID, idValue[DirectoryID](79), "foreign", 3)
	foreignChild := baseChild
	foreignChild.parent = foreignDirectoryID
	foreignChild.name = "foreign-child"
	foreignMeter := backendTestMeter(t)
	foreign := stageBackendGeneration(
		t, backend, share, foreignDirectory, idValue[DirectoryGeneration](84), []NodeRecord{foreignChild}, foreignMeter,
	)
	if _, err := foreign.Prepare(context.Background()); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("file foreign node collision = %v", err)
	}
	_ = foreign.Abort()
	foreignMeter.Close()

	closedDirectoryID := idValue[DirectoryID](85)
	closedDirectory := backendDirectoryRecord(t, closedDirectoryID, idValue[DirectoryID](79), "closed", 4)
	closedChild, _ := scannedFile(t, 86, "closed-child", 1).nodeRecord(closedDirectoryID)
	closedMeter := backendTestMeter(t)
	closed := stageBackendGeneration(
		t, backend, share, closedDirectory, idValue[DirectoryGeneration](87), []NodeRecord{closedChild}, closedMeter,
	)
	if _, err := closed.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := closed.Publish(context.Background()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("publication after backend close = %v", err)
	}
	_ = closed.Abort()
	closedMeter.Close()
	if _, _, err := backend.LoadPage(context.Background(), closedDirectoryID, idValue[DirectoryGeneration](87), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed file page load = %v", err)
	}
	if _, _, err := backend.LoadNode(context.Background(), baseChild.NodeID()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed file node load = %v", err)
	}
}

func TestFileBackendRecoveryRejectsSemanticAndPhysicalCorruption(t *testing.T) {
	mutations := map[string]func(*testing.T, string, *FileCatalogBackend, fileCatalogMeta){
		"unexpected committed object": func(t *testing.T, _ string, backend *FileCatalogBackend, _ fileCatalogMeta) {
			if err := os.WriteFile(filepath.Join(backend.committedDir, "unexpected"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"share identity": func(t *testing.T, path string, _ *FileCatalogBackend, meta fileCatalogMeta) {
			meta.share = idValue[ShareInstance](99)
			writeBackendMeta(t, path, meta)
		},
		"directory identity": func(t *testing.T, path string, _ *FileCatalogBackend, meta fileCatalogMeta) {
			meta.directory = idValue[DirectoryID](99)
			writeBackendMeta(t, path, meta)
		},
		"directory record": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			if err := os.WriteFile(filepath.Join(path, fileCatalogDirectoryName), []byte{0xff}, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing page": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			if err := os.Remove(filepath.Join(path, "pages", "00000000.page")); err != nil {
				t.Fatal(err)
			}
		},
		"missing page object": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			if err := os.Remove(filepath.Join(path, fileCatalogObjectsName, "00000000.object")); err != nil {
				t.Fatal(err)
			}
		},
		"conflicting page object": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			objectPath := filepath.Join(path, fileCatalogObjectsName, "00000000.object")
			object, err := os.ReadFile(objectPath)
			if err != nil {
				t.Fatal(err)
			}
			object[0] ^= 0xff
			if err := os.WriteFile(objectPath, object, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"truncated child frame": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			if err := os.WriteFile(filepath.Join(path, fileCatalogChildrenName), []byte{0, 0}, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"extra child frame": func(t *testing.T, path string, _ *FileCatalogBackend, _ fileCatalogMeta) {
			file, err := os.OpenFile(filepath.Join(path, fileCatalogChildrenName), os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte{0, 0, 0, 1, 0xff}); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"committed geometry": func(t *testing.T, path string, _ *FileCatalogBackend, meta fileCatalogMeta) {
			meta.entryCount++
			writeBackendMeta(t, path, meta)
		},
		"digest": func(t *testing.T, path string, _ *FileCatalogBackend, meta fileCatalogMeta) {
			meta.digest[0] ^= 0xff
			writeBackendMeta(t, path, meta)
		},
		"physical size": func(t *testing.T, path string, _ *FileCatalogBackend, meta fileCatalogMeta) {
			meta.spillBytes++
			writeBackendMeta(t, path, meta)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			root, backend, path, meta := durableRecoveryFixture(t)
			mutate(t, path, backend, meta)
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: idValue[ShareInstance](90)})
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Destroy()
			if _, err := recovered.Recover(context.Background()); err == nil {
				t.Fatal("corrupt durable generation was accepted")
			}
		})
	}
}

func durableRecoveryFixture(t *testing.T) (string, *FileCatalogBackend, string, fileCatalogMeta) {
	t.Helper()
	root := t.TempDir()
	share := idValue[ShareInstance](90)
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	directoryID := idValue[DirectoryID](91)
	directory := backendDirectoryRecord(t, directoryID, idValue[DirectoryID](89), "durable", 1)
	child, _ := scannedFile(t, 92, "child", 1).nodeRecord(directoryID)
	meter := backendTestMeter(t)
	publishBackendGeneration(t, stageBackendGeneration(
		t, backend, share, directory, idValue[DirectoryGeneration](93), []NodeRecord{child}, meter,
	))
	meter.Close()
	path := backend.directoryPath(directoryID)
	meta, err := readFileCatalogMeta(filepath.Join(path, fileCatalogMetaName))
	if err != nil {
		t.Fatal(err)
	}
	return root, backend, path, meta
}

func writeBackendMeta(t *testing.T, path string, meta fileCatalogMeta) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(path, fileCatalogMetaName), encodeFileCatalogMeta(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFileBackendLoadRejectsCorruptMetadataPageAndNodeStream(t *testing.T) {
	root, backend, path, meta := durableRecoveryFixture(t)
	defer backend.Destroy()
	directory := meta.directory
	generation := meta.generation
	metaPath := filepath.Join(path, fileCatalogMetaName)
	originalMeta, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metaPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadDirectory(context.Background(), directory); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt load metadata = %v", err)
	}
	if _, _, err := backend.LoadPage(context.Background(), directory, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt page metadata = %v", err)
	}
	if err := os.WriteFile(metaPath, originalMeta, 0o600); err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(path, "pages", "00000000.page")
	originalPage, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pagePath, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadPage(context.Background(), directory, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt page payload = %v", err)
	}
	otherPage, err := NewCatalogPage(CatalogPageSpec{
		ShareInstance: meta.share, DirectoryID: idValue[DirectoryID](94), Generation: generation, Terminal: true,
	}, semanticTestCommitter{})
	if err != nil {
		t.Fatal(err)
	}
	encodedOther, _ := encodeCatalogPage(otherPage)
	if err := os.WriteFile(pagePath, encodedOther, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadPage(context.Background(), directory, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("misaddressed page = %v", err)
	}
	if err := os.WriteFile(pagePath, originalPage, 0o600); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(path, fileCatalogObjectsName, "00000000.object")
	originalObject, err := os.ReadFile(objectPath)
	if err != nil {
		t.Fatal(err)
	}
	conflictingObject := append([]byte(nil), originalObject...)
	conflictingObject[0] ^= 0xff
	if err := os.WriteFile(objectPath, conflictingObject, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadPageObject(context.Background(), directory, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("conflicting page object = %v", err)
	}
	if err := os.WriteFile(objectPath, originalObject, 0o600); err != nil {
		t.Fatal(err)
	}
	childrenPath := filepath.Join(path, fileCatalogChildrenName)
	if err := os.WriteFile(childrenPath, []byte{0, 0, 0, 1, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadNode(context.Background(), idValue[NodeID](92)); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt node stream = %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestFileMetadataValidationRejectsEveryIdentityAndGeometryInvariant(t *testing.T) {
	valid := fileCatalogMeta{
		share: idValue[ShareInstance](1), directory: idValue[DirectoryID](2), generation: idValue[DirectoryGeneration](3),
		pageCount: 1, entryCount: 1, terminal: mustPageCommitment(t, bytes.Repeat([]byte{1}, PageCommitmentBytes)),
		spillBytes: fileCatalogMetaBytes,
	}
	mutations := []func([]byte){
		func(encoded []byte) { encoded[0] ^= 0xff },
		func(encoded []byte) { binary.BigEndian.PutUint16(encoded[4:6], catalogStorageSchema+1) },
		func(encoded []byte) { clear(encoded[8:24]) },
		func(encoded []byte) { clear(encoded[24:40]) },
		func(encoded []byte) { clear(encoded[40:56]) },
		func(encoded []byte) { clear(encoded[76:108]) },
		func(encoded []byte) { binary.BigEndian.PutUint32(encoded[56:60], 0) },
		func(encoded []byte) { binary.BigEndian.PutUint32(encoded[56:60], 2) },
		func(encoded []byte) { binary.BigEndian.PutUint64(encoded[60:68], MaxDirectoryEntries+1) },
		func(encoded []byte) {
			binary.BigEndian.PutUint64(encoded[60:68], MaxDirectoryEntries)
			binary.BigEndian.PutUint64(encoded[68:76], 1)
		},
	}
	for index, mutate := range mutations {
		encoded := encodeFileCatalogMeta(valid)
		mutate(encoded)
		if _, err := decodeFileCatalogMeta(encoded); !errors.Is(err, ErrCorruptCatalogStorage) {
			t.Fatalf("invalid metadata %d = %v", index, err)
		}
	}
}
