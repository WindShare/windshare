package catalog

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"sync/atomic"
	"testing"
	"time"
)

func testFailureRecord(t *testing.T, attempt, generation byte, previous ScanAttemptID, kind FailureKind) DirectoryFailureRecord {
	t.Helper()
	retryAfter := time.Duration(0)
	if kind == FailureKindTransientIO || kind == FailureKindBudget {
		retryAfter = MinScanRetryCooldown
	}
	record, err := newDirectoryFailureRecord(
		idValue[ShareInstance](1), idValue[DirectoryID](2), idValue[ScanAttemptID](attempt),
		idValue[DirectoryGeneration](generation), previous, kind, retryAfter,
	)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestFailureRecordValidationAndClassificationRejectAmbiguousAuthority(t *testing.T) {
	valid := testFailureRecord(t, 3, 4, ScanAttemptID{}, FailureKindPermanentIO)
	if _, err := newDirectoryFailureRecord(
		ShareInstance{}, valid.directory, valid.attempt, valid.generation, ScanAttemptID{},
		FailureKindPermanentIO, 0,
	); err == nil {
		t.Fatal("zero-share failure record was accepted")
	}
	for name, mutate := range map[string]func(*DirectoryFailureRecord){
		"unknown kind": func(record *DirectoryFailureRecord) { record.kind = FailureKindUnknown },
		"future kind":  func(record *DirectoryFailureRecord) { record.kind = FailureKindTransientIO + 1 },
		"self link":    func(record *DirectoryFailureRecord) { record.previousAttempt = record.attempt },
		"permanent retry": func(record *DirectoryFailureRecord) {
			record.retryAfter = MinScanRetryCooldown
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			mutate(&record)
			if err := record.valid(); err == nil {
				t.Fatal("invalid failure record was accepted")
			}
		})
	}
	transient := testFailureRecord(t, 5, 6, valid.attempt, FailureKindTransientIO)
	for name, delay := range map[string]time.Duration{
		"too short": MinScanRetryCooldown - time.Millisecond,
		"too long":  MaxScanRetryCooldown + time.Millisecond,
		"fraction":  MinScanRetryCooldown + time.Nanosecond,
	} {
		t.Run(name, func(t *testing.T) {
			record := transient
			record.retryAfter = delay
			if err := record.valid(); err == nil {
				t.Fatal("invalid transient delay was accepted")
			}
		})
	}
	if _, err := NewFailurePayload(nil); err == nil {
		t.Fatal("empty failure payload was accepted")
	}
	if _, err := NewFailurePayload(make([]byte, MaxFailurePayloadBytes+1)); err == nil {
		t.Fatal("oversized failure payload was accepted")
	}
	wantEncodeErr := errors.New("encode failed")
	encoder := FailureEncoderFunc(func(DirectoryFailureRecord) (FailurePayload, error) {
		return FailurePayload{}, wantEncodeErr
	})
	if _, err := encoder.EncodeFailure(valid); !errors.Is(err, wantEncodeErr) {
		t.Fatalf("failure encoder function = %v", err)
	}

	for cause, want := range map[error]FailureKind{
		ErrDirectoryStale:   FailureKindStale,
		fs.ErrPermission:    FailureKindPermission,
		ErrSiblingCollision: FailureKindCollision,
		ErrPageLimit:        FailureKindTooWide,
		ErrBudgetExceeded:   FailureKindBudget,
		errors.New("I/O"):   FailureKindPermanentIO,
	} {
		if got := classifyFailureKind(cause); got != want {
			t.Fatalf("classify %v = %d want %d", cause, got, want)
		}
	}
	transientCause := NewTransientScanError(errors.New("retry"), MinScanRetryCooldown)
	if got := classifyFailureKind(transientCause); got != FailureKindTransientIO {
		t.Fatalf("classify transient = %d", got)
	}
	budgetRecord := testFailureRecord(t, 7, 8, transient.attempt, FailureKindBudget)
	if !budgetRecord.Retryable() || failureRetryAfter(FailureKindBudget, ErrBudgetExceeded) != DefaultScanRetryCooldown {
		t.Fatalf("budget failure retryability = %v / %v", budgetRecord.Retryable(), failureRetryAfter(FailureKindBudget, ErrBudgetExceeded))
	}
	if _, err := newDirectoryFailureRecord(
		budgetRecord.share, budgetRecord.directory, budgetRecord.attempt, budgetRecord.generation,
		budgetRecord.previousAttempt, FailureKindBudget, 0,
	); err == nil {
		t.Fatal("budget failure without retry cooldown was accepted")
	}
}

func TestCatalogStoreFailurePayloadRequiresPublishedAttemptAuthority(t *testing.T) {
	record := testFailureRecord(t, 100, 101, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewFailurePayload([]byte("orphan failure"))
	store := &CatalogStore{
		failures: map[DirectoryID]map[ScanAttemptID]committedFailure{
			record.directory: {record.attempt: {record: record, payload: object}},
		},
		usedAttempts: make(map[scanAttemptIdentity]struct{}),
	}
	if _, _, err := store.FailurePayload(context.Background(), record.directory, record.attempt); err == nil {
		t.Fatal("unpublished attempt object was accepted")
	}
	identity := scanAttemptIdentity{directory: record.directory, attempt: record.attempt}
	store.usedAttempts[identity] = struct{}{}
	loaded, found, err := store.FailurePayload(context.Background(), record.directory, record.attempt)
	if err != nil || !found || string(loaded.Bytes()) != "orphan failure" {
		t.Fatalf("published attempt object = %q found=%v err=%v", loaded.Bytes(), found, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.FailurePayload(cancelled, record.directory, record.attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled store failure object = %v", err)
	}
	if _, found, err := store.FailurePayload(
		context.Background(), record.directory, idValue[ScanAttemptID](102),
	); err != nil || found {
		t.Fatalf("missing store failure object = found=%v err=%v", found, err)
	}
	store.closed = true
	if _, _, err := store.FailurePayload(context.Background(), record.directory, record.attempt); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed store failure object = %v", err)
	}
}

func TestPersistAttemptFailureEnforcesEncoderBudgetAndAttemptBounds(t *testing.T) {
	newAttempt := func(t *testing.T, store *CatalogStore, session *BudgetAccount, attemptByte byte) *scanAttempt {
		t.Helper()
		meter, err := newAttemptResourceMeter(store.hierarchy(session))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(meter.Close)
		attemptCtx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &scanAttempt{
			id:         idValue[ScanAttemptID](attemptByte),
			generation: idValue[DirectoryGeneration](attemptByte + 1),
			directory:  idValue[DirectoryID](110),
			ctx:        attemptCtx,
			cancel:     cancel,
			resources:  meter,
		}
	}

	t.Run("empty payload rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		store.failureEncoder = FailureEncoderFunc(func(DirectoryFailureRecord) (FailurePayload, error) {
			return FailurePayload{}, nil
		})
		session := generousBudget(t, "empty-payload-session")
		attempt := newAttempt(t, store, session, 10)
		if _, err := store.prepareAttemptFailure(attempt, errors.New("boom")); err == nil {
			t.Fatal("empty failure payload was persisted")
		}
	})

	t.Run("closed store rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		session := generousBudget(t, "closed-failure-session")
		attempt := newAttempt(t, store, session, 20)
		prepared, err := store.prepareAttemptFailure(attempt, errors.New("boom"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		_, commitErr := store.commitPreparedFailureLocked(attempt, prepared, errors.New("boom"), session)
		store.mu.Unlock()
		if !errors.Is(commitErr, ErrCatalogClosed) {
			t.Fatalf("closed store failure commit = %v", commitErr)
		}
	})

	t.Run("non-owner attempt rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		session := generousBudget(t, "non-owner-failure-session")
		attempt := newAttempt(t, store, session, 25)
		prepared, err := store.prepareAttemptFailure(attempt, errors.New("boom"))
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		_, commitErr := store.commitPreparedFailureLocked(attempt, prepared, errors.New("boom"), session)
		store.mu.Unlock()
		if !errors.Is(commitErr, context.Canceled) {
			t.Fatalf("non-owner failure commit = %v", commitErr)
		}
	})

	t.Run("tight budget rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		tightSession, err := NewBudgetAccount("tight-failure-session", BudgetLimits{
			ActiveScans: 1, ScanWork: 1, Entries: 1, MemoryBytes: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		attempt := newAttempt(t, store, tightSession, 30)
		if _, err := store.prepareAttemptFailure(attempt, errors.New("boom")); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("tight budget failure prepare = %v", err)
		}
	})

	t.Run("per-directory attempt cap rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		session := generousBudget(t, "cap-session")
		attempt := newAttempt(t, store, session, 40)
		prepared, err := store.prepareAttemptFailure(attempt, errors.New("boom"))
		if err != nil {
			t.Fatal(err)
		}
		filled := make(map[ScanAttemptID]committedFailure, maxFailureAttemptsPerDirectory)
		for index := range maxFailureAttemptsPerDirectory {
			var id ScanAttemptID
			id[0] = byte(index)
			id[1] = byte(index >> 8)
			id[2] = 1
			filled[id] = committedFailure{}
		}
		store.mu.Lock()
		store.attempts[attempt.directory] = attempt
		store.failures[attempt.directory] = filled
		_, commitErr := store.commitPreparedFailureLocked(attempt, prepared, errors.New("boom"), session)
		store.mu.Unlock()
		if !errors.Is(commitErr, ErrBudgetExceeded) {
			t.Fatalf("attempt cap error = %v", commitErr)
		}
	})

	t.Run("memory budget failure releases scan buffer before persisting failure payload", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		rootSession := generousBudget(t, "root-session")
		directory := prepareScannableDirectory(t, store, rootSession, 140, 141)
		// Allow enough room for the failure encoder (MaxCatalogPayloadBytes) plus
		// one large child, so the second child trips ErrBudgetExceeded while the
		// transient buffer still occupies most of the session memory budget.
		session, err := NewBudgetAccount("tight-scan-session", BudgetLimits{
			ActiveScans: 4, ScanWork: 100, Entries: 100, MemoryBytes: MaxCatalogPayloadBytes + (32 << 10),
		})
		if err != nil {
			t.Fatal(err)
		}
		bigRef, err := NewSourceReference(bytes.Repeat([]byte("r"), MaxSourceReferenceBytes))
		if err != nil {
			t.Fatal(err)
		}
		scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			for index := range 40 {
				child := scannedFile(t, byte(150+index), "item-"+string(rune('a'+index/10))+string(rune('0'+index%10)), 1)
				child.SourceReference = bigRef
				if err := request.Children.Add(ctx, child); err != nil {
					return ScanResult{}, err
				}
			}
			return ScanResult{}, nil
		})
		_, listErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
		var failure *DirectoryFailure
		if !errors.As(listErr, &failure) || failure.Kind != FailureKindBudget ||
			!failure.Transient || failure.RetryAfter != DefaultScanRetryCooldown {
			t.Fatalf("budget-exhausted scan error = %v, want retryable persisted FailureKindBudget", listErr)
		}
		if _, found, err := store.FailurePayload(context.Background(), directory, failure.AttemptID); err != nil || !found {
			t.Fatalf("persisted budget failure payload: found=%v err=%v", found, err)
		}
		if session.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("failed scan leaked session budget: %+v", session.Snapshot().Used)
		}
	})

	t.Run("sub-60KiB remaining share budget still commits empty directory and persists budget failures", func(t *testing.T) {
		process, err := NewBudgetAccount("tight-remaining-process", DefaultProcessBudgetLimits())
		if err != nil {
			t.Fatal(err)
		}
		// Reserve a tight share limit so committing the first directory leaves
		// ~12 KiB (far below MaxCatalogPayloadBytes = 60 KiB).
		const shareLimit = 48 << 10
		share, err := NewBudgetAccount("tight-remaining-share", BudgetLimits{
			ActiveScans: 4, ScanWork: 1_000, Entries: 1_000, MemoryBytes: shareLimit,
		})
		if err != nil {
			t.Fatal(err)
		}
		var nextAttempt, nextGen atomic.Uint32
		store, err := NewCatalogStore(StoreConfig{
			ShareInstance:  idValue[ShareInstance](1),
			ProcessBudget:  process,
			ShareBudget:    share,
			FailureEncoder: testFailureEncoder{},
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
		defer store.Close()

		session := generousBudget(t, "tight-remaining-session")
		dirs := prepareSelectedDirectories(t, store, session, 170, 171, 172, 173)
		filledDir, emptyDir, overflowDir := dirs[0], dirs[1], dirs[2]

		if _, err := store.ListChildren(context.Background(), filledDir, session, ScanOptions{}, DirectoryScannerFunc(
			func(ctx context.Context, request ScanRequest) (ScanResult, error) {
				return ScanResult{}, request.Children.Add(ctx, fixedSizeScannedFile(t, 180, "chunk", 32<<10))
			},
		)); err != nil {
			t.Fatalf("initial directory commit failed: %v", err)
		}
		remaining := shareLimit - share.Snapshot().Used.MemoryBytes
		if remaining == 0 || remaining >= MaxCatalogPayloadBytes {
			t.Fatalf("remaining share budget = %d, want 0 < remaining < %d", remaining, MaxCatalogPayloadBytes)
		}

		// Opening an empty directory must succeed with < 60 KiB remaining.
		emptyCommitted, err := store.ListChildren(context.Background(), emptyDir, session, ScanOptions{}, DirectoryScannerFunc(
			func(context.Context, ScanRequest) (ScanResult, error) { return ScanResult{}, nil },
		))
		if err != nil || emptyCommitted.EntryCount() != 0 {
			t.Fatalf("empty directory under sub-60KiB budget = %+v err=%v", emptyCommitted, err)
		}

		// A directory exceeding the remaining ~12 KiB budget must still persist a
		// retryable FailureKindBudget record instead of failing encoder reservation.
		_, overflowErr := store.ListChildren(context.Background(), overflowDir, session, ScanOptions{}, DirectoryScannerFunc(
			func(ctx context.Context, request ScanRequest) (ScanResult, error) {
				return ScanResult{}, request.Children.Add(ctx, fixedSizeScannedFile(t, 181, "too-big", 24<<10))
			},
		))
		var failure *DirectoryFailure
		if !errors.As(overflowErr, &failure) || failure.Kind != FailureKindBudget || !failure.Transient {
			t.Fatalf("overflow under sub-60KiB budget = %v, want retryable FailureKindBudget", overflowErr)
		}
		if _, found, err := store.FailurePayload(context.Background(), overflowDir, failure.AttemptID); err != nil || !found {
			t.Fatalf("persisted failure under sub-60KiB budget: found=%v err=%v", found, err)
		}
		if session.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("session budget leaked: %+v", session.Snapshot().Used)
		}
	})
}

func TestCatalogStorePagePayloadOwnsBytesAndRejectsWrongAddress(t *testing.T) {
	store, _, _ := newStore(t, nil)
	session := generousBudget(t, "page-object-session")
	root := idValue[DirectoryID](121)
	commitSyntheticRoot(t, store, rootCommit(
		t, idValue[ShareInstance](1), root, idValue[DirectoryGeneration](122),
		[]NodeRecord{selectedDirectory(t, root, 123, "selected")},
	), session)
	committed, found, err := store.Directory(context.Background(), root)
	if err != nil || !found {
		t.Fatalf("committed root = %+v found=%v err=%v", committed, found, err)
	}
	if _, found, err := store.PagePayload(
		context.Background(), root, idValue[DirectoryGeneration](124), 0,
	); err != nil || found {
		t.Fatalf("missing generation object = found=%v err=%v", found, err)
	}
	payload, found, err := store.PagePayload(context.Background(), root, committed.Generation(), 0)
	if err != nil || !found {
		t.Fatalf("read committed page: found=%v err=%v", found, err)
	}
	mutated := payload.Bytes()
	mutated[0] ^= 0xff
	reloaded, found, err := store.PagePayload(context.Background(), root, committed.Generation(), 0)
	if err != nil || !found || bytes.Equal(reloaded.Bytes(), mutated) {
		t.Fatalf("mutable byte copy affected committed page: found=%v err=%v", found, err)
	}

	store.mu.Lock()
	payload.address.PageIndex++
	state := store.directories[root]
	state.pages[0] = payload
	store.directories[root] = state
	store.mu.Unlock()
	if _, _, err := store.PagePayload(
		context.Background(), root, committed.Generation(), 0,
	); !errors.Is(err, ErrInvalidPagePayload) {
		t.Fatalf("mismatched page object = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PagePayload(
		context.Background(), root, committed.Generation(), 0,
	); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page object lookup = %v", err)
	}
}

func TestCancelledFailureCannotOverwriteSuccessorScan(t *testing.T) {
	t.Run("successor stays active for third receiver", func(t *testing.T) {
		store, process, share := newStore(t, nil)
		rootSession := generousBudget(t, "root-session")
		directory := prepareScannableDirectory(t, store, rootSession, 190, 191)

		encodeStarted := make(chan struct{})
		releaseEncode := make(chan struct{})
		store.failureEncoder = FailureEncoderFunc(func(record DirectoryFailureRecord) (FailurePayload, error) {
			close(encodeStarted)
			<-releaseEncode
			return testFailureEncoder{}.EncodeFailure(record)
		})

		scanBReady := make(chan struct{})
		releaseScanB := make(chan struct{})
		var scanCalls atomic.Int32
		scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			if scanCalls.Add(1) == 1 {
				return ScanResult{}, NewPermanentScanError(errors.New("attempt-a-permanent"))
			}
			if err := request.Children.Add(ctx, scannedFile(t, 192, "from-b", 4)); err != nil {
				return ScanResult{}, err
			}
			close(scanBReady)
			select {
			case <-ctx.Done():
				return ScanResult{}, ctx.Err()
			case <-releaseScanB:
				return ScanResult{}, nil
			}
		})

		session1 := generousBudget(t, "session-1")
		ctx1, cancel1 := context.WithCancel(context.Background())
		waiter1Err := make(chan error, 1)
		go func() {
			_, err := store.ListChildren(ctx1, directory, session1, ScanOptions{}, scanner)
			waiter1Err <- err
		}()
		<-encodeStarted

		store.mu.RLock()
		attemptA := store.attempts[directory]
		store.mu.RUnlock()
		if attemptA == nil {
			t.Fatal("attempt A was not registered while encoding failure")
		}

		// Cancel A's only waiter before A commits its encoded failure.
		cancel1()
		if err := <-waiter1Err; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter 1 cancel error = %v", err)
		}

		// Start successor scan B on the same directory and hold it mid-scan.
		session2 := generousBudget(t, "session-2")
		type listResult struct {
			committed CommittedDirectory
			err       error
		}
		waiter2Result := make(chan listResult, 1)
		go func() {
			committed, err := store.ListChildren(context.Background(), directory, session2, ScanOptions{}, scanner)
			waiter2Result <- listResult{committed: committed, err: err}
		}()
		<-scanBReady

		store.mu.RLock()
		attemptB := store.attempts[directory]
		store.mu.RUnlock()
		if attemptB == nil || attemptB == attemptA {
			t.Fatalf("successor attempt B did not take ownership: attemptA=%p attemptB=%p", attemptA, attemptB)
		}

		// Let cancelled attempt A finish encoding and attempt completion while B is still running.
		close(releaseEncode)
		<-attemptA.done

		store.mu.RLock()
		current := store.attempts[directory]
		store.mu.RUnlock()
		if current != attemptB {
			t.Fatalf("cancelled attempt A overwrote active attempt B: got=%p want=%p", current, attemptB)
		}
		if _, found, err := store.FailurePayload(context.Background(), directory, attemptA.id); err != nil || found {
			t.Fatalf("cancelled attempt A leaked failure payload: found=%v err=%v", found, err)
		}
		if session1.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("cancelled attempt A leaked session budget: %+v", session1.Snapshot().Used)
		}

		// A third receiver arriving while B is still running must join B rather
		// than observing A's permanent error.
		session3 := generousBudget(t, "session-3")
		waiter3JoinedB := make(chan struct{})
		waiter3Result := make(chan listResult, 1)
		go func() {
			var joined atomic.Bool
			observer := ScanProgressObserverFunc(func(_ context.Context, progress ScanProgress) error {
				if progress.AttemptID == attemptB.id && joined.CompareAndSwap(false, true) {
					close(waiter3JoinedB)
				}
				return nil
			})
			committed, err := store.ListChildren(context.Background(), directory, session3, ScanOptions{Progress: observer}, scanner)
			waiter3Result <- listResult{committed: committed, err: err}
		}()
		<-waiter3JoinedB

		close(releaseScanB)
		res2 := <-waiter2Result
		res3 := <-waiter3Result
		if res2.err != nil || res2.committed.EntryCount() != 1 {
			t.Fatalf("waiter 2 result = %+v, %v", res2.committed, res2.err)
		}
		if res3.err != nil || res3.committed != res2.committed {
			t.Fatalf("waiter 3 result = %+v, %v (want %+v)", res3.committed, res3.err, res2.committed)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if process.Snapshot().Used != (ResourceUsage{}) || share.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("store close leaked budgets: process=%+v share=%+v", process.Snapshot().Used, share.Snapshot().Used)
		}
	})

	t.Run("cancelled failure does not resurrect without successor", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		rootSession := generousBudget(t, "root-session")
		directory := prepareScannableDirectory(t, store, rootSession, 195, 196)

		encodeStarted := make(chan struct{})
		releaseEncode := make(chan struct{})
		store.failureEncoder = FailureEncoderFunc(func(record DirectoryFailureRecord) (FailurePayload, error) {
			close(encodeStarted)
			<-releaseEncode
			return testFailureEncoder{}.EncodeFailure(record)
		})

		var calls atomic.Int32
		scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
			if calls.Add(1) == 1 {
				return ScanResult{}, NewPermanentScanError(errors.New("cancelled-before-publish"))
			}
			return ScanResult{}, request.Children.Add(ctx, scannedFile(t, 197, "fresh", 2))
		})

		session1 := generousBudget(t, "session-1")
		ctx1, cancel1 := context.WithCancel(context.Background())
		waiter1Err := make(chan error, 1)
		go func() {
			_, err := store.ListChildren(ctx1, directory, session1, ScanOptions{}, scanner)
			waiter1Err <- err
		}()
		<-encodeStarted

		store.mu.RLock()
		attemptA := store.attempts[directory]
		store.mu.RUnlock()

		cancel1()
		if err := <-waiter1Err; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter 1 cancel error = %v", err)
		}

		close(releaseEncode)
		<-attemptA.done

		store.mu.RLock()
		current := store.attempts[directory]
		store.mu.RUnlock()
		if current != nil {
			t.Fatalf("cancelled attempt resurrected in s.attempts: %+v", current)
		}
		if _, found, err := store.FailurePayload(context.Background(), directory, attemptA.id); err != nil || found {
			t.Fatalf("cancelled attempt persisted failure payload: found=%v err=%v", found, err)
		}
		if session1.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("cancelled attempt leaked session budget: %+v", session1.Snapshot().Used)
		}

		session2 := generousBudget(t, "session-2")
		committed, err := store.ListChildren(context.Background(), directory, session2, ScanOptions{}, scanner)
		if err != nil || committed.EntryCount() != 1 {
			t.Fatalf("subsequent scan after cancelled failure = %+v, %v", committed, err)
		}
	})

	t.Run("superseded commit attempt does not publish over successor", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		rootSession := generousBudget(t, "root-session")
		directory := prepareScannableDirectory(t, store, rootSession, 200, 201)
		dirNode, found, err := store.Node(context.Background(), directory.NodeID())
		if err != nil || !found {
			t.Fatalf("load directory node: found=%v err=%v", found, err)
		}
		session := generousBudget(t, "superseded-commit-session")
		meter, err := newAttemptResourceMeter(store.hierarchy(session))
		if err != nil {
			t.Fatal(err)
		}
		defer meter.Close()

		attemptA := &scanAttempt{id: idValue[ScanAttemptID](202), generation: idValue[DirectoryGeneration](203), directory: directory}
		ctxB, cancelB := context.WithCancel(context.Background())
		defer cancelB()
		attemptB := &scanAttempt{
			id: idValue[ScanAttemptID](204), generation: idValue[DirectoryGeneration](205),
			directory: directory, ctx: ctxB, cancel: cancelB,
		}
		store.mu.Lock()
		store.attempts[directory] = attemptB
		store.mu.Unlock()

		commit := DirectoryCommit{directory: dirNode, generation: attemptA.generation}
		store.commitMu.Lock()
		_, _, commitErr := store.commitDirectoryLocked(context.Background(), commit, session, meter, false, attemptA)
		store.commitMu.Unlock()
		if !errors.Is(commitErr, context.Canceled) {
			t.Fatalf("superseded commit error = %v, want context.Canceled", commitErr)
		}
		if _, exists, err := store.Directory(context.Background(), directory); err != nil || exists {
			t.Fatalf("superseded commit published directory: exists=%v err=%v", exists, err)
		}
		store.mu.RLock()
		current := store.attempts[directory]
		store.mu.RUnlock()
		if current != attemptB {
			t.Fatalf("superseded commit altered successor attempt: got=%p want=%p", current, attemptB)
		}
		meter.Close()
		if session.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("superseded commit leaked session budget: %+v", session.Snapshot().Used)
		}
	})
}
