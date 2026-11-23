package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func prepareSelectedDirectories(
	t *testing.T,
	store *CatalogStore,
	session *BudgetAccount,
	rootByte byte,
	dirBytes ...byte,
) []DirectoryID {
	t.Helper()
	root := idValue[DirectoryID](rootByte)
	selected := make([]NodeRecord, 0, len(dirBytes))
	ids := make([]DirectoryID, 0, len(dirBytes))
	for index, b := range dirBytes {
		ids = append(ids, idValue[DirectoryID](b))
		selected = append(selected, selectedDirectory(t, root, b, fmt.Sprintf("dir-%d", index)))
	}
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](rootByte+1), selected,
	), session)
	return ids
}

func fixedSizeScannedFile(t *testing.T, seed byte, name string, payloadBytes int) ScannedChild {
	t.Helper()
	ref, err := NewSourceReference(bytes.Repeat([]byte("r"), payloadBytes))
	if err != nil {
		t.Fatal(err)
	}
	child := scannedFile(t, seed, name, 1)
	child.SourceReference = ref
	return child
}

func TestListChildrenResolvesConcurrentSessionMemoryContentionWithoutPermanentFailure(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()

	rootSession := generousBudget(t, "root-session")
	dirs := prepareSelectedDirectories(t, store, rootSession, 10, 11, 12)
	dirA, dirB := dirs[0], dirs[1]

	// Each child consumes ~33 KiB; 3 children (~100 KiB) fit comfortably within
	// a 160 KiB session memory budget sequentially, but two concurrent scans
	// exceed 160 KiB after both add 2 children (~132 KiB combined).
	const (
		childrenPerDir = 3
		refBytes       = 32 << 10
		sessionMem     = 160 << 10
	)
	session, err := NewBudgetAccount("contended-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: sessionMem,
	})
	if err != nil {
		t.Fatal(err)
	}

	var dirAScans, dirBScans atomic.Int32
	bothEntered := make(chan struct{})
	var entered atomic.Int32
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		dirID, _ := request.Directory.DirectoryID()
		var seedBase byte
		var pass int32
		if dirID == dirA {
			seedBase = 100
			pass = dirAScans.Add(1)
		} else {
			seedBase = 150
			pass = dirBScans.Add(1)
		}
		for index := range childrenPerDir {
			child := fixedSizeScannedFile(t, seedBase+byte(index), fmt.Sprintf("f-%d", index), refBytes)
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
			// Hold both scans after 2 children on their first pass so they deterministically
			// contend for the shared session memory budget on the 3rd child.
			if pass == 1 && index == 1 {
				if entered.Add(1) == 2 {
					close(bothEntered)
				}
				select {
				case <-bothEntered:
				case <-ctx.Done():
					return ScanResult{}, ctx.Err()
				}
			}
		}
		return ScanResult{}, nil
	})

	type outcome struct {
		dir       DirectoryID
		committed CommittedDirectory
		err       error
	}
	outcomes := make(chan outcome, 2)
	for _, dir := range []DirectoryID{dirA, dirB} {
		go func(dir DirectoryID) {
			committed, listErr := store.ListChildren(context.Background(), dir, session, ScanOptions{}, scanner)
			outcomes <- outcome{dir: dir, committed: committed, err: listErr}
		}(dir)
	}
	for range 2 {
		got := <-outcomes
		if got.err != nil {
			t.Fatalf("concurrent scan for %x failed: %v", got.dir, got.err)
		}
		if got.committed.EntryCount() != childrenPerDir {
			t.Fatalf("directory %x entries = %d, want %d", got.dir, got.committed.EntryCount(), childrenPerDir)
		}
	}
	if dirAScans.Load()+dirBScans.Load() < 3 {
		t.Fatalf("expected at least one contended exclusive retry, got scans A=%d B=%d", dirAScans.Load(), dirBScans.Load())
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("session leaked budget after contended scans: %+v", session.Snapshot().Used)
	}
}

func TestListChildrenFourWaySimultaneousContentionDoesNotDeadlockAndPreservesMonotonicProgress(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()

	rootSession := generousBudget(t, "root-session")
	dirs := prepareSelectedDirectories(t, store, rootSession, 20, 21, 22, 23, 24)

	// 4 scans each reserve 1 child (~44.3 KiB -> ~177 KiB total). On a 184 KiB
	// session budget, a single directory (2 children ~89 KiB + 60 KiB page encode
	// = ~149 KiB) fits cleanly alone, while all 4 parallel scans fail their 2nd
	// child simultaneously and must drain through the FIFO exclusive retry lane.
	const (
		childrenPerDir = 2
		refBytes       = 44 << 10
		sessionMem     = 184 << 10
	)
	session, err := NewBudgetAccount("four-way-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: sessionMem,
	})
	if err != nil {
		t.Fatal(err)
	}

	var atBarrier atomic.Int32
	barrier := make(chan struct{})
	var firstPassDone sync.Map

	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		dirID, _ := request.Directory.DirectoryID()
		_, retried := firstPassDone.LoadOrStore(dirID, struct{}{})
		seedBase := dirID[0] * 10
		for index := range childrenPerDir {
			child := fixedSizeScannedFile(t, seedBase+byte(index), fmt.Sprintf("item-%d", index), refBytes)
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
			if !retried && index == 0 {
				if atBarrier.Add(1) == 4 {
					close(barrier)
				}
				select {
				case <-barrier:
				case <-ctx.Done():
					return ScanResult{}, ctx.Err()
				}
			}
		}
		return ScanResult{}, nil
	})

	var progressMu sync.Mutex
	lastProgress := make(map[DirectoryID]uint64, len(dirs))
	progressErrs := make(chan error, len(dirs))
	results := make(chan error, len(dirs))

	for _, dir := range dirs {
		go func(dir DirectoryID) {
			observer := ScanProgressObserverFunc(func(_ context.Context, update ScanProgress) error {
				progressMu.Lock()
				prev := lastProgress[dir]
				if update.DiscoveredEntries < prev {
					progressMu.Unlock()
					progressErrs <- fmt.Errorf("progress regressed for %x: %d < %d", dir, update.DiscoveredEntries, prev)
					return nil
				}
				lastProgress[dir] = update.DiscoveredEntries
				progressMu.Unlock()
				return nil
			})
			committed, listErr := store.ListChildren(
				context.Background(), dir, session, ScanOptions{Progress: observer}, scanner,
			)
			if listErr == nil && committed.EntryCount() != childrenPerDir {
				listErr = fmt.Errorf("dir %x committed %d entries, want %d", dir, committed.EntryCount(), childrenPerDir)
			}
			results <- listErr
		}(dir)
	}

	for range dirs {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	close(progressErrs)
	for err := range progressErrs {
		t.Fatal(err)
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("session leaked budget after four-way contention: %+v", session.Snapshot().Used)
	}
}

func TestListChildrenSoloBudgetFailureIsRetryableAfterCooldownWithoutLocalRescan(t *testing.T) {
	clock := &fakeClock{now: time.Unix(200, 0)}
	store, _, _ := newStore(t, clock)
	defer store.Close()

	rootSession := generousBudget(t, "root-session")
	directory := prepareScannableDirectory(t, store, rootSession, 30, 31)

	tightSession, err := NewBudgetAccount("tight-solo", BudgetLimits{
		ActiveScans: 4, ScanWork: 100, Entries: 100, MemoryBytes: MaxCatalogPayloadBytes + (32 << 10),
	})
	if err != nil {
		t.Fatal(err)
	}
	generousSession := generousBudget(t, "later-session")

	var scans atomic.Int32
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		scans.Add(1)
		for index := range 4 {
			child := fixedSizeScannedFile(t, byte(80+index), fmt.Sprintf("big-%d", index), 40<<10)
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})

	_, firstErr := store.ListChildren(context.Background(), directory, tightSession, ScanOptions{}, scanner)
	var firstFailure *DirectoryFailure
	if !errors.As(firstErr, &firstFailure) || firstFailure.Kind != FailureKindBudget ||
		!firstFailure.Transient || firstFailure.RetryAfter != DefaultScanRetryCooldown {
		t.Fatalf("solo budget failure = %v, want retryable FailureKindBudget", firstErr)
	}
	if scans.Load() != 1 {
		t.Fatalf("uncontended solo budget failure rescanned %d times, want 1", scans.Load())
	}

	// Before cooldown elapses, explicit retry must replay the exact failure authority.
	_, duringCooldownErr := store.ListChildren(context.Background(), directory, generousSession, ScanOptions{Retry: true}, scanner)
	var replayed *DirectoryFailure
	if !errors.As(duringCooldownErr, &replayed) || replayed.AttemptID != firstFailure.AttemptID || scans.Load() != 1 {
		t.Fatalf("cooldown retry = %v scans=%d", duringCooldownErr, scans.Load())
	}

	clock.Advance(DefaultScanRetryCooldown)
	// Implicit lookup after cooldown still requires explicit retry.
	if _, err := store.ListChildren(context.Background(), directory, generousSession, ScanOptions{}, scanner); err == nil || scans.Load() != 1 {
		t.Fatalf("implicit call bypassed explicit retry: err=%v scans=%d", err, scans.Load())
	}

	// Explicit retry after cooldown with sufficient budget commits the directory.
	committed, err := store.ListChildren(context.Background(), directory, generousSession, ScanOptions{Retry: true}, scanner)
	if err != nil || committed.EntryCount() != 4 || scans.Load() != 2 {
		t.Fatalf("explicit retry after cooldown = %+v err=%v scans=%d", committed, err, scans.Load())
	}
}

func TestListChildrenCrossShareProcessBudgetFailureRecoversAfterSiblingShareCloses(t *testing.T) {
	clock := &fakeClock{now: time.Unix(300, 0)}
	process, err := NewBudgetAccount("shared-process", BudgetLimits{
		ActiveScans: 8, ScanWork: 1_000, Entries: 1_000, MemoryBytes: 220 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	newShareStore := func(shareSeed byte) *CatalogStore {
		share, err := NewBudgetAccount(fmt.Sprintf("share-%d", shareSeed), BudgetLimits{
			ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: 220 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		var nextAttempt, nextGen atomic.Uint32
		store, err := NewCatalogStore(StoreConfig{
			ShareInstance:  idValue[ShareInstance](shareSeed),
			ProcessBudget:  process,
			ShareBudget:    share,
			FailureEncoder: testFailureEncoder{},
			Clock:          clock,
			AttemptIDs: ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) {
				return idValue[ScanAttemptID](byte(nextAttempt.Add(1))), nil
			}),
			Generations: DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
				return idValue[DirectoryGeneration](byte(nextGen.Add(1))), nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	store1 := newShareStore(1)
	defer store1.Close()
	store2 := newShareStore(2)
	defer store2.Close()

	session1 := generousBudget(t, "s1")
	session2 := generousBudget(t, "s2")
	root1 := idValue[DirectoryID](40)
	dir1 := idValue[DirectoryID](41)
	commitSyntheticRoot(t, store1, rootCommit(
		t, idValue[ShareInstance](1), root1, idValue[DirectoryGeneration](42),
		[]NodeRecord{selectedDirectory(t, root1, 41, "s1-dir")},
	), session1)
	root2 := idValue[DirectoryID](50)
	dir2 := idValue[DirectoryID](51)
	commitSyntheticRoot(t, store2, rootCommit(
		t, idValue[ShareInstance](2), root2, idValue[DirectoryGeneration](52),
		[]NodeRecord{selectedDirectory(t, root2, 51, "s2-dir")},
	), session2)

	// Share 1 commits ~128 KiB, leaving enough room for failure encoding (~60 KiB)
	// in Process but not enough for Share 2's ~128 KiB directory.
	scan3 := func(seed byte, counter *atomic.Int32) DirectoryScanner {
		return DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			if counter != nil {
				counter.Add(1)
			}
			for index := range 3 {
				child := fixedSizeScannedFile(t, seed+byte(index), fmt.Sprintf("f-%d", index), 40<<10)
				if err := request.Children.Add(ctx, child); err != nil {
					return ScanResult{}, err
				}
			}
			return ScanResult{}, nil
		})
	}
	if _, err := store1.ListChildren(context.Background(), dir1, session1, ScanOptions{}, scan3(100, nil)); err != nil {
		t.Fatalf("share 1 commit failed: %v", err)
	}

	var share2Scans atomic.Int32
	_, err = store2.ListChildren(context.Background(), dir2, session2, ScanOptions{}, scan3(140, &share2Scans))
	var failure *DirectoryFailure
	if !errors.As(err, &failure) || failure.Kind != FailureKindBudget || !failure.Transient {
		t.Fatalf("share 2 process-budget error = %v, want retryable FailureKindBudget", err)
	}
	if share2Scans.Load() != 1 {
		t.Fatalf("cross-share process budget failure rescanned locally: %d", share2Scans.Load())
	}

	if err := store1.Close(); err != nil {
		t.Fatal(err)
	}
	clock.Advance(DefaultScanRetryCooldown)
	committed, err := store2.ListChildren(context.Background(), dir2, session2, ScanOptions{Retry: true}, scan3(140, &share2Scans))
	if err != nil || committed.EntryCount() != 3 {
		t.Fatalf("share 2 retry after share 1 closed = %+v err=%v", committed, err)
	}
}

func TestListChildrenContendedOversizedDirectoryRetriesAtMostOnce(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()

	rootSession := generousBudget(t, "root-session")
	dirs := prepareSelectedDirectories(t, store, rootSession, 60, 61, 62)
	smallDir, hugeDir := dirs[0], dirs[1]

	session, err := NewBudgetAccount("oversized-contended", BudgetLimits{
		ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: 120 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	smallEntered := make(chan struct{})
	releaseSmall := make(chan struct{})
	var hugeScans atomic.Int32
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		dirID, _ := request.Directory.DirectoryID()
		if dirID == smallDir {
			close(smallEntered)
			<-releaseSmall
			return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 90, "tiny", 1))
		}
		if hugeScans.Add(1) == 1 {
			<-smallEntered
			defer close(releaseSmall)
		}
		// 5 * 40 KiB = 200 KiB > 120 KiB even when running alone.
		for index := range 5 {
			child := fixedSizeScannedFile(t, byte(110+index), fmt.Sprintf("huge-%d", index), 40<<10)
			if err := request.Children.Add(ctx, child); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})

	smallDone := make(chan error, 1)
	go func() {
		_, listErr := store.ListChildren(context.Background(), smallDir, session, ScanOptions{}, scanner)
		smallDone <- listErr
	}()
	_, hugeErr := store.ListChildren(context.Background(), hugeDir, session, ScanOptions{}, scanner)
	if err := <-smallDone; err != nil {
		t.Fatalf("small peer scan failed: %v", err)
	}
	var failure *DirectoryFailure
	if !errors.As(hugeErr, &failure) || failure.Kind != FailureKindBudget || !failure.Transient {
		t.Fatalf("oversized contended directory error = %v, want retryable FailureKindBudget", hugeErr)
	}
	if hugeScans.Load() != 2 {
		t.Fatalf("oversized contended directory scanned %d times, want exactly 2 (initial + 1 exclusive retry)", hugeScans.Load())
	}
}

func TestListChildrenCancelingQueuedExclusiveRetryWakesLaterScansAndLeavesNoFailure(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()

	rootSession := generousBudget(t, "root-session")
	dirs := prepareSelectedDirectories(t, store, rootSession, 70, 71, 72, 73)
	dirA, dirB, dirC := dirs[0], dirs[1], dirs[2]

	// dirA holds 2 * ~42 KiB = ~84 KiB; dirB needs 3 * ~42 KiB = ~126 KiB and
	// hits the 160 KiB session ceiling while A is active, whereas tiny dirC
	// (~300 B + 60 KiB page encode) fits alongside A once B leaves retryQueue.
	session, err := NewBudgetAccount("cancel-queue-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: 160 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	dirAHolding := make(chan struct{})
	releaseA := make(chan struct{})
	bContended := make(chan struct{})
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		dirID, _ := request.Directory.DirectoryID()
		switch dirID {
		case dirA:
			for index := range 2 {
				if err := request.Children.Add(ctx, fixedSizeScannedFile(t, byte(120+index), fmt.Sprintf("a-%d", index), 42<<10)); err != nil {
					return ScanResult{}, err
				}
			}
			close(dirAHolding)
			<-releaseA
			return ScanResult{}, nil
		case dirB:
			<-dirAHolding
			for index := range 3 {
				if err := request.Children.Add(ctx, fixedSizeScannedFile(t, byte(140+index), fmt.Sprintf("b-%d", index), 42<<10)); err != nil {
					close(bContended)
					return ScanResult{}, err
				}
			}
			return ScanResult{}, nil
		default:
			return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 160, "c-0", 1))
		}
	})

	aDone := make(chan error, 1)
	go func() {
		_, listErr := store.ListChildren(context.Background(), dirA, session, ScanOptions{}, scanner)
		aDone <- listErr
	}()

	bCtx, cancelB := context.WithCancel(context.Background())
	bDone := make(chan error, 1)
	go func() {
		_, listErr := store.ListChildren(bCtx, dirB, session, ScanOptions{}, scanner)
		bDone <- listErr
	}()

	<-bContended
	// Wait until B is parked in the exclusive retry queue, then start C (which
	// blocks in enterParallelScan while retryQueue is non-empty).
	for {
		store.mu.RLock()
		queued := len(store.retryQueue)
		store.mu.RUnlock()
		if queued == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	cDone := make(chan error, 1)
	go func() {
		_, listErr := store.ListChildren(context.Background(), dirC, session, ScanOptions{}, scanner)
		cDone <- listErr
	}()

	cancelB()
	if err := <-bDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued retry = %v, want context.Canceled", err)
	}
	// Cancelling B emptied retryQueue, so C should proceed immediately even before A finishes.
	if err := <-cDone; err != nil {
		t.Fatalf("later parallel scan C failed after B cancelled: %v", err)
	}
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("scan A failed: %v", err)
	}
	// B left no persisted failure and succeeds cleanly on a fresh call.
	committedB, err := store.ListChildren(context.Background(), dirB, session, ScanOptions{}, DirectoryScannerFunc(
		func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 170, "b-ok", 1))
		},
	))
	if err != nil || committedB.EntryCount() != 1 {
		t.Fatalf("fresh scan of cancelled dirB = %+v err=%v", committedB, err)
	}
}

func TestCatalogStoreCloseUnblocksQueuedExclusiveRetry(t *testing.T) {
	store, _, _ := newStore(t, nil)
	rootSession := generousBudget(t, "root-session")
	dirs := prepareSelectedDirectories(t, store, rootSession, 80, 81, 82)
	dirA, dirB := dirs[0], dirs[1]

	session, err := NewBudgetAccount("close-queue-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: 120 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	dirAHolding := make(chan struct{})
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		dirID, _ := request.Directory.DirectoryID()
		if dirID == dirA {
			for index := range 2 {
				if err := request.Children.Add(ctx, fixedSizeScannedFile(t, byte(180+index), fmt.Sprintf("a-%d", index), 36<<10)); err != nil {
					return ScanResult{}, err
				}
			}
			close(dirAHolding)
			<-ctx.Done()
			return ScanResult{}, ctx.Err()
		}
		<-dirAHolding
		for index := range 3 {
			if err := request.Children.Add(ctx, fixedSizeScannedFile(t, byte(190+index), fmt.Sprintf("b-%d", index), 36<<10)); err != nil {
				return ScanResult{}, err
			}
		}
		return ScanResult{}, nil
	})

	done := make(chan error, 2)
	go func() {
		_, listErr := store.ListChildren(context.Background(), dirA, session, ScanOptions{}, scanner)
		done <- listErr
	}()
	go func() {
		_, listErr := store.ListChildren(context.Background(), dirB, session, ScanOptions{}, scanner)
		done <- listErr
	}()

	for {
		store.mu.RLock()
		queued := len(store.retryQueue)
		store.mu.RUnlock()
		if queued == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err == nil {
			t.Fatal("scan succeeded after store close")
		}
	}
	if session.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("closed store leaked session budget: %+v", session.Snapshot().Used)
	}
}
