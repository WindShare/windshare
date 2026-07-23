package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type failureTestMeter struct {
	usage      ResourceUsage
	consumeErr error
	releaseErr error
}

func (meter *failureTestMeter) Consume(usage ResourceUsage) error {
	if meter.consumeErr != nil {
		return meter.consumeErr
	}
	next, ok := addUsage(meter.usage, usage)
	if !ok {
		return ErrBudgetExceeded
	}
	meter.usage = next
	return nil
}

func (meter *failureTestMeter) Release(usage ResourceUsage) error {
	if meter.releaseErr != nil {
		return meter.releaseErr
	}
	meter.usage = subtractUsage(meter.usage, usage)
	return nil
}

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
	if _, err := NewSealedFailureObject(nil); err == nil {
		t.Fatal("empty sealed failure object was accepted")
	}
	if _, err := NewSealedFailureObject(make([]byte, MaxCatalogPageObjectBytes+1)); err == nil {
		t.Fatal("oversized sealed failure object was accepted")
	}
	wantSealErr := errors.New("seal failed")
	sealer := FailureSealerFunc(func(DirectoryFailureRecord) (SealedFailureObject, error) {
		return SealedFailureObject{}, wantSealErr
	})
	if _, err := sealer.SealFailure(valid); !errors.Is(err, wantSealErr) {
		t.Fatalf("failure sealer function = %v", err)
	}

	for cause, want := range map[error]FailureKind{
		ErrDirectoryStale:   FailureKindStale,
		os.ErrPermission:    FailureKindPermission,
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

func TestFailureChainRejectsForksCyclesDuplicatesAndMissingPredecessors(t *testing.T) {
	root := testFailureRecord(t, 10, 20, ScanAttemptID{}, FailureKindPermanentIO)
	second := testFailureRecord(t, 11, 21, root.attempt, FailureKindTransientIO)
	third := testFailureRecord(t, 12, 22, second.attempt, FailureKindPermanentIO)
	if tail, err := validateFailureChain([]DirectoryFailureRecord{third, root, second}); err != nil || tail != third.attempt {
		t.Fatalf("valid shuffled chain tail = %x err=%v", tail, err)
	}
	invalid := root
	invalid.kind = FailureKindUnknown
	for name, records := range map[string][]DirectoryFailureRecord{
		"invalid record":      {invalid},
		"duplicate":           {root, root},
		"two roots":           {root, testFailureRecord(t, 13, 23, ScanAttemptID{}, FailureKindPermanentIO)},
		"fork":                {root, second, testFailureRecord(t, 14, 24, root.attempt, FailureKindPermanentIO)},
		"missing predecessor": {root, testFailureRecord(t, 15, 25, idValue[ScanAttemptID](99), FailureKindPermanentIO)},
		"disconnected cycle":  {root, failureRecordWithLink(t, 16, 26, 17), failureRecordWithLink(t, 17, 27, 16)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateFailureChain(records); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("invalid chain = %v", err)
			}
		})
	}
}

func failureRecordWithLink(t *testing.T, attempt, generation, previous byte) DirectoryFailureRecord {
	t.Helper()
	return testFailureRecord(
		t, attempt, generation, idValue[ScanAttemptID](previous), FailureKindPermanentIO,
	)
}

func TestMemoryFailureBackendIdempotencyReplayCancellationAndCorruption(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	record := testFailureRecord(t, 30, 40, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewSealedFailureObject([]byte("failure-a"))
	meter := &failureTestMeter{}
	prepareCalls := 0
	prepare := func(ResourceUsage) error { prepareCalls++; return nil }
	prepared, err := backend.CommitFailure(context.Background(), record, object, meter, prepare)
	if err != nil || prepared.Existing || prepareCalls != 1 {
		t.Fatalf("first memory failure commit = %+v err=%v calls=%d", prepared, err, prepareCalls)
	}
	firstUsage := prepared.Usage
	prepared, err = backend.CommitFailure(context.Background(), record, object, meter, prepare)
	if err != nil || !prepared.Existing || prepareCalls != 1 {
		t.Fatalf("idempotent memory failure commit = %+v err=%v calls=%d", prepared, err, prepareCalls)
	}
	different, _ := NewSealedFailureObject([]byte("failure-b"))
	if _, err := backend.CommitFailure(context.Background(), record, different, meter, prepare); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("conflicting failure object = %v", err)
	}
	wrongPrevious := testFailureRecord(t, 31, 41, idValue[ScanAttemptID](99), FailureKindPermanentIO)
	if _, err := backend.CommitFailure(context.Background(), wrongPrevious, different, meter, prepare); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("missing predecessor commit = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CommitFailure(cancelled, record, object, meter, prepare); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled memory commit = %v", err)
	}
	if _, err := backend.CommitFailure(context.Background(), DirectoryFailureRecord{}, object, meter, prepare); err == nil {
		t.Fatal("invalid memory failure commit was accepted")
	}
	failingMeter := &failureTestMeter{consumeErr: ErrBudgetExceeded}
	second := testFailureRecord(t, 32, 42, record.attempt, FailureKindPermanentIO)
	if _, err := backend.CommitFailure(context.Background(), second, different, failingMeter, prepare); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("memory failure budget = %v", err)
	}
	if _, err := backend.CommitFailure(context.Background(), second, different, &failureTestMeter{}, func(ResourceUsage) error {
		return errors.New("prepare failed")
	}); err == nil {
		t.Fatal("memory prepare failure was ignored")
	}
	loaded, found, err := backend.LoadFailureObject(context.Background(), record.directory, record.attempt)
	if err != nil || !found || string(loaded.Bytes()) != "failure-a" {
		t.Fatalf("loaded memory failure = %q found=%v err=%v", loaded.Bytes(), found, err)
	}
	if _, found, err := backend.LoadFailureObject(context.Background(), record.directory, idValue[ScanAttemptID](77)); err != nil || found {
		t.Fatalf("missing memory failure = found=%v err=%v", found, err)
	}
	if _, _, err := backend.LoadFailureObject(cancelled, record.directory, record.attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled memory failure load = %v", err)
	}
	active := 0
	if err := backend.ReplayFailures(context.Background(), func(replayed DirectoryFailureRecord, isActive bool) error {
		if replayed == record && isActive {
			active++
		}
		return nil
	}); err != nil || active != 1 {
		t.Fatalf("memory failure replay active=%d err=%v", active, err)
	}
	if err := backend.ReplayFailures(context.Background(), nil); err == nil {
		t.Fatal("nil replay consumer was accepted")
	}
	wantYieldErr := errors.New("yield failed")
	if err := backend.ReplayFailures(context.Background(), func(DirectoryFailureRecord, bool) error {
		return wantYieldErr
	}); !errors.Is(err, wantYieldErr) {
		t.Fatalf("memory replay consumer error = %v", err)
	}
	if err := backend.ReplayFailures(cancelled, func(DirectoryFailureRecord, bool) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled replay = %v", err)
	}
	recovered, err := backend.Recover(context.Background())
	wantRecovered, _ := addUsage(firstUsage, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes})
	if err != nil || recovered != wantRecovered {
		t.Fatalf("memory failure recovery usage = %+v want %+v err=%v", recovered, wantRecovered, err)
	}
	if _, err := backend.Recover(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled memory recovery = %v", err)
	}
	backend.mu.Lock()
	overflow := backend.failures[record.directory][record.attempt]
	overflow.usage.MemoryBytes = math.MaxUint64
	backend.failures[record.directory][record.attempt] = overflow
	backend.mu.Unlock()
	if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("overflowed memory failure recovery = %v", err)
	}
	backend.mu.Lock()
	overflow.usage = firstUsage
	backend.failures[record.directory][record.attempt] = overflow
	backend.mu.Unlock()
	backend.mu.Lock()
	otherRoot := testFailureRecord(t, 33, 43, ScanAttemptID{}, FailureKindPermanentIO)
	backend.failures[record.directory][otherRoot.attempt] = memoryFailure{
		record: otherRoot, object: []byte("other"), usage: ResourceUsage{MemoryBytes: 1},
	}
	backend.mu.Unlock()
	if err := backend.ReplayFailures(context.Background(), func(DirectoryFailureRecord, bool) error { return nil }); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("corrupt memory chain replay = %v", err)
	}
	backend.mu.Lock()
	delete(backend.failures[record.directory], otherRoot.attempt)
	backend.mu.Unlock()
	backend.mu.Lock()
	corrupt := backend.failures[record.directory][record.attempt]
	corrupt.object = nil
	backend.failures[record.directory][record.attempt] = corrupt
	backend.mu.Unlock()
	if _, _, err := backend.LoadFailureObject(context.Background(), record.directory, record.attempt); err == nil {
		t.Fatal("corrupt memory failure object was accepted")
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadFailureObject(context.Background(), record.directory, record.attempt); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory failure load = %v", err)
	}
	if err := backend.ReplayFailures(context.Background(), func(DirectoryFailureRecord, bool) error { return nil }); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory failure replay = %v", err)
	}
	if _, err := backend.CommitFailure(context.Background(), record, object, meter, prepare); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory failure commit = %v", err)
	}
	if _, err := backend.Recover(context.Background()); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed memory recovery = %v", err)
	}
}

func TestFailureMetadataChecksumBindsEveryAuthorityField(t *testing.T) {
	record := testFailureRecord(t, 50, 60, ScanAttemptID{}, FailureKindTransientIO)
	object, _ := NewSealedFailureObject([]byte("opaque failure"))
	valid := encodeFileFailureMeta(fileFailureMeta{
		record: record, commitment: object.commitment, objectSize: uint64(len(object.encoded)),
		spillBytes: failureMetaBytes + uint64(len(object.encoded)),
	})
	if _, err := decodeFileFailureMeta(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func([]byte){
		"checksum": func(encoded []byte) { encoded[len(encoded)-1] ^= 1 },
		"magic": func(encoded []byte) {
			encoded[0] ^= 1
			rechecksumFailureMeta(encoded)
		},
		"schema": func(encoded []byte) {
			binary.BigEndian.PutUint16(encoded[4:6], failureStorageSchema+1)
			rechecksumFailureMeta(encoded)
		},
		"reserved header": func(encoded []byte) {
			encoded[6] = 1
			rechecksumFailureMeta(encoded)
		},
		"reserved retry": func(encoded []byte) {
			encoded[90] = 1
			rechecksumFailureMeta(encoded)
		},
		"retry overflow": func(encoded []byte) {
			binary.BigEndian.PutUint64(encoded[96:104], uint64(MaxScanRetryCooldown/time.Millisecond)+1)
			rechecksumFailureMeta(encoded)
		},
		"retry flag": func(encoded []byte) {
			encoded[89] = 2
			rechecksumFailureMeta(encoded)
		},
		"zero object": func(encoded []byte) {
			binary.BigEndian.PutUint64(encoded[104:112], 0)
			rechecksumFailureMeta(encoded)
		},
		"spill mismatch": func(encoded []byte) {
			binary.BigEndian.PutUint64(encoded[144:152], 1)
			rechecksumFailureMeta(encoded)
		},
		"invalid kind": func(encoded []byte) {
			encoded[88] = byte(FailureKindUnknown)
			rechecksumFailureMeta(encoded)
		},
	} {
		t.Run(name, func(t *testing.T) {
			encoded := append([]byte(nil), valid...)
			mutate(encoded)
			if _, err := decodeFileFailureMeta(encoded); !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("corrupt metadata = %v", err)
			}
		})
	}
	if _, err := decodeFileFailureMeta(valid[:len(valid)-1]); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("short metadata = %v", err)
	}
}

func rechecksumFailureMeta(encoded []byte) {
	checksum := sha256Sum(encoded[:failureMetaBodyBytes])
	copy(encoded[failureMetaBodyBytes:], checksum[:])
}

func sha256Sum(encoded []byte) [32]byte {
	return sha256.Sum256(encoded)
}

func TestFileFailureBackendIdempotencyConflictAndClosedState(t *testing.T) {
	root := t.TempDir()
	share := idValue[ShareInstance](1)
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{Root: root, ShareInstance: share})
	if err != nil {
		t.Fatal(err)
	}
	record := testFailureRecord(t, 70, 80, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewSealedFailureObject([]byte("durable failure"))
	meter := &failureTestMeter{}
	prepareCalls := 0
	prepare := func(ResourceUsage) error { prepareCalls++; return nil }
	prepared, err := backend.CommitFailure(context.Background(), record, object, meter, prepare)
	if err != nil || prepared.Existing || prepareCalls != 1 {
		t.Fatalf("first file failure commit = %+v err=%v", prepared, err)
	}
	prepared, err = backend.CommitFailure(context.Background(), record, object, meter, prepare)
	if err != nil || !prepared.Existing || prepareCalls != 1 {
		t.Fatalf("idempotent file failure commit = %+v err=%v", prepared, err)
	}
	conflict, _ := NewSealedFailureObject([]byte("other durable failure"))
	if _, err := backend.CommitFailure(context.Background(), record, conflict, meter, prepare); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("conflicting durable failure = %v", err)
	}
	loaded, found, err := backend.LoadFailureObject(context.Background(), record.directory, record.attempt)
	if err != nil || !found || string(loaded.Bytes()) != "durable failure" {
		t.Fatalf("loaded durable failure = %q found=%v err=%v", loaded.Bytes(), found, err)
	}
	if err := backend.ReplayFailures(context.Background(), nil); err == nil {
		t.Fatal("nil file replay consumer was accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CommitFailure(cancelled, record, object, meter, prepare); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled file commit = %v", err)
	}
	if _, found, err := backend.LoadFailureObject(context.Background(), record.directory, idValue[ScanAttemptID](99)); err != nil || found {
		t.Fatalf("missing file failure = found=%v err=%v", found, err)
	}
	if _, _, err := backend.LoadFailureObject(cancelled, record.directory, record.attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled file failure load = %v", err)
	}
	wantYieldErr := errors.New("yield failed")
	if err := backend.ReplayFailures(context.Background(), func(DirectoryFailureRecord, bool) error {
		return wantYieldErr
	}); !errors.Is(err, wantYieldErr) {
		t.Fatalf("file replay consumer error = %v", err)
	}
	if err := backend.ReplayFailures(cancelled, func(DirectoryFailureRecord, bool) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled file replay = %v", err)
	}
	second := testFailureRecord(t, 71, 81, record.attempt, FailureKindPermanentIO)
	invalidChainEntry := filepath.Join(
		backend.failuresDir, hex.EncodeToString(record.directory[:]), "not-an-attempt",
	)
	if err := os.WriteFile(invalidChainEntry, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CommitFailure(
		context.Background(), second, conflict, &failureTestMeter{}, prepare,
	); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("invalid predecessor namespace = %v", err)
	}
	if err := os.Remove(invalidChainEntry); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CommitFailure(
		context.Background(), second, conflict, &failureTestMeter{},
		func(ResourceUsage) error { return errors.New("retain failed") },
	); err == nil {
		t.Fatal("file failure retain error was ignored")
	}
	if _, err := backend.CommitFailure(
		context.Background(), second, conflict, &failureTestMeter{consumeErr: ErrBudgetExceeded}, prepare,
	); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("file failure stage budget = %v", err)
	}
	foreign := second
	foreign.share = idValue[ShareInstance](99)
	if _, err := backend.CommitFailure(context.Background(), foreign, conflict, meter, prepare); err == nil {
		t.Fatal("foreign-share failure record was accepted")
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.LoadFailureObject(context.Background(), record.directory, record.attempt); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed file failure load = %v", err)
	}
	if _, err := backend.CommitFailure(context.Background(), record, object, meter, prepare); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed file failure commit = %v", err)
	}
	if err := backend.ReplayFailures(context.Background(), func(DirectoryFailureRecord, bool) error { return nil }); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed file failure replay = %v", err)
	}
	if err := backend.Destroy(); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestStageFailureFileRejectsBudgetAndInvalidTargets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "failure.object")
	if err := stageFailureFile(path, []byte("object"), &failureTestMeter{consumeErr: ErrBudgetExceeded}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("stage failure budget = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("budget-rejected stage created a file: %v", err)
	}
	invalid := filepath.Join(t.TempDir(), "missing", "failure.object")
	if err := stageFailureFile(invalid, []byte("object"), &failureTestMeter{}); err == nil {
		t.Fatal("invalid stage target was accepted")
	}
}

func TestFailureRecoveryVisitorsPropagateCancellationAndConsumerErrors(t *testing.T) {
	root := t.TempDir()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: root, ShareInstance: idValue[ShareInstance](1),
	})
	if err != nil {
		t.Fatal(err)
	}
	record := testFailureRecord(t, 90, 91, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewSealedFailureObject([]byte("visitor failure"))
	if _, err := backend.CommitFailure(
		context.Background(), record, object, &failureTestMeter{}, func(ResourceUsage) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	nonempty := backend.failurePath(record.directory, record.attempt)
	if empty, err := failureDirectoryIsEmpty(nonempty); err != nil || empty {
		t.Fatalf("committed attempt directory empty=%v err=%v", empty, err)
	}
	if _, err := failureDirectoryIsEmpty(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing failure directory appeared empty")
	}
	wantVisitErr := errors.New("visit failed")
	if err := backend.visitFailureDirectoriesLocked(context.Background(), func(DirectoryID, string) error {
		return wantVisitErr
	}); !errors.Is(err, wantVisitErr) {
		t.Fatalf("failure directory visitor error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := backend.visitFailureDirectoriesLocked(cancelled, func(DirectoryID, string) error {
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled failure directory visit = %v", err)
	}
	if err := backend.Destroy(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogStoreFailureObjectRequiresPublishedAttemptAuthority(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	record := testFailureRecord(t, 100, 101, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewSealedFailureObject([]byte("orphan failure"))
	if _, err := backend.CommitFailure(
		context.Background(), record, object, &failureTestMeter{}, func(ResourceUsage) error { return nil },
	); err != nil {
		t.Fatal(err)
	}
	store := &CatalogStore{
		backend: backend, usedAttempts: make(map[scanAttemptIdentity]struct{}),
	}
	if _, _, err := store.FailureObject(context.Background(), record.directory, record.attempt); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("unpublished attempt object = %v", err)
	}
	identity := scanAttemptIdentity{directory: record.directory, attempt: record.attempt}
	store.usedAttempts[identity] = struct{}{}
	loaded, found, err := store.FailureObject(context.Background(), record.directory, record.attempt)
	if err != nil || !found || string(loaded.Bytes()) != "orphan failure" {
		t.Fatalf("published attempt object = %q found=%v err=%v", loaded.Bytes(), found, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.FailureObject(cancelled, record.directory, record.attempt); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled store failure object = %v", err)
	}
	if _, found, err := store.FailureObject(
		context.Background(), record.directory, idValue[ScanAttemptID](102),
	); err != nil || found {
		t.Fatalf("missing store failure object = found=%v err=%v", found, err)
	}
	store.closed = true
	if _, _, err := store.FailureObject(context.Background(), record.directory, record.attempt); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed store failure object = %v", err)
	}
}

func TestFileFailureNamespaceHelpersRollbackEveryUnpublishedRename(t *testing.T) {
	root := t.TempDir()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: root, ShareInstance: idValue[ShareInstance](1),
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := idValue[DirectoryID](110)
	originalFailuresDir := backend.failuresDir
	backend.failuresDir = filepath.Join(root, "missing-parent", "failures")
	if _, _, err := backend.ensureFailureDirectoryLocked(directory); err == nil {
		t.Fatal("missing failure namespace parent was accepted")
	}
	backend.failuresDir = originalFailuresDir
	fileCollision := filepath.Join(originalFailuresDir, hex.EncodeToString(directory[:]))
	if err := os.WriteFile(fileCollision, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.ensureFailureDirectoryLocked(directory); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("failure directory file collision = %v", err)
	}
	if err := os.Remove(fileCollision); err != nil {
		t.Fatal(err)
	}
	if err := backend.publishFailureLocked(
		filepath.Join(root, "missing-transaction"), filepath.Join(root, "target"), root,
	); err == nil {
		t.Fatal("missing failure transaction was published")
	}
	transaction := filepath.Join(root, "transaction")
	if err := os.Mkdir(transaction, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "published")
	if err := backend.publishFailureLocked(transaction, target, filepath.Join(root, "missing-sync-directory")); err == nil {
		t.Fatal("unsynced failure rename was reported as published")
	}
	if _, err := os.Stat(transaction); err != nil {
		t.Fatalf("failed publication was not rolled back: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publication remained visible: %v", err)
	}
	originalStagingDir := backend.stagingDir
	backend.stagingDir = filepath.Join(root, "missing-staging-parent", "transactions")
	record := testFailureRecord(t, 111, 112, ScanAttemptID{}, FailureKindPermanentIO)
	object, _ := NewSealedFailureObject([]byte("staging"))
	if _, _, err := backend.stageFailureLocked(record, object, &failureTestMeter{}); err == nil {
		t.Fatal("missing staging namespace was accepted")
	}
	backend.stagingDir = originalStagingDir
	if err := backend.Destroy(); err != nil {
		t.Fatal(err)
	}
	if err := backend.visitFailureDirectoriesLocked(context.Background(), func(DirectoryID, string) error {
		return nil
	}); err == nil {
		t.Fatal("destroyed failure namespace was replayed")
	}
}

func TestFileFailureRecoveryStreamsBeyondOneDirectoryBatch(t *testing.T) {
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: t.TempDir(), ShareInstance: idValue[ShareInstance](1),
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := idValue[DirectoryID](120)
	directoryPath := filepath.Join(backend.failuresDir, hex.EncodeToString(directory[:]))
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var previous ScanAttemptID
	var wantTail ScanAttemptID
	for index := range failureDirectoryReadBatch + 1 {
		var attempt ScanAttemptID
		var generation DirectoryGeneration
		binary.BigEndian.PutUint32(attempt[12:], uint32(index+1))
		binary.BigEndian.PutUint32(generation[12:], uint32(index+1))
		record, err := newDirectoryFailureRecord(
			backend.share, directory, attempt, generation, previous, FailureKindPermanentIO, 0,
		)
		if err != nil {
			t.Fatal(err)
		}
		object, _ := NewSealedFailureObject([]byte{byte(index), byte(index >> 8), 1})
		meta := fileFailureMeta{
			record: record, commitment: object.commitment, objectSize: uint64(len(object.encoded)),
			spillBytes: failureMetaBytes + uint64(len(object.encoded)),
		}
		attemptPath := backend.failurePath(directory, attempt)
		if err := os.Mkdir(attemptPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(attemptPath, failureMetaName), encodeFileFailureMeta(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(attemptPath, failureObjectName), object.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		previous = attempt
		wantTail = attempt
	}
	records, err := backend.failureRecordsLocked(directory)
	if err != nil || len(records) != failureDirectoryReadBatch+1 {
		t.Fatalf("streamed failure records = %d err=%v", len(records), err)
	}
	if tail, err := validateFailureChain(records); err != nil || tail != wantTail {
		t.Fatalf("streamed failure chain tail = %x want %x err=%v", tail, wantTail, err)
	}
	if err := backend.Destroy(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogStorePageObjectRejectsMissingAndMismatchedAuthority(t *testing.T) {
	backend := NewMemoryCatalogBackend()
	store, _, _ := newStore(t, backend, nil)
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
	if _, found, err := store.PageObject(
		context.Background(), root, idValue[DirectoryGeneration](124), 0,
	); err != nil || found {
		t.Fatalf("missing generation object = found=%v err=%v", found, err)
	}
	backend.mu.Lock()
	directory := backend.directories[root]
	directory.pageObjects[0] = []byte("different committed object")
	backend.directories[root] = directory
	backend.mu.Unlock()
	if _, _, err := store.PageObject(
		context.Background(), root, committed.Generation(), 0,
	); !errors.Is(err, ErrCorruptCatalogStorage) {
		t.Fatalf("mismatched page object = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PageObject(
		context.Background(), root, committed.Generation(), 0,
	); !errors.Is(err, ErrCatalogClosed) {
		t.Fatalf("closed page object lookup = %v", err)
	}
}
