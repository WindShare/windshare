package catalog

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

type mutableFileFaults struct {
	mu    sync.Mutex
	point FileBackendFaultPoint
	err   error
}

func (f *mutableFileFaults) set(point FileBackendFaultPoint, err error) {
	f.mu.Lock()
	f.point = point
	f.err = err
	f.mu.Unlock()
}

func (f *mutableFileFaults) Fail(point FileBackendFaultPoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if point == f.point {
		return f.err
	}
	return nil
}

func wideFileID(index uint64) FileID {
	var id FileID
	binary.BigEndian.PutUint64(id[8:], index+1)
	return id
}

func wideScannedFile(t *testing.T, index int) ScannedChild {
	t.Helper()
	name := fmt.Sprintf("file-%06d", index)
	locator, err := NewLocator(0, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := NewSourceIdentity([]byte(fmt.Sprintf("source-%d", index)))
	candidate, _ := NewVersionCandidate([]byte(fmt.Sprintf("version-%d", index)))
	return ScannedChild{
		FileID: wideFileID(uint64(index)), Name: name, Locator: locator,
		SourceIdentity: identity, VersionCandidate: candidate, ExpectedSize: uint64(index),
	}
}

func fileStore(
	t *testing.T,
	root string,
	share ShareInstance,
	process, shareBudget *BudgetAccount,
	faults FileBackendFaults,
	attemptByte, generationByte byte,
) (*CatalogStore, *FileCatalogBackend) {
	t.Helper()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: root, ShareInstance: share, Faults: faults,
	})
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Uint32
	var generations atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, Backend: backend, ProcessBudget: process, ShareBudget: shareBudget,
		PageSealer: semanticTestCommitter{}, SpillFactory: NewFileSpillFactory(filepath.Join(root, "sort")),
		SortRunBytes: 1024,
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			id := idValue[ScanAttemptID](attemptByte)
			binary.BigEndian.PutUint32(id[12:], attempts.Add(1))
			return id, nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			id := idValue[DirectoryGeneration](generationByte)
			binary.BigEndian.PutUint32(id[12:], generations.Add(1))
			return id, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, backend
}

func fileBudget(t *testing.T, name string, entries uint64, memory, spill uint64) *BudgetAccount {
	t.Helper()
	account, err := NewBudgetAccount(name, BudgetLimits{
		ActiveScans: 32, ScanWork: entries * 4, Entries: entries,
		MemoryBytes: memory, SpillBytes: spill,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func replayPageBytes(t *testing.T, store *CatalogStore, directory CommittedDirectory) [][]byte {
	t.Helper()
	result := make([][]byte, directory.PageCount())
	for index := uint32(0); index < directory.PageCount(); index++ {
		page, ok, err := store.Page(context.Background(), directory.DirectoryID(), directory.Generation(), index)
		if err != nil || !ok {
			t.Fatalf("load page %d: ok=%v err=%v", index, ok, err)
		}
		result[index], err = encodeCatalogPage(page)
		if err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func replayPageObjects(t *testing.T, store *CatalogStore, directory CommittedDirectory) [][]byte {
	t.Helper()
	result := make([][]byte, directory.PageCount())
	for index := uint32(0); index < directory.PageCount(); index++ {
		object, ok, err := store.PageObject(
			context.Background(), directory.DirectoryID(), directory.Generation(), index,
		)
		if err != nil || !ok {
			t.Fatalf("load page object %d: ok=%v err=%v", index, ok, err)
		}
		result[index] = object.Bytes()
	}
	return result
}

func TestFileCatalogBackendWideSortReplayAndRecoveryAccounting(t *testing.T) {
	const childCount = 4_097
	rootPath := t.TempDir()
	share := idValue[ShareInstance](1)
	process := fileBudget(t, "process", childCount+100, 8<<20, 256<<20)
	shareBudget := fileBudget(t, "share", childCount+100, 8<<20, 256<<20)
	session := fileBudget(t, "session", childCount+100, 1<<20, 256<<20)
	store, backend := fileStore(t, rootPath, share, process, shareBudget, nil, 10, 20)
	root := idValue[DirectoryID](2)
	directory := idValue[DirectoryID](3)
	syntheticCommit := rootCommit(
		t, share, root, idValue[DirectoryGeneration](4),
		[]NodeRecord{selectedDirectory(t, root, 3, "wide")},
	)
	commitSyntheticRoot(t, store, syntheticCommit, session)

	var peakMemory atomic.Uint64
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := childCount - 1; index >= 0; index-- {
			if err := request.Children.Add(ctx, wideScannedFile(t, index)); err != nil {
				return ScanResult{}, err
			}
			used := session.Snapshot().Used.MemoryBytes
			for {
				previous := peakMemory.Load()
				if used <= previous || peakMemory.CompareAndSwap(previous, used) {
					break
				}
			}
		}
		return ScanResult{}, nil
	})
	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	if committed.EntryCount() != childCount ||
		committed.PageCount() != uint32((childCount+MaxCatalogPageEntries-1)/MaxCatalogPageEntries) {
		t.Fatalf("wide directory geometry = %+v", committed)
	}
	if peakMemory.Load() > session.Snapshot().Limits.MemoryBytes {
		t.Fatalf("sort memory peak %d exceeded limit", peakMemory.Load())
	}
	before := replayPageBytes(t, store, committed)
	beforeObjects := replayPageObjects(t, store, committed)
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("completed scan retained session usage: %+v", session.Snapshot().Used)
	}
	expectedSpill, err := directoryTreeBytes(filepath.Join(rootPath, "committed"))
	if err != nil {
		t.Fatal(err)
	}
	if shareBudget.Snapshot().Used.SpillBytes != expectedSpill {
		t.Fatalf("share spill accounting = %d, disk = %d", shareBudget.Snapshot().Used.SpillBytes, expectedSpill)
	}

	// Closing the backend models process handle eviction while preserving the
	// durable share. A new store must re-admit every committed byte before use.
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredProcess := fileBudget(t, "recovered-process", childCount+100, 8<<20, 256<<20)
	recoveredShare := fileBudget(t, "recovered-share", childCount+100, 8<<20, 256<<20)
	recovered, _ := fileStore(t, rootPath, share, recoveredProcess, recoveredShare, nil, 30, 40)
	recoveredSession := fileBudget(t, "recovered-session", childCount+100, 1<<20, 256<<20)
	if rootAuthority := commitSyntheticRoot(t, recovered, syntheticCommit, recoveredSession); rootAuthority.IsZero() {
		t.Fatal("restart did not restore committed-root registration authority")
	}
	afterDirectory, ok, err := recovered.Directory(context.Background(), directory)
	if err != nil || !ok || afterDirectory != committed {
		t.Fatalf("recovered directory = %+v ok=%v err=%v", afterDirectory, ok, err)
	}
	after := replayPageBytes(t, recovered, afterDirectory)
	afterObjects := replayPageObjects(t, recovered, afterDirectory)
	if len(before) != len(after) {
		t.Fatal("restart changed page count")
	}
	for index := range before {
		if !bytes.Equal(before[index], after[index]) {
			t.Fatalf("page %d changed bytes after restart", index)
		}
		if !bytes.Equal(beforeObjects[index], afterObjects[index]) {
			t.Fatalf("sealed page object %d changed bytes after restart", index)
		}
	}
	node, ok, err := recovered.Node(context.Background(), wideFileID(childCount/2).NodeID())
	if err != nil || !ok || node.Entry().Name() != fmt.Sprintf("file-%06d", childCount/2) {
		t.Fatalf("recovered private node = %+v ok=%v err=%v", node, ok, err)
	}
	if recoveredShare.Snapshot().Used.Entries != childCount+1 ||
		recoveredShare.Snapshot().Used.SpillBytes != expectedSpill {
		t.Fatalf("recovered usage = %+v", recoveredShare.Snapshot().Used)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileCatalogBackendFaultsNeverPublishHalfGeneration(t *testing.T) {
	for _, point := range []FileBackendFaultPoint{
		FileFaultStageDirectory, FileFaultStageChild, FileFaultStagePage, FileFaultStagePageObject,
		FileFaultPrepare, FileFaultPublish,
	} {
		t.Run(string(point), func(t *testing.T) {
			rootPath := t.TempDir()
			faults := &mutableFileFaults{}
			process := generousBudget(t, "process")
			shareBudget := generousBudget(t, "share")
			session := generousBudget(t, "session")
			store, backend := fileStore(t, rootPath, idValue[ShareInstance](1), process, shareBudget, faults, 50, 60)
			defer store.Close()
			root := idValue[DirectoryID](5)
			directory := idValue[DirectoryID](6)
			commitSyntheticRoot(t, store, rootCommit(
				t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](7),
				[]NodeRecord{selectedDirectory(t, root, 6, "faulted")},
			), session)
			baseline := shareBudget.Snapshot().Used
			faultErr := errors.New("injected disk fault")
			if point == FileFaultStagePage || point == FileFaultStagePageObject {
				faultErr = syscall.ENOSPC
			}
			faults.set(point, faultErr)
			scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
				for index := 0; index < 3; index++ {
					if err := request.Children.Add(ctx, wideScannedFile(t, index)); err != nil {
						return ScanResult{}, err
					}
				}
				return ScanResult{}, nil
			})
			_, scanErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
			if scanErr == nil {
				t.Fatal("injected backend fault was accepted")
			} else if (point == FileFaultStagePage || point == FileFaultStagePageObject) && !errors.Is(scanErr, syscall.ENOSPC) {
				t.Fatalf("disk-full cause was lost: %v", scanErr)
			}
			var failure *DirectoryFailure
			if !errors.As(scanErr, &failure) {
				t.Fatalf("failed scan did not publish durable failure authority: %v", scanErr)
			}
			if _, exists, err := store.Directory(context.Background(), directory); err != nil || exists {
				t.Fatalf("fault published directory: exists=%v err=%v", exists, err)
			}
			transactions, err := os.ReadDir(filepath.Join(rootPath, "transactions"))
			if err != nil || len(transactions) != 0 {
				t.Fatalf("failed transaction was not cleaned: entries=%d err=%v", len(transactions), err)
			}
			failureBytes, err := directoryTreeBytes(backend.failurePath(directory, failure.AttemptID))
			if err != nil {
				t.Fatal(err)
			}
			wantAfterFailure, _ := addUsage(baseline, ResourceUsage{
				MemoryBytes: ScanAttemptLedgerBytes, SpillBytes: failureBytes,
			})
			if shareBudget.Snapshot().Used != wantAfterFailure || session.Snapshot().Used != (ResourceUsage{}) {
				t.Fatalf("fault leaked budget: share=%+v session=%+v", shareBudget.Snapshot().Used, session.Snapshot().Used)
			}
		})
	}
}

func TestFileCatalogCancellationCleansStagingAndSortRuns(t *testing.T) {
	rootPath := t.TempDir()
	process := generousBudget(t, "process")
	shareBudget := generousBudget(t, "share")
	session := generousBudget(t, "session")
	store, _ := fileStore(t, rootPath, idValue[ShareInstance](1), process, shareBudget, nil, 61, 62)
	defer store.Close()
	root := idValue[DirectoryID](21)
	directory := idValue[DirectoryID](22)
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](23),
		[]NodeRecord{selectedDirectory(t, root, 22, "cancelled")},
	), session)
	started := make(chan struct{})
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := 0; index < 100; index++ {
			if err := request.Children.Add(ctx, wideScannedFile(t, index)); err != nil {
				return ScanResult{}, err
			}
		}
		close(started)
		<-ctx.Done()
		return ScanResult{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.ListChildren(ctx, directory, session, ScanOptions{}, scanner)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled listing = %v", err)
	}
	store.scanWG.Wait()
	if _, exists, err := store.Directory(context.Background(), directory); err != nil || exists {
		t.Fatalf("cancelled generation became visible: exists=%v err=%v", exists, err)
	}
	transactions, err := os.ReadDir(filepath.Join(rootPath, "transactions"))
	if err != nil || len(transactions) != 0 {
		t.Fatalf("cancelled backend staging remained: entries=%d err=%v", len(transactions), err)
	}
	sortRoot := NewFileSpillFactory(filepath.Join(rootPath, "sort")).shareRoot(idValue[ShareInstance](1))
	sortEntries, err := os.ReadDir(sortRoot)
	if err != nil || len(sortEntries) != 0 {
		t.Fatalf("cancelled sort runs remained: entries=%d err=%v", len(sortEntries), err)
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("cancelled scan leaked session budget: %+v", session.Snapshot().Used)
	}
}

func TestFileCatalogRecoveryCleansCrashDebrisAndRejectsCorruption(t *testing.T) {
	rootPath := t.TempDir()
	share := idValue[ShareInstance](1)
	process := generousBudget(t, "process")
	shareBudget := generousBudget(t, "share")
	session := generousBudget(t, "session")
	store, backend := fileStore(t, rootPath, share, process, shareBudget, nil, 70, 80)
	root := idValue[DirectoryID](8)
	commitSyntheticRoot(t, store, rootCommit(
		t, share, root, idValue[DirectoryGeneration](9),
		[]NodeRecord{selectedDirectory(t, root, 10, "selected")},
	), session)
	abandoned := filepath.Join(rootPath, "transactions", "abandoned")
	if err := os.MkdirAll(abandoned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandoned, "partial"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	spillFactory := NewFileSpillFactory(filepath.Join(rootPath, "sort"))
	abandonedSort := filepath.Join(spillFactory.shareRoot(share), "abandoned")
	if err := os.MkdirAll(abandonedSort, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abandonedSort, "run"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	recoveredBackend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: rootPath, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	recoveredStore, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, Backend: recoveredBackend, ProcessBudget: generousBudget(t, "rp"),
		ShareBudget: generousBudget(t, "rs"), PageSealer: semanticTestCommitter{}, SpillFactory: spillFactory,
	})
	if err != nil {
		t.Fatal(err)
	}
	transactions, err := os.ReadDir(filepath.Join(rootPath, "transactions"))
	if err != nil || len(transactions) != 0 {
		t.Fatalf("recovery did not clean crash debris: entries=%d err=%v", len(transactions), err)
	}
	sortEntries, err := os.ReadDir(spillFactory.shareRoot(share))
	if err != nil || len(sortEntries) != 0 {
		t.Fatalf("recovery did not clean abandoned sort runs: entries=%d err=%v", len(sortEntries), err)
	}
	if err := recoveredBackend.Close(); err != nil {
		t.Fatal(err)
	}

	pagePath := filepath.Join(rootPath, "committed", fmt.Sprintf("%032x", root), "pages", "00000000.page")
	page, err := os.ReadFile(pagePath)
	if err != nil {
		t.Fatal(err)
	}
	page[len(page)-1] ^= 0xff
	if err := os.WriteFile(pagePath, page, 0o600); err != nil {
		t.Fatal(err)
	}
	corruptBackend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: rootPath, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCatalogStore(StoreConfig{
		ShareInstance: share, Backend: corruptBackend, ProcessBudget: generousBudget(t, "cp"),
		ShareBudget: generousBudget(t, "cs"), PageSealer: semanticTestCommitter{},
	}); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt committed page was accepted: %v", err)
	}
	_ = recoveredStore.Close()
	_ = store.Close()
}

type failingSpillFactory struct {
	err error
}

func (f failingSpillFactory) NewWorkspace(context.Context, SpillRequest) (SpillWorkspace, error) {
	return nil, f.err
}

func TestInjectedSpillFailureIsAtomicAndBudgetClean(t *testing.T) {
	process := generousBudget(t, "process")
	share := generousBudget(t, "share")
	session := generousBudget(t, "session")
	backend := NewMemoryCatalogBackend()
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: backend,
		ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{},
		SpillFactory: failingSpillFactory{err: errors.New("spill unavailable")},
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			return idValue[ScanAttemptID](1), nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			return idValue[DirectoryGeneration](2), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root := idValue[DirectoryID](11)
	directory := idValue[DirectoryID](12)
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](13),
		[]NodeRecord{selectedDirectory(t, root, 12, "child")},
	), session)
	baseline := share.Snapshot().Used
	scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		t.Fatal("scanner ran without spill admission")
		return ScanResult{}, nil
	})
	_, scanErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if scanErr == nil {
		t.Fatal("spill creation failure was accepted")
	}
	var failure *DirectoryFailure
	if !errors.As(scanErr, &failure) {
		t.Fatalf("spill failure did not publish durable failure authority: %v", scanErr)
	}
	object, found, err := store.FailureObject(context.Background(), directory, failure.AttemptID)
	if err != nil || !found {
		t.Fatalf("durable failure object: found=%v err=%v", found, err)
	}
	wantAfterFailure, _ := addUsage(baseline, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes + memoryObjectOverhead*2 + uint64(len(object.Bytes()))})
	if share.Snapshot().Used != wantAfterFailure || session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("spill failure leaked budget: share=%+v session=%+v", share.Snapshot().Used, session.Snapshot().Used)
	}
}

func TestExternalSortIsPermutationDeterministic(t *testing.T) {
	const entries = 321
	var reference [][]byte
	for seed := int64(0); seed < 6; seed++ {
		rootPath := t.TempDir()
		process := generousBudget(t, fmt.Sprintf("process-%d", seed))
		shareBudget := generousBudget(t, fmt.Sprintf("share-%d", seed))
		session := generousBudget(t, fmt.Sprintf("session-%d", seed))
		store, _ := fileStore(t, rootPath, idValue[ShareInstance](1), process, shareBudget, nil, 90, 91)
		root := idValue[DirectoryID](14)
		directory := idValue[DirectoryID](15)
		commitSyntheticRoot(t, store, rootCommit(
			t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](16),
			[]NodeRecord{selectedDirectory(t, root, 15, "directory")},
		), session)
		order := rand.New(rand.NewSource(seed)).Perm(entries)
		scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			for _, index := range order {
				if err := request.Children.Add(ctx, wideScannedFile(t, index)); err != nil {
					return ScanResult{}, err
				}
			}
			return ScanResult{}, nil
		})
		committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
		if err != nil {
			t.Fatal(err)
		}
		pages := replayPageBytes(t, store, committed)
		if reference == nil {
			reference = pages
		} else {
			for index := range reference {
				if !bytes.Equal(reference[index], pages[index]) {
					t.Fatalf("seed %d changed page %d", seed, index)
				}
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileBackendRejectsInvalidLifecycleAndCorruptFrames(t *testing.T) {
	if _, err := NewFileCatalogBackend(FileCatalogBackendConfig{}); err == nil {
		t.Fatal("empty file backend configuration was accepted")
	}
	rootPath := t.TempDir()
	share := idValue[ShareInstance](1)
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: rootPath, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	meter, err := newAttemptResourceMeter(BudgetHierarchy{
		Process: generousBudget(t, "process"),
		Share:   generousBudget(t, "share"),
		Session: generousBudget(t, "session"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer meter.Close()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.Recover(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled recovery = %v", err)
	}
	if _, err := backend.BeginDirectory(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled begin = %v", err)
	}
	if _, err := backend.BeginDirectory(context.Background(), DirectoryID{}, DirectoryGeneration{}, meter); err == nil {
		t.Fatal("identity-free transaction was accepted")
	}
	if _, err := backend.BeginDirectory(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), nil); err == nil {
		t.Fatal("meter-free transaction was accepted")
	}
	if _, _, err := backend.LoadDirectory(cancelled, idValue[DirectoryID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled directory load = %v", err)
	}
	if _, _, err := backend.LoadPage(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page load = %v", err)
	}
	if _, _, err := backend.LoadPageObject(cancelled, idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled page-object load = %v", err)
	}
	if _, _, err := backend.LoadNode(cancelled, idValue[NodeID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled node load = %v", err)
	}
	if _, ok, err := backend.LoadDirectory(context.Background(), idValue[DirectoryID](1)); err != nil || ok {
		t.Fatalf("unknown directory = ok=%v err=%v", ok, err)
	}
	if _, ok, err := backend.LoadPage(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); err != nil || ok {
		t.Fatalf("unknown page = ok=%v err=%v", ok, err)
	}
	if _, ok, err := backend.LoadPageObject(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); err != nil || ok {
		t.Fatalf("unknown page object = ok=%v err=%v", ok, err)
	}

	transaction, err := backend.BeginDirectory(
		context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutDirectory(NodeRecord{}); err == nil {
		t.Fatal("invalid directory record was staged")
	}
	if err := transaction.PutChild(NodeRecord{}); err == nil {
		t.Fatal("invalid child record was staged")
	}
	if err := transaction.PutPage(CatalogPage{}, SealedPageObject{}); err == nil {
		t.Fatal("invalid page was staged")
	}
	if _, err := transaction.Prepare(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prepare = %v", err)
	}
	if _, err := transaction.Publish(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publish = %v", err)
	}
	if _, err := transaction.Prepare(context.Background()); err == nil {
		t.Fatal("incomplete transaction prepared")
	}
	if _, err := transaction.Publish(context.Background()); err == nil {
		t.Fatal("unprepared transaction published")
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.PutDirectory(NodeRecord{}); err == nil {
		t.Fatal("aborted transaction accepted state")
	}
	if err := transaction.PutChild(NodeRecord{}); err == nil {
		t.Fatal("aborted transaction accepted child state")
	}
	if err := transaction.PutPage(CatalogPage{}, SealedPageObject{}); err == nil {
		t.Fatal("aborted transaction accepted page state")
	}
	if _, err := transaction.Prepare(context.Background()); err == nil {
		t.Fatal("aborted transaction prepared")
	}
	if _, err := transaction.Publish(context.Background()); err == nil {
		t.Fatal("aborted transaction published")
	}

	if _, err := decodeFileCatalogMeta(nil); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("empty metadata = %v", err)
	}
	meta := fileCatalogMeta{
		share: share, directory: idValue[DirectoryID](1), generation: idValue[DirectoryGeneration](2),
		pageCount: 1, terminal: mustPageCommitment(t, bytes.Repeat([]byte{1}, PageCommitmentBytes)),
		spillBytes: fileCatalogMetaBytes,
	}
	encodedMeta := encodeFileCatalogMeta(meta)
	encodedMeta[6] = 1
	if _, err := decodeFileCatalogMeta(encodedMeta); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("reserved metadata byte = %v", err)
	}
	if encoded, ok, err := readNodeFrame(bytes.NewReader(nil)); err != nil || ok || encoded != nil {
		t.Fatalf("empty node stream = %x ok=%v err=%v", encoded, ok, err)
	}
	if _, _, err := readNodeFrame(bytes.NewReader([]byte{0, 0, 0, 0})); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("zero node frame = %v", err)
	}
	if _, _, err := readNodeFrame(bytes.NewReader([]byte{0, 0})); err == nil {
		t.Fatal("truncated node frame was accepted")
	}
	cancelledReader, cancelReader := context.WithCancel(context.Background())
	cancelReader()
	if _, _, err := findNodeInFile(cancelledReader, bytes.NewReader(nil), idValue[NodeID](1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled node search = %v", err)
	}
	oversized := filepath.Join(rootPath, "oversized")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(maxCatalogStorageRecord + 1)); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := readCatalogObject(oversized); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("oversized catalog object = %v", err)
	}

	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed recovery = %v", err)
	}
	if _, err := backend.BeginDirectory(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), meter); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed begin = %v", err)
	}
	if _, _, err := backend.LoadDirectory(context.Background(), idValue[DirectoryID](1)); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed load = %v", err)
	}
	if _, _, err := backend.LoadPage(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page load = %v", err)
	}
	if _, _, err := backend.LoadPageObject(context.Background(), idValue[DirectoryID](1), idValue[DirectoryGeneration](2), 0); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page-object load = %v", err)
	}
	if err := backend.Destroy(); err != nil {
		t.Fatal(err)
	}
}
