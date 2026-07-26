package catalog

import (
	"context"
	"errors"
	"testing"
)

type contractReadBackend struct {
	CatalogBackend
	recover        func(context.Context) (ResourceUsage, error)
	replayFailures func(context.Context, func(DirectoryFailureRecord, bool) error) error
	loadDirectory  func(context.Context, DirectoryID) (CommittedDirectory, bool, error)
	loadPage       func(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error)
	loadPageObject func(context.Context, DirectoryID, DirectoryGeneration, uint32) (SealedPageObject, bool, error)
	loadNode       func(context.Context, NodeID) (NodeRecord, bool, error)
}

func (b contractReadBackend) Recover(ctx context.Context) (ResourceUsage, error) {
	if b.recover != nil {
		return b.recover(ctx)
	}
	return b.CatalogBackend.Recover(ctx)
}

func (b contractReadBackend) ReplayFailures(ctx context.Context, visit func(DirectoryFailureRecord, bool) error) error {
	if b.replayFailures != nil {
		return b.replayFailures(ctx, visit)
	}
	return b.CatalogBackend.ReplayFailures(ctx, visit)
}

func (b contractReadBackend) LoadDirectory(ctx context.Context, directory DirectoryID) (CommittedDirectory, bool, error) {
	if b.loadDirectory != nil {
		return b.loadDirectory(ctx, directory)
	}
	return b.CatalogBackend.LoadDirectory(ctx, directory)
}

func (b contractReadBackend) LoadPage(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (CatalogPage, bool, error) {
	if b.loadPage != nil {
		return b.loadPage(ctx, directory, generation, index)
	}
	return b.CatalogBackend.LoadPage(ctx, directory, generation, index)
}

func (b contractReadBackend) LoadPageObject(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, index uint32) (SealedPageObject, bool, error) {
	if b.loadPageObject != nil {
		return b.loadPageObject(ctx, directory, generation, index)
	}
	return b.CatalogBackend.LoadPageObject(ctx, directory, generation, index)
}

func (b contractReadBackend) LoadNode(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
	if b.loadNode != nil {
		return b.loadNode(ctx, id)
	}
	return b.CatalogBackend.LoadNode(ctx, id)
}

func TestAttemptResourceMeterOwnsReservationLifecycle(t *testing.T) {
	var nilMeter *attemptResourceMeter
	nilMeter.Close()
	if _, err := newAttemptResourceMeter(BudgetHierarchy{}); err == nil {
		t.Fatal("resource meter accepted a hierarchy without owners")
	}

	process := generousBudget(t, "meter-process")
	share := generousBudget(t, "meter-share")
	session := generousBudget(t, "meter-session")
	hierarchy := BudgetHierarchy{Process: process, Share: share, Session: session}
	meter, err := newAttemptResourceMeter(hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	if err := meter.reopen(hierarchy); err == nil {
		t.Fatal("an open meter accepted a second reservation")
	}
	if _, err := meter.retain(ResourceUsage{MemoryBytes: 1}, session); err == nil {
		t.Fatal("meter retained usage it had never reserved")
	}
	meter.Close()
	if err := meter.Consume(ResourceUsage{ScanWork: 1}); err == nil {
		t.Fatal("closed meter consumed work")
	}
	if err := meter.Release(ResourceUsage{}); err == nil {
		t.Fatal("closed meter released usage")
	}
	if _, err := meter.retain(ResourceUsage{}, session); err == nil {
		t.Fatal("closed meter transferred ownership")
	}
	if err := meter.reopen(BudgetHierarchy{}); err == nil {
		t.Fatal("meter reopened without budget owners")
	}
	if err := meter.reopen(hierarchy); err != nil {
		t.Fatalf("reopen after publication rollback: %v", err)
	}
	meter.Close()

	wrongSession := generousBudget(t, "meter-wrong-session")
	meter, err = newAttemptResourceMeter(hierarchy)
	if err != nil {
		t.Fatal(err)
	}
	if err := meter.Consume(ResourceUsage{MemoryBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := meter.retain(ResourceUsage{MemoryBytes: 1}, wrongSession); err == nil {
		t.Fatal("meter detached a budget account outside its hierarchy")
	}
	meter.Close()
}

func TestCommitAdmissionRejectsInvalidOwnershipAndClosedStores(t *testing.T) {
	parent, child := faultCommitFixture(t)
	generation := idValue[DirectoryGeneration](210)
	valid := DirectoryCommit{
		directory: parent, generation: generation, children: newSliceNodeSource([]NodeRecord{child}),
	}
	if err := validateDirectoryCommit(ShareInstance{}, valid, false); err == nil {
		t.Fatal("commit without a share identity was accepted")
	}
	if err := validateDirectoryCommit(idValue[ShareInstance](1), DirectoryCommit{
		directory: parent, generation: generation,
		children: faultNodeSource{count: MaxDirectoryEntries + 1},
	}, false); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("oversized commit error = %v", err)
	}
	if err := validateDirectoryCommit(idValue[ShareInstance](1), DirectoryCommit{
		directory: parent, generation: generation,
		children: faultNodeSource{count: 1}, omittedCount: MaxDirectoryEntries,
	}, false); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("entry plus omission overflow error = %v", err)
	}
	if err := validateDirectoryCommit(idValue[ShareInstance](1), DirectoryCommit{
		directory: parent, generation: generation, children: newSliceNodeSource(nil), synthetic: true,
	}, true); err == nil {
		t.Fatal("synthetic commit accepted a non-synthetic directory")
	}

	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	if _, err := store.CommitDirectory(context.Background(), valid, nil); err == nil {
		t.Fatal("directory commit without a session budget was admitted")
	}
	root := idValue[DirectoryID](211)
	synthetic := rootCommit(t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](212), []NodeRecord{
		selectedDirectory(t, root, 213, "selected"),
	})
	if _, err := store.CommitSyntheticRoot(context.Background(), synthetic, nil); err == nil {
		t.Fatal("synthetic-root commit without a startup budget was admitted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.CommitDirectory(cancelled, valid, generousBudget(t, "cancelled-session")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled commit error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitDirectory(context.Background(), valid, generousBudget(t, "closed-session")); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed directory commit error = %v", err)
	}
	if _, err := store.CommitSyntheticRoot(context.Background(), synthetic, generousBudget(t, "closed-startup")); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed synthetic-root commit error = %v", err)
	}
}

func TestExistingCommitReplayValidatesEveryDurableBinding(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	store, _, _ := newStore(t, backend, nil)
	defer store.Close()
	parent, child := faultCommitFixture(t)
	generation := idValue[DirectoryGeneration](214)
	commit := DirectoryCommit{
		directory: parent, generation: generation, children: newSliceNodeSource([]NodeRecord{child}),
	}
	existing, err := store.CommitDirectory(context.Background(), commit, generousBudget(t, "publish-session"))
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected durable read failure")
	otherChild, err := scannedFile(t, 215, "other", 1).nodeRecord(parent.directoryID)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		backend  CatalogBackend
		commit   DirectoryCommit
		existing CommittedDirectory
		want     error
	}{
		{
			name: "committed identity mismatch", backend: backend,
			commit: commit, existing: func() CommittedDirectory {
				value := existing
				value.shareInstance = idValue[ShareInstance](216)
				return value
			}(), want: ErrGenerationConflict,
		},
		{
			name: "directory read failure", backend: contractReadBackend{CatalogBackend: backend,
				loadNode: func(context.Context, NodeID) (NodeRecord, bool, error) { return NodeRecord{}, false, injected }},
			commit: commit, existing: existing, want: injected,
		},
		{
			name: "directory record missing", backend: contractReadBackend{CatalogBackend: backend,
				loadNode: func(context.Context, NodeID) (NodeRecord, bool, error) { return NodeRecord{}, false, nil }},
			commit: commit, existing: existing, want: ErrGenerationConflict,
		},
		{
			name: "source open failure", backend: backend,
			commit:   DirectoryCommit{directory: parent, generation: generation, children: faultNodeSource{count: 1, openErr: injected}},
			existing: existing, want: injected,
		},
		{
			name: "page read failure", backend: contractReadBackend{CatalogBackend: backend,
				loadPage: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error) {
					return CatalogPage{}, false, injected
				}},
			commit: commit, existing: existing, want: injected,
		},
		{
			name: "page object read failure", backend: contractReadBackend{CatalogBackend: backend,
				loadPageObject: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (SealedPageObject, bool, error) {
					return SealedPageObject{}, false, injected
				}},
			commit: commit, existing: existing, want: injected,
		},
		{
			name: "page missing", backend: contractReadBackend{CatalogBackend: backend,
				loadPage: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error) {
					return CatalogPage{}, false, nil
				}},
			commit: commit, existing: existing, want: ErrCorruptCatalogStorage,
		},
		{
			name: "source read failure", backend: backend,
			commit: DirectoryCommit{directory: parent, generation: generation,
				children: faultNodeSource{count: 1, iterator: &faultNodeIterator{nextErr: injected}}},
			existing: existing, want: injected,
		},
		{
			name: "entry mismatch", backend: backend,
			commit: DirectoryCommit{directory: parent, generation: generation,
				children: newSliceNodeSource([]NodeRecord{otherChild})},
			existing: existing, want: ErrGenerationConflict,
		},
		{
			name: "child read failure", backend: contractReadBackend{CatalogBackend: backend,
				loadNode: func(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
					if id == child.NodeID() {
						return NodeRecord{}, false, injected
					}
					return backend.LoadNode(ctx, id)
				}},
			commit: commit, existing: existing, want: injected,
		},
		{
			name: "child record missing", backend: contractReadBackend{CatalogBackend: backend,
				loadNode: func(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
					if id == child.NodeID() {
						return NodeRecord{}, false, nil
					}
					return backend.LoadNode(ctx, id)
				}},
			commit: commit, existing: existing, want: ErrGenerationConflict,
		},
		{
			name: "directory count contradicts page count", backend: backend,
			commit: commit, existing: func() CommittedDirectory {
				value := existing
				value.pageCount = 0
				return value
			}(), want: ErrCorruptCatalogStorage,
		},
		{
			name: "source exceeds declared count", backend: backend,
			commit: DirectoryCommit{directory: parent, generation: generation,
				children: faultNodeSource{count: 0, iterator: &faultNodeIterator{extra: &child}}},
			existing: CommittedDirectory{
				shareInstance: store.shareInstance, directoryID: parent.directoryID, generation: generation,
			}, want: ErrGenerationConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store.backend = test.backend
			_, got := store.validateExistingCommit(context.Background(), test.commit, test.existing, nil)
			if !errors.Is(got, test.want) {
				t.Fatalf("validation error = %v; want %v", got, test.want)
			}
		})
	}
	store.backend = backend
}

func TestCatalogReadAPIsRejectBackendIdentitySubstitution(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	store, _, _ := newStore(t, backend, nil)
	defer store.Close()
	injected := errors.New("injected page read failure")
	directory := idValue[DirectoryID](217)
	generation := idValue[DirectoryGeneration](218)
	object, err := NewSealedPageObject([]byte("object"))
	if err != nil {
		t.Fatal(err)
	}

	store.backend = contractReadBackend{CatalogBackend: backend,
		loadDirectory: func(context.Context, DirectoryID) (CommittedDirectory, bool, error) {
			return CommittedDirectory{shareInstance: store.shareInstance, directoryID: idValue[DirectoryID](219)}, true, nil
		},
	}
	if _, _, err := store.Directory(context.Background(), directory); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("substituted directory error = %v", err)
	}

	store.backend = contractReadBackend{CatalogBackend: backend,
		loadPage: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error) {
			return CatalogPage{shareInstance: store.shareInstance, directoryID: directory, generation: generation, pageIndex: 1}, true, nil
		},
	}
	if _, _, err := store.Page(context.Background(), directory, generation, 0); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("substituted page error = %v", err)
	}

	store.backend = contractReadBackend{CatalogBackend: backend,
		loadPageObject: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (SealedPageObject, bool, error) {
			return object, true, nil
		},
		loadPage: func(context.Context, DirectoryID, DirectoryGeneration, uint32) (CatalogPage, bool, error) {
			return CatalogPage{}, false, injected
		},
	}
	if _, _, err := store.PageObject(context.Background(), directory, generation, 0); !errors.Is(err, injected) {
		t.Fatalf("page-object authority read error = %v", err)
	}

	record := selectedDirectory(t, idValue[DirectoryID](220), 221, "substitute")
	store.backend = contractReadBackend{CatalogBackend: backend,
		loadNode: func(context.Context, NodeID) (NodeRecord, bool, error) { return record, true, nil },
	}
	if _, _, err := store.Node(context.Background(), idValue[NodeID](222)); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("substituted node error = %v", err)
	}
	store.backend = backend
}

type contractSpillFactory struct {
	recoverErr   error
	destroyErr   error
	workspace    SpillWorkspace
	workspaceErr error
}

func (f contractSpillFactory) Recover(context.Context, ShareInstance) error { return f.recoverErr }
func (f contractSpillFactory) Destroy(ShareInstance) error                  { return f.destroyErr }
func (f contractSpillFactory) NewWorkspace(context.Context, SpillRequest) (SpillWorkspace, error) {
	return f.workspace, f.workspaceErr
}

type contractScanWork func(uint64) error

func (work contractScanWork) Consume(units uint64) error { return work(units) }

func contractStoreConfig(t *testing.T, backend CatalogBackend, spill SpillFactory) StoreConfig {
	t.Helper()
	return StoreConfig{
		ShareInstance: idValue[ShareInstance](230), Backend: backend,
		ProcessBudget: generousBudget(t, "contract-process"), ShareBudget: generousBudget(t, "contract-share"),
		PageSealer: semanticTestCommitter{}, SpillFactory: spill,
	}
}

func contractBudget(t *testing.T, name string, memory uint64) *BudgetAccount {
	t.Helper()
	account, err := NewBudgetAccount(name, BudgetLimits{
		ActiveScans: 8, ScanWork: 8, Entries: 8, MemoryBytes: memory, SpillBytes: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestCatalogStoreRecoveryIsAtomicAcrossBackendSpillAndBudget(t *testing.T) {
	injected := errors.New("injected recovery failure")
	backend := NewMemoryCatalogBackend()
	config := contractStoreConfig(t, backend, contractSpillFactory{recoverErr: injected})
	if _, err := NewCatalogStore(config); !errors.Is(err, injected) {
		t.Fatalf("spill recovery error = %v", err)
	}

	config = contractStoreConfig(t, contractReadBackend{CatalogBackend: backend,
		recover: func(context.Context) (ResourceUsage, error) { return ResourceUsage{}, injected },
	}, contractSpillFactory{})
	if _, err := NewCatalogStore(config); !errors.Is(err, injected) {
		t.Fatalf("backend recovery error = %v", err)
	}

	config = contractStoreConfig(t, contractReadBackend{CatalogBackend: backend,
		recover: func(context.Context) (ResourceUsage, error) { return ResourceUsage{MemoryBytes: 2}, nil },
	}, contractSpillFactory{})
	config.ProcessBudget = contractBudget(t, "limited-process", 1)
	config.ShareBudget = contractBudget(t, "limited-share", 1)
	if _, err := NewCatalogStore(config); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("recovered-state admission error = %v", err)
	}

	process := generousBudget(t, "rollback-process")
	share := generousBudget(t, "rollback-share")
	config = contractStoreConfig(t, contractReadBackend{CatalogBackend: backend,
		recover: func(context.Context) (ResourceUsage, error) { return ResourceUsage{MemoryBytes: 1}, nil },
		replayFailures: func(context.Context, func(DirectoryFailureRecord, bool) error) error {
			return injected
		},
	}, contractSpillFactory{})
	config.ProcessBudget = process
	config.ShareBudget = share
	if _, err := NewCatalogStore(config); !errors.Is(err, injected) {
		t.Fatalf("failure-authority replay error = %v", err)
	}
	if process.Snapshot().Used != (ResourceUsage{}) || share.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("failed recovery retained recovered-state budget")
	}
}

func TestCatalogCloseCancelsUnfinishedAttemptsBeforeStorage(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &scanAttempt{ctx: attemptContext, cancel: cancel}
	store.attempts[idValue[DirectoryID](231)] = attempt
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(attemptContext), context.Canceled) {
		t.Fatal("store close left an unfinished scan attempt live")
	}
	store.completeAttempt(&scanAttempt{completed: true}, CommittedDirectory{}, nil)
}

func TestListChildrenPreservesKnownAuthorityAndReadFailureTaxonomy(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	store, _, _ := newStore(t, backend, nil)
	defer store.Close()
	session := generousBudget(t, "list-session")
	directory := prepareScannableDirectory(t, store, session, 232, 233)
	root := idValue[DirectoryID](232)
	never := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		t.Fatal("known or rejected directory invoked its scanner")
		return ScanResult{}, nil
	})
	if committed, err := store.ListChildren(context.Background(), root, session, ScanOptions{}, never); err != nil || committed.DirectoryID() != root {
		t.Fatalf("known directory replay = %+v, %v", committed, err)
	}

	injected := errors.New("injected catalog read failure")
	store.backend = contractReadBackend{CatalogBackend: backend,
		loadDirectory: func(context.Context, DirectoryID) (CommittedDirectory, bool, error) {
			return CommittedDirectory{}, false, injected
		},
	}
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, never); !errors.Is(err, injected) {
		t.Fatalf("directory read error = %v", err)
	}

	store.backend = contractReadBackend{CatalogBackend: backend,
		loadNode: func(context.Context, NodeID) (NodeRecord, bool, error) { return NodeRecord{}, false, injected },
	}
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, never); !errors.Is(err, injected) {
		t.Fatalf("node read error = %v", err)
	}

	wrong := selectedDirectory(t, root, 234, "wrong")
	store.backend = contractReadBackend{CatalogBackend: backend,
		loadNode: func(context.Context, NodeID) (NodeRecord, bool, error) { return wrong, true, nil },
	}
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, never); err == nil {
		t.Fatal("directory listing accepted a substituted node record")
	}

	store.backend = contractReadBackend{CatalogBackend: backend,
		loadNode: func(ctx context.Context, id NodeID) (NodeRecord, bool, error) {
			record, found, err := backend.LoadNode(ctx, id)
			store.mu.Lock()
			store.closed = true
			store.mu.Unlock()
			return record, found, err
		},
	}
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, never); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("close-after-read error = %v", err)
	}
	store.mu.Lock()
	store.closed = false
	store.mu.Unlock()
	store.backend = backend
}

func TestScanAdmissionAndChildSinkEnforceLocalBudgetAndContext(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	limitedSession := contractBudget(t, "limited-session", 1)
	if _, err := store.admitAttempt(idValue[DirectoryID](235), ScanAttemptID{}, limitedSession); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("session ledger admission error = %v", err)
	}

	config := contractStoreConfig(t, NewMemoryCatalogBackend(), contractSpillFactory{})
	config.ProcessBudget = contractBudget(t, "ledger-process", 1)
	config.ShareBudget = contractBudget(t, "ledger-share", 1)
	limitedStore, err := NewCatalogStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer limitedStore.Close()
	if _, err := limitedStore.admitAttempt(idValue[DirectoryID](236), ScanAttemptID{}, generousBudget(t, "ledger-session")); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("share ledger admission error = %v", err)
	}

	if err := (scanWorkMeter{}).Consume(0); err != nil {
		t.Fatalf("zero work changed resource state: %v", err)
	}
	store.mu.Lock()
	store.closed = true
	store.mu.Unlock()
	if err := (scanWorkMeter{store: store}).Consume(1); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed-store work error = %v", err)
	}
	store.mu.Lock()
	store.closed = false
	store.mu.Unlock()

	collector := &scanChildCollector{}
	if err := collector.Add(nil, ScannedChild{}); err == nil {
		t.Fatal("child sink accepted a nil context")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := collector.Add(cancelled, ScannedChild{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation error = %v", err)
	}
	collector.ctx = cancelled
	if err := collector.Add(context.Background(), ScannedChild{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("attempt cancellation error = %v", err)
	}

	collector = &scanChildCollector{
		ctx:    context.Background(),
		work:   contractScanWork(func(uint64) error { return nil }),
		sorter: &directorySorter{parent: idValue[DirectoryID](237)},
	}
	if err := collector.Add(context.Background(), ScannedChild{}); err == nil {
		t.Fatal("child sink accepted an identity-less child")
	}
}

func TestRunAttemptContainsAdmissionAndPostScanValidationFailures(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	session := generousBudget(t, "run-session")
	admission, err := ReserveHierarchy(store.hierarchy(session), ResourceUsage{ActiveScans: 1})
	if err != nil {
		t.Fatal(err)
	}
	resources, err := newAttemptResourceMeter(store.hierarchy(session))
	if err != nil {
		t.Fatal(err)
	}
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &scanAttempt{
		id: idValue[ScanAttemptID](238), generation: idValue[DirectoryGeneration](239),
		directory: idValue[DirectoryID](240), done: make(chan struct{}), ctx: attemptContext, cancel: cancel,
		admission: admission, resources: resources,
	}
	store.mu.Lock()
	store.closed = true
	store.mu.Unlock()
	store.scanWG.Add(1)
	store.runAttempt(attempt, NodeRecord{}, session, nil)
	if !errors.Is(attempt.err, ErrCatalogClosed) {
		t.Fatalf("closed-store attempt error = %v", attempt.err)
	}
	store.mu.Lock()
	store.closed = false
	store.mu.Unlock()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		spill   SpillFactory
		scanner DirectoryScanner
	}{
		{
			name: "nil workspace", spill: contractSpillFactory{},
			scanner: DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
				return ScanResult{}, nil
			}),
		},
		{
			name: "omission overflow", spill: NewFileSpillFactory(t.TempDir()),
			scanner: DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
				return ScanResult{OmittedCount: MaxDirectoryEntries + 1}, nil
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := NewMemoryCatalogBackend()
			config := contractStoreConfig(t, backend, test.spill)
			candidate, err := NewCatalogStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.Close()
			candidateSession := generousBudget(t, "candidate-session")
			directory := prepareScannableDirectory(t, candidate, candidateSession, 241, 242)
			if _, err := candidate.ListChildren(context.Background(), directory, candidateSession, ScanOptions{}, test.scanner); err == nil {
				t.Fatal("invalid scan attempt completed successfully")
			}
		})
	}
}
