package catalog

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
)

func commitContractFixture(t *testing.T) (NodeRecord, NodeRecord) {
	t.Helper()
	root := idValue[DirectoryID](200)
	parent := selectedDirectory(t, root, 201, "parent")
	parentID, _ := parent.DirectoryID()
	child, err := scannedFile(t, 202, "child", 1).nodeRecord(parentID)
	if err != nil {
		t.Fatal(err)
	}
	return parent, child
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

func TestBudgetReservationRejectsInvalidStateTransitions(t *testing.T) {
	var nilAccount *BudgetAccount
	if err := nilAccount.add(ResourceUsage{Entries: 1}); err == nil {
		t.Fatal("nil account admitted usage")
	}
	if _, err := reserveAccounts(nil, ResourceUsage{}); err == nil {
		t.Fatal("empty account list created a reservation")
	}
	var nilReservation *BudgetReservation
	if nilReservation.active() {
		t.Fatal("nil reservation reported active")
	}
	if err := nilReservation.Grow(ResourceUsage{Entries: 1}); err == nil {
		t.Fatal("nil reservation grew")
	}
	if err := nilReservation.Shrink(ResourceUsage{Entries: 1}); err == nil {
		t.Fatal("nil reservation shrank")
	}
	if err := nilReservation.keep(ResourceUsage{}); err == nil {
		t.Fatal("nil reservation kept usage")
	}
	if err := nilReservation.dropAccount(nil); err == nil {
		t.Fatal("nil reservation dropped an account")
	}

	process := generousBudget(t, "edge-process")
	share := generousBudget(t, "edge-share")
	session := generousBudget(t, "edge-session")
	reservation, err := ReserveHierarchy(
		BudgetHierarchy{Process: process, Share: share, Session: session},
		ResourceUsage{MemoryBytes: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reservation.active() {
		t.Fatal("live reservation reported inactive")
	}
	if err := reservation.Grow(ResourceUsage{MemoryBytes: math.MaxUint64}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("overflow Grow = %v", err)
	}
	if err := reservation.Shrink(ResourceUsage{MemoryBytes: 11}); err == nil {
		t.Fatal("shrink exceeding reservation succeeded")
	}
	reservation.Release()
	reservation.Release()
	if reservation.active() {
		t.Fatal("released reservation reported active")
	}
	if err := reservation.Grow(ResourceUsage{MemoryBytes: 1}); err == nil {
		t.Fatal("released reservation grew")
	}
	if err := reservation.Shrink(ResourceUsage{}); err == nil {
		t.Fatal("released reservation shrank")
	}
	if err := reservation.keep(ResourceUsage{}); err == nil {
		t.Fatal("released reservation kept usage")
	}
	if err := reservation.dropAccount(session); err == nil {
		t.Fatal("released reservation dropped an account")
	}
}

func TestCommitAdmissionRejectsInvalidOwnershipAndClosedStores(t *testing.T) {
	parent, child := commitContractFixture(t)
	generation := idValue[DirectoryGeneration](210)
	valid := DirectoryCommit{
		directory: parent, generation: generation, children: []NodeRecord{child},
	}
	if err := validateDirectoryCommit(ShareInstance{}, valid, false); err == nil {
		t.Fatal("commit without a share identity was accepted")
	}
	if err := validateDirectoryCommit(idValue[ShareInstance](1), DirectoryCommit{
		directory: parent, generation: generation,
		children: []NodeRecord{child}, omittedCount: MaxDirectoryEntries,
	}, false); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("entry plus omission overflow error = %v", err)
	}
	if err := validateDirectoryCommit(idValue[ShareInstance](1), DirectoryCommit{
		directory: parent, generation: generation, synthetic: true,
	}, true); err == nil {
		t.Fatal("synthetic commit accepted a non-synthetic directory")
	}

	store, _, _ := newStore(t, nil)
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

func TestExistingCommitReplayValidatesEveryBinding(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()
	parent, child := commitContractFixture(t)
	generation := idValue[DirectoryGeneration](214)
	commit := DirectoryCommit{
		directory: parent, generation: generation, children: []NodeRecord{child},
	}
	existing, err := store.CommitDirectory(context.Background(), commit, generousBudget(t, "publish-session"))
	if err != nil {
		t.Fatal(err)
	}
	otherChild, err := scannedFile(t, 215, "other", 1).nodeRecord(parent.directoryID)
	if err != nil {
		t.Fatal(err)
	}
	storedState := store.directories[parent.directoryID]

	t.Run("committed identity mismatch", func(t *testing.T) {
		mutated := storedState
		mutated.committed.shareInstance = idValue[ShareInstance](216)
		if _, err := store.validateExistingCommitLocked(commit, mutated); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("identity mismatch = %v", err)
		}
	})
	t.Run("entry mismatch", func(t *testing.T) {
		other := DirectoryCommit{directory: parent, generation: generation, children: []NodeRecord{otherChild}}
		if _, err := store.validateExistingCommitLocked(other, storedState); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("entry mismatch = %v", err)
		}
	})
	t.Run("directory record missing", func(t *testing.T) {
		delete(store.nodes, parent.NodeID())
		defer func() { store.nodes[parent.NodeID()] = parent }()
		if _, err := store.validateExistingCommitLocked(commit, storedState); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("missing directory node = %v", err)
		}
	})
	t.Run("child record missing", func(t *testing.T) {
		delete(store.nodes, child.NodeID())
		defer func() { store.nodes[child.NodeID()] = child }()
		if _, err := store.validateExistingCommitLocked(commit, storedState); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("missing child node = %v", err)
		}
	})
	t.Run("corrupt page payload", func(t *testing.T) {
		corrupt := storedState
		corrupt.pages = []PagePayload{{encoded: []byte{0xff}}}
		if _, err := store.validateExistingCommitLocked(commit, corrupt); !errors.Is(err, ErrInvalidPagePayload) {
			t.Fatalf("corrupt page payload = %v", err)
		}
	})
	t.Run("page count shorter than children", func(t *testing.T) {
		short := storedState
		short.pages = nil
		if _, err := store.validateExistingCommitLocked(commit, short); !errors.Is(err, ErrGenerationConflict) {
			t.Fatalf("short pages = %v", err)
		}
	})
	if got, err := store.validateExistingCommitLocked(commit, storedState); err != nil || got != existing {
		t.Fatalf("valid replay = %+v err=%v", got, err)
	}
}

type contractScanWork func(uint64) error

func (work contractScanWork) Consume(units uint64) error { return work(units) }

func contractStoreConfig(t *testing.T) StoreConfig {
	t.Helper()
	return StoreConfig{
		ShareInstance:  idValue[ShareInstance](230),
		ProcessBudget:  generousBudget(t, "contract-process"),
		ShareBudget:    generousBudget(t, "contract-share"),
		FailureEncoder: testFailureEncoder{},
	}
}

func contractBudget(t *testing.T, name string, memory uint64) *BudgetAccount {
	t.Helper()
	account, err := NewBudgetAccount(name, BudgetLimits{
		ActiveScans: 8, ScanWork: 8, Entries: 8, MemoryBytes: memory,
	})
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func TestCatalogCloseCancelsUnfinishedAttempts(t *testing.T) {
	store, _, _ := newStore(t, nil)
	attemptContext, cancel := context.WithCancel(context.Background())
	attempt := &scanAttempt{ctx: attemptContext, cancel: cancel}
	store.attempts[idValue[DirectoryID](231)] = attempt
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(attemptContext), context.Canceled) {
		t.Fatal("store close left an unfinished scan attempt live")
	}
	store.completeSucceededAttempt(&scanAttempt{completed: true}, CommittedDirectory{})
	store.completeFailedAttempt(&scanAttempt{completed: true}, errors.New("ignored"), nil, nil, nil)
}

func TestListChildrenPreservesKnownAuthority(t *testing.T) {
	store, _, _ := newStore(t, nil)
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
	store.mu.Lock()
	original := store.nodes[directory.NodeID()]
	store.nodes[directory.NodeID()] = selectedDirectory(t, root, 234, "wrong")
	store.mu.Unlock()
	if _, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, never); err == nil {
		t.Fatal("directory listing accepted a substituted node record")
	}
	store.mu.Lock()
	store.nodes[directory.NodeID()] = original
	store.mu.Unlock()
}

func TestScanAdmissionAndChildSinkEnforceLocalBudgetAndContext(t *testing.T) {
	store, _, _ := newStore(t, nil)
	defer store.Close()
	limitedSession := contractBudget(t, "limited-session", 1)
	if _, err := store.admitAttempt(idValue[DirectoryID](235), ScanAttemptID{}, limitedSession); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("session ledger admission error = %v", err)
	}

	config := contractStoreConfig(t)
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
	if err := collector.Add(commitStoreMissingContext(), ScannedChild{}); err == nil {
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

// commitStoreMissingContext keeps intentional nil-capability coverage distinct
// from accidental nil Context arguments in ordinary tests.
func commitStoreMissingContext() context.Context { return nil }

func TestRunAttemptContainsAdmissionAndPostScanValidationFailures(t *testing.T) {
	store, _, _ := newStore(t, nil)
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

	candidate, err := NewCatalogStore(contractStoreConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	candidateSession := generousBudget(t, "candidate-session")
	directory := prepareScannableDirectory(t, candidate, candidateSession, 241, 242)
	overflow := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		return ScanResult{OmittedCount: MaxDirectoryEntries + 1}, nil
	})
	if _, err := candidate.ListChildren(context.Background(), directory, candidateSession, ScanOptions{}, overflow); !errors.Is(err, ErrPageLimit) {
		t.Fatalf("omission overflow error = %v", err)
	}
}

func multiPageScannedFile(t *testing.T, index int) ScannedChild {
	t.Helper()
	name := fmt.Sprintf("item-%04d", index)
	locator, err := NewSourceReference([]byte("object:" + name))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewSourceIdentity([]byte("source:" + name))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewVersionCandidate([]byte("version:" + name))
	if err != nil {
		t.Fatal(err)
	}
	var id FileID
	binary.BigEndian.PutUint64(id[8:], uint64(index)+1)
	return ScannedChild{
		FileID: id, Name: name, SourceReference: locator, SourceIdentity: identity,
		VersionCandidate: candidate, ExpectedSize: uint64(index),
	}
}

func TestCommitReservesPageBudgetIncrementallyAndTracksPeakUsage(t *testing.T) {
	const entryCount = MaxCatalogPageEntries*2 + 8

	store, _, share := newStore(t, nil)
	defer store.Close()
	startup := generousBudget(t, "startup")
	directory := prepareScannableDirectory(t, store, startup, 243, 244)
	session := generousBudget(t, "peak-session")
	var scanOnlyPeak uint64
	scanner := DirectoryScannerFunc(func(ctx context.Context, request ScanRequest) (ScanResult, error) {
		for index := range entryCount {
			if err := request.Children.Add(ctx, multiPageScannedFile(t, index)); err != nil {
				return ScanResult{}, err
			}
		}
		scanOnlyPeak = session.Snapshot().Used.MemoryBytes
		return ScanResult{}, nil
	})
	committed, err := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	if err != nil {
		t.Fatal(err)
	}
	if committed.PageCount() != 3 {
		t.Fatalf("page count = %d, want 3", committed.PageCount())
	}
	sessionSnap := session.Snapshot()
	if sessionSnap.Used != (ResourceUsage{}) {
		t.Fatalf("completed scan retained session budget: %+v", sessionSnap.Used)
	}
	if sessionSnap.Peak.MemoryBytes <= scanOnlyPeak {
		t.Fatalf("session peak %d did not capture post-scan page building above scan-only peak %d", sessionSnap.Peak.MemoryBytes, scanOnlyPeak)
	}
	// Exact directory retained bytes = scanOnlyPeak + memoryDirectoryOverhead + all encoded pages.
	var encodedPageBytes uint64
	for pageIndex := uint32(0); pageIndex < committed.PageCount(); pageIndex++ {
		payload, found, err := store.PagePayload(context.Background(), directory, committed.Generation(), pageIndex)
		if err != nil || !found {
			t.Fatalf("load page %d: found=%v err=%v", pageIndex, found, err)
		}
		encodedPageBytes += memoryPageOverhead + uint64(len(payload.encoded))
	}
	if wantMinPeak := scanOnlyPeak + memoryDirectoryOverhead + encodedPageBytes; sessionSnap.Peak.MemoryBytes < wantMinPeak {
		t.Fatalf("session peak %d < retained working set %d", sessionSnap.Peak.MemoryBytes, wantMinPeak)
	}
	if share.Snapshot().Peak.MemoryBytes < share.Snapshot().Used.MemoryBytes {
		t.Fatalf("share peak %d < retained %d", share.Snapshot().Peak.MemoryBytes, share.Snapshot().Used.MemoryBytes)
	}

	// A session that fits all scanned children plus one full page encoding
	// reservation, but not the full multi-page batch, must reject during page
	// building without publishing or leaking session budget.
	tightStore, _, _ := newStore(t, nil)
	defer tightStore.Close()
	tightDirectory := prepareScannableDirectory(t, tightStore, generousBudget(t, "tight-startup"), 245, 246)
	firstPageEntries := make([]Entry, 0, MaxCatalogPageEntries)
	for index := range MaxCatalogPageEntries {
		record, err := multiPageScannedFile(t, index).nodeRecord(tightDirectory)
		if err != nil {
			t.Fatal(err)
		}
		firstPageEntries = append(firstPageEntries, record.Entry())
	}
	firstPageReservation := pageEncodingReservationBytes(firstPageEntries)
	tightLimit := scanOnlyPeak + memoryDirectoryOverhead + firstPageReservation + encodedPageBytes/4
	tightSession, err := NewBudgetAccount("tight-page-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 100_000, Entries: 100_000, MemoryBytes: tightLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tightStore.ListChildren(context.Background(), tightDirectory, tightSession, ScanOptions{}, scanner); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("tight multi-page commit error = %v", err)
	}
	if _, exists, err := tightStore.Directory(context.Background(), tightDirectory); err != nil || exists {
		t.Fatalf("over-budget directory published: exists=%v err=%v", exists, err)
	}
	if tightSession.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("rejected multi-page commit leaked session budget: %+v", tightSession.Snapshot().Used)
	}

	// Direct CommitDirectory must charge children before building any page.
	directStore, _, _ := newStore(t, nil)
	defer directStore.Close()
	parent := selectedDirectory(t, idValue[DirectoryID](247), 248, "direct")
	parentID, _ := parent.DirectoryID()
	children := make([]NodeRecord, 0, entryCount)
	for index := range entryCount {
		record, err := multiPageScannedFile(t, index).nodeRecord(parentID)
		if err != nil {
			t.Fatal(err)
		}
		children = append(children, record)
	}
	directSession, err := NewBudgetAccount("direct-tight-session", BudgetLimits{
		ActiveScans: 4, ScanWork: 100_000, Entries: 100_000, MemoryBytes: uint64(CatalogPageMemoryOverhead+MaxCatalogPayloadBytes) * 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	directCommit := DirectoryCommit{
		directory: parent, generation: idValue[DirectoryGeneration](249), children: children,
	}
	if _, err := directStore.CommitDirectory(context.Background(), directCommit, directSession); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("direct over-budget commit error = %v", err)
	}
	if directSession.Snapshot().Peak.MemoryBytes != 0 || directSession.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("direct commit allocated page budget before upfront child charge: %+v", directSession.Snapshot())
	}
}
