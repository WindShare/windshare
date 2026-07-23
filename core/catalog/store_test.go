package catalog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func generousBudget(t *testing.T, name string) *BudgetAccount {
	t.Helper()
	account, err := NewBudgetAccount(name, BudgetLimits{
		ActiveScans: 100, ScanWork: 100_000, Entries: 100_000,
		MemoryBytes: 64 << 20, SpillBytes: 256 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func rootCommit(t *testing.T, instance ShareInstance, root DirectoryID, generation DirectoryGeneration, children []NodeRecord) DirectoryCommit {
	t.Helper()
	commit, err := NewSyntheticRootCommit(SyntheticRootCommitSpec{
		ShareInstance: instance, SyntheticRoot: root, Generation: generation, SelectedRoots: children,
	})
	if err != nil {
		// Some validation tests intentionally need an empty synthetic root.
		rootRecord, createErr := NewSyntheticRootNodeRecord(root)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return DirectoryCommit{
			directory: rootRecord, generation: generation, children: newSliceNodeSource(children), synthetic: true,
		}
	}
	return commit
}

func commitSyntheticRoot(t *testing.T, store *CatalogStore, commit DirectoryCommit, startupBudget *BudgetAccount) CommittedRoot {
	t.Helper()
	root, err := store.CommitSyntheticRoot(context.Background(), commit, startupBudget)
	if err != nil {
		t.Fatal(err)
	}
	if root.IsZero() {
		t.Fatal("successful synthetic-root commit returned no capability")
	}
	return root
}

func newStore(t *testing.T, backend CatalogBackend, clock Clock) (*CatalogStore, *BudgetAccount, *BudgetAccount) {
	t.Helper()
	process := generousBudget(t, "process")
	share := generousBudget(t, "share")
	var nextAttempt atomic.Uint32
	var nextGeneration atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: backend,
		ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{},
		Clock: clock, SortRunBytes: 512,
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			return idValue[ScanAttemptID](byte(nextAttempt.Add(1))), nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			return idValue[DirectoryGeneration](byte(nextGeneration.Add(20))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, process, share
}

func TestDefaultMemoryStoreSpillNamespacesArePrivate(t *testing.T) {
	first, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	second, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	firstSpill, firstOK := first.spillFactory.(*FileSpillFactory)
	secondSpill, secondOK := second.spillFactory.(*FileSpillFactory)
	if !firstOK || !secondOK || !firstSpill.ownedRoot || !secondSpill.ownedRoot || firstSpill.root == secondSpill.root {
		t.Fatalf("default spill roots are not private: %#v / %#v", firstSpill, secondSpill)
	}
	for _, path := range []string{firstSpill.shareRoot(first.shareInstance), secondSpill.shareRoot(second.shareInstance)} {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("private spill root %q: info=%v err=%v", path, info, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(firstSpill.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed store retained private spill root: %v", err)
	}
	if info, err := os.Stat(secondSpill.shareRoot(second.shareInstance)); err != nil || !info.IsDir() {
		t.Fatalf("closing first store damaged second root: info=%v err=%v", info, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func scannedFile(t *testing.T, id byte, name string, size uint64) ScannedChild {
	t.Helper()
	locator, err := NewLocator(0, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := NewSourceIdentity([]byte("source-" + name))
	candidate, _ := NewVersionCandidate([]byte("version-" + name))
	return ScannedChild{
		FileID: idValue[FileID](id), Name: name, Locator: locator,
		SourceIdentity: identity, VersionCandidate: candidate, ExpectedSize: size,
	}
}

func scannedDirectory(t *testing.T, id byte, name string) ScannedChild {
	t.Helper()
	locator, err := NewLocator(0, name)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := NewSourceIdentity([]byte("source-" + name))
	return ScannedChild{
		DirectoryID: idValue[DirectoryID](id), Name: name, Locator: locator, SourceIdentity: identity,
	}
}

func selectedDirectory(t *testing.T, root DirectoryID, id byte, name string) NodeRecord {
	t.Helper()
	child := scannedDirectory(t, id, name)
	record, err := child.nodeRecord(root)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestBudgetReservationIsHierarchicalTransferableAndRollbackSafe(t *testing.T) {
	process := generousBudget(t, "process")
	share, _ := NewBudgetAccount("share", BudgetLimits{
		ActiveScans: 2, ScanWork: 2, Entries: 2, MemoryBytes: 20, SpillBytes: 20,
	})
	session := generousBudget(t, "session")
	reservation, err := ReserveHierarchy(
		BudgetHierarchy{Process: process, Share: share, Session: session},
		ResourceUsage{Entries: 1, MemoryBytes: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Grow(ResourceUsage{Entries: 1, MemoryBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Grow(ResourceUsage{Entries: 1}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("overflow error = %v", err)
	}
	if err := reservation.Shrink(ResourceUsage{Entries: 1, MemoryBytes: 5}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.keep(ResourceUsage{Entries: 1, MemoryBytes: 5}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.dropAccount(session); err != nil {
		t.Fatal(err)
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("dropping the session retained its usage")
	}
	reservation.Release()
	for _, account := range []*BudgetAccount{process, share, session} {
		if account.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("%s leaked usage: %+v", account.Name(), account.Snapshot().Used)
		}
	}
}

type failingBackend struct {
	CatalogBackend
	failPublish atomic.Bool
}

func (b *failingBackend) BeginDirectory(ctx context.Context, directory DirectoryID, generation DirectoryGeneration, meter ResourceMeter) (BackendTransaction, error) {
	transaction, err := b.CatalogBackend.BeginDirectory(ctx, directory, generation, meter)
	if err != nil {
		return nil, err
	}
	return &failingTransaction{BackendTransaction: transaction, failPublish: &b.failPublish}, nil
}

type failingTransaction struct {
	BackendTransaction
	failPublish *atomic.Bool
}

func (t *failingTransaction) Publish(ctx context.Context) (CommittedDirectory, error) {
	if t.failPublish.Load() {
		return CommittedDirectory{}, errors.New("injected publish failure")
	}
	return t.BackendTransaction.Publish(ctx)
}

func TestCatalogStorePublishesPagesAndBudgetOnlyAfterAtomicCommit(t *testing.T) {
	backend := &failingBackend{CatalogBackend: NewMemoryCatalogBackend()}
	backend.failPublish.Store(true)
	store, process, share := newStore(t, backend, nil)
	session := generousBudget(t, "session")
	root := idValue[DirectoryID](2)
	commit := rootCommit(t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](3), []NodeRecord{
		selectedDirectory(t, root, 4, "selected"),
	})
	if failed, err := store.CommitSyntheticRoot(context.Background(), commit, session); err == nil || !failed.IsZero() {
		t.Fatal("injected publish failure created a committed-root capability")
	}
	if _, ok, err := store.Directory(context.Background(), root); err != nil || ok {
		t.Fatalf("failed generation was visible: ok=%v err=%v", ok, err)
	}
	if process.Snapshot().Used != (ResourceUsage{}) || share.Snapshot().Used != (ResourceUsage{}) ||
		session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("failed transaction leaked budget")
	}

	backend.failPublish.Store(false)
	committedRoot := commitSyntheticRoot(t, store, commit, session)
	directory, ok, err := store.Directory(context.Background(), root)
	if err != nil || !ok || directory.PageCount() != 1 || directory.EntryCount() != 1 {
		t.Fatalf("committed directory = %+v ok=%v err=%v", directory, ok, err)
	}
	page, ok, err := store.Page(context.Background(), root, directory.Generation(), 0)
	if err != nil || !ok || page.Entries()[0].Name() != "selected" {
		t.Fatalf("page replay = %+v ok=%v err=%v", page, ok, err)
	}
	descriptor, err := NewShareDescriptor(DescriptorSpec{
		WireVersion: WireVersionV2, Suite: SuiteV2, ShareInstance: idValue[ShareInstance](1),
		SyntheticRoot: root, RootCommit: committedRoot, ChunkSize: DefaultChunkSize,
		Capabilities: CapabilityCatalog, SenderPublicKey: bytes.Repeat([]byte{1}, SenderPublicKeySize),
		PathPolicy: PathPolicyV1,
	})
	if err != nil || committedRoot.AuthorizeRegistration(descriptor) != nil {
		t.Fatalf("committed root did not authorize descriptor: %v", err)
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("completed commit retained reconnect-scoped session budget: %+v", session.Snapshot().Used)
	}
	if process.Snapshot().Used.Entries != 1 || share.Snapshot().Used.Entries != 1 {
		t.Fatal("process/share did not retain committed entry accounting")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if process.Snapshot().Used != (ResourceUsage{}) || share.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("store close did not release retained budget")
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func prepareScannableDirectory(t *testing.T, store *CatalogStore, session *BudgetAccount, rootByte, directoryByte byte) DirectoryID {
	t.Helper()
	root := idValue[DirectoryID](rootByte)
	directory := idValue[DirectoryID](directoryByte)
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](rootByte+1),
		[]NodeRecord{selectedDirectory(t, root, directoryByte, "directory")},
	), session)
	return directory
}

func TestListChildrenSingleflightSortsStreamAndReplaysPages(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 40, 42)
	started := make(chan struct{})
	release := make(chan struct{})
	var scans atomic.Int32
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		if scans.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return ScanResult{}, ctx.Err()
		case <-release:
		}
		for _, child := range []ScannedChild{
			scannedFile(t, 51, "zeta", 3), scannedFile(t, 52, "alpha", 1), scannedFile(t, 53, "middle", 2),
		} {
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
			if err == nil && committed.EntryCount() != 3 {
				err = errors.New("wrong entry count")
			}
			results <- err
		}()
	}
	<-started
	close(release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if scans.Load() != 1 {
		t.Fatalf("singleflight invoked scanner %d times", scans.Load())
	}
	committed, ok, err := store.Directory(context.Background(), directory)
	if err != nil || !ok {
		t.Fatal(err)
	}
	page, ok, err := store.Page(context.Background(), directory, committed.Generation(), 0)
	if err != nil || !ok {
		t.Fatal(err)
	}
	names := []string{page.entries[0].name, page.entries[1].name, page.entries[2].name}
	if fmtNames(names) != "alpha,middle,zeta" {
		t.Fatalf("deterministic order = %v", names)
	}
}

func fmtNames(names []string) string {
	var result string
	for index, name := range names {
		if index != 0 {
			result += ","
		}
		result += name
	}
	return result
}

func TestListChildrenFailureAuthorityRetryAndStaleDoNotPublish(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), clock)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 60, 62)
	var calls atomic.Int32
	transient := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		calls.Add(1)
		return ScanResult{}, NewTransientScanError(errors.New("busy"), time.Second)
	})
	_, firstErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, transient)
	_, reusedErr := store.ListChildren(context.Background(), directory, session, ScanOptions{Retry: true}, transient)
	var first, reused *DirectoryFailure
	if !errors.As(firstErr, &first) || !errors.As(reusedErr, &reused) ||
		first.AttemptID != reused.AttemptID || calls.Load() != 1 {
		t.Fatalf("transient authority was not reused: %v / %v", firstErr, reusedErr)
	}
	if first.Error() == "" {
		t.Fatal("transient directory failure lost its diagnostic")
	}
	clock.Advance(time.Second)
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, transient); err == nil || calls.Load() != 1 {
		t.Fatal("retry occurred without an explicit request")
	}
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{Retry: true}, transient); err == nil || calls.Load() != 2 {
		t.Fatal("explicit cooled retry did not start a new attempt")
	}

	staleStore, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer staleStore.Close()
	staleSession := generousBudget(t, "stale-session")
	other := prepareScannableDirectory(t, staleStore, staleSession, 70, 72)
	stale := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		if err := request.Children.Add(ctx, scannedFile(t, 73, "provisional", 1)); err != nil {
			return ScanResult{}, err
		}
		return ScanResult{}, ErrDirectoryStale
	})
	if _, err := staleStore.ListChildren(context.Background(), other, staleSession, ScanOptions{}, stale); !errors.Is(err, ErrDirectoryStale) {
		t.Fatalf("stale scan error = %v", err)
	}
	if _, exists, err := staleStore.Directory(context.Background(), other); err != nil || exists {
		t.Fatal("stale scan published a half generation")
	}
}

func TestListChildrenCancelsLastWaiterAndContainsPanics(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 80, 82)
	started := make(chan struct{})
	finished := make(chan struct{})
	blocked := DirectoryScannerFunc(func(ctx context.Context, _ ScanRequest) (ScanResult, error) {
		close(started)
		<-ctx.Done()
		close(finished)
		return ScanResult{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := store.ListChildren(ctx, directory, session, ScanOptions{}, blocked)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	<-finished

	panicStore, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer panicStore.Close()
	panicSession := generousBudget(t, "panic-session")
	panicDirectory := prepareScannableDirectory(t, panicStore, panicSession, 90, 92)
	panicScanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		panic("boom")
	})
	if _, err := panicStore.ListChildren(context.Background(), panicDirectory, panicSession, ScanOptions{}, panicScanner); err == nil {
		t.Fatal("scanner panic escaped as success")
	}
}

func TestScanChildSinkSerializesConcurrentProducersAndRejectsLateUse(t *testing.T) {
	store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
	defer store.Close()
	session := generousBudget(t, "session")
	directory := prepareScannableDirectory(t, store, session, 93, 95)
	children := make([]ScannedChild, 32)
	for index := range children {
		name := "child-" + string(rune('a'+index/10)) + string(rune('0'+index%10))
		children[index] = scannedFile(t, byte(130+index), name, 1)
	}
	var retained ScanChildSink
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		retained = request.Children
		errorsByChild := make(chan error, len(children))
		var workers sync.WaitGroup
		for _, child := range children {
			workers.Add(1)
			go func(child ScannedChild) {
				defer workers.Done()
				errorsByChild <- request.Children.Add(ctx, child)
			}(child)
		}
		workers.Wait()
		close(errorsByChild)
		for err := range errorsByChild {
			if err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})
	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil || committed.EntryCount() != uint64(len(children)) {
		t.Fatalf("concurrent sink result = %+v err=%v", committed, err)
	}
	if err := retained.Add(context.Background(), scannedFile(t, 200, "late", 1)); !errors.Is(err, ErrScanSinkClosed) {
		t.Fatalf("late sink use = %v", err)
	}
}

func TestExternalSortRejectsPortableAndIdentityCollisions(t *testing.T) {
	for name, children := range map[string][]ScannedChild{
		"portable-name": {scannedFile(t, 100, "README", 1), scannedFile(t, 101, "Readme", 1)},
		"node-identity": {scannedFile(t, 102, "a", 1), scannedFile(t, 102, "b", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			store, _, _ := newStore(t, NewMemoryCatalogBackend(), nil)
			defer store.Close()
			session := generousBudget(t, "session")
			directory := prepareScannableDirectory(t, store, session, 110, 112)
			scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
				for _, child := range children {
					if err := request.Children.Add(ctx, child); err != nil {
						return ScanResult{}, err
					}
				}
				return ScanResult{}, nil
			})
			if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner); !errors.Is(err, ErrSiblingCollision) {
				t.Fatalf("collision error = %v", err)
			}
			if _, exists, _ := store.Directory(context.Background(), directory); exists {
				t.Fatal("colliding scan was committed")
			}
		})
	}
}

func TestShareBudgetSurvivesSessionReconnect(t *testing.T) {
	process := generousBudget(t, "process")
	share, err := NewBudgetAccount("share", BudgetLimits{
		ActiveScans: 10, ScanWork: 100, Entries: 2, MemoryBytes: 1 << 20, SpillBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	var attempt atomic.Uint32
	var generation atomic.Uint32
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: NewMemoryCatalogBackend(),
		ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{}, SortRunBytes: 256,
		AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
			return idValue[ScanAttemptID](byte(attempt.Add(1))), nil
		}),
		Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			return idValue[DirectoryGeneration](byte(generation.Add(30))), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessionA := generousBudget(t, "session-a")
	sessionB := generousBudget(t, "session-b")
	root := idValue[DirectoryID](120)
	first := selectedDirectory(t, root, 121, "first")
	second := selectedDirectory(t, root, 122, "second")
	commitSyntheticRoot(t, store, rootCommit(t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](123), []NodeRecord{first, second}), sessionA)
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 124, "file", 1))
	})
	if _, err := store.ListChildren(context.Background(), idValue[DirectoryID](121), sessionB, ScanOptions{}, scanner); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reconnect reset share entry budget: %v", err)
	}
	if sessionA.Snapshot().Used != (ResourceUsage{}) || sessionB.Snapshot().Used != (ResourceUsage{}) {
		t.Fatal("session budget was retained after operation completion")
	}
}
