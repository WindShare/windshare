package catalog

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"
)

func testFailureRecord(t *testing.T, attempt, generation byte, previous ScanAttemptID, kind FailureKind) DirectoryFailureRecord {
	t.Helper()
	retryAfter := time.Duration(0)
	if kind == FailureKindTransientIO {
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
	if _, err := NewFailurePayload(make([]byte, MaxCatalogPayloadBytes+1)); err == nil {
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
		return &scanAttempt{
			id:         idValue[ScanAttemptID](attemptByte),
			generation: idValue[DirectoryGeneration](attemptByte + 1),
			directory:  idValue[DirectoryID](110),
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
		if _, err := store.persistAttemptFailure(attempt, errors.New("boom"), session); err == nil {
			t.Fatal("empty failure payload was persisted")
		}
	})

	t.Run("closed store rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		session := generousBudget(t, "closed-failure-session")
		attempt := newAttempt(t, store, session, 20)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.persistAttemptFailure(attempt, errors.New("boom"), session); !errors.Is(err, ErrCatalogClosed) {
			t.Fatalf("closed store failure persist = %v", err)
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
		if _, err := store.persistAttemptFailure(attempt, errors.New("boom"), tightSession); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("tight budget failure persist = %v", err)
		}
	})

	t.Run("per-directory attempt cap rejected", func(t *testing.T) {
		store, _, _ := newStore(t, nil)
		defer store.Close()
		session := generousBudget(t, "cap-session")
		attempt := newAttempt(t, store, session, 40)
		filled := make(map[ScanAttemptID]committedFailure, maxFailureAttemptsPerDirectory)
		for index := range maxFailureAttemptsPerDirectory {
			var id ScanAttemptID
			id[0] = byte(index)
			id[1] = byte(index >> 8)
			id[2] = 1
			filled[id] = committedFailure{}
		}
		store.failures[attempt.directory] = filled
		if _, err := store.persistAttemptFailure(attempt, errors.New("boom"), session); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("attempt cap error = %v", err)
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
		if !errors.As(listErr, &failure) || failure.Kind != FailureKindBudget {
			t.Fatalf("budget-exhausted scan error = %v, want persisted FailureKindBudget", listErr)
		}
		if _, found, err := store.FailurePayload(context.Background(), directory, failure.AttemptID); err != nil || !found {
			t.Fatalf("persisted budget failure payload: found=%v err=%v", found, err)
		}
		if session.Snapshot().Used != (ResourceUsage{}) {
			t.Fatalf("failed scan leaked session budget: %+v", session.Snapshot().Used)
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
