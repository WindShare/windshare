package outputsession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestDirectoryDispositionSeparatesRootAndDescendantAuthority(t *testing.T) {
	tests := []struct {
		disposition DirectoryDisposition
		root        bool
		valid       bool
	}{
		{DirectoryCallerProvidedRoot, true, true},
		{DirectoryCallerProvidedRoot, false, false},
		{DirectoryAuthorityCreatedRoot, true, true},
		{DirectoryAuthorityCreatedRoot, false, false},
		{DirectoryAuthorityCreatedDescendant, true, false},
		{DirectoryAuthorityCreatedDescendant, false, true},
		{DirectoryPreexistingDescendant, true, false},
		{DirectoryPreexistingDescendant, false, true},
		{0, true, false},
		{0, false, false},
	}
	for _, test := range tests {
		if got := test.disposition.validFor(test.root); got != test.valid {
			t.Fatalf("disposition=%d root=%t valid=%t want=%t", test.disposition, test.root, got, test.valid)
		}
	}
}

const concurrencyTestTimeout = 3 * time.Second

type sessionLockProbe struct {
	state           sessionState
	directoryClaims int
	metadataBytes   uint64
}

// Capturing live ownership state makes callback re-entry prove both unlock
// ordering and the coherence of the state exposed at each external boundary.
func captureSessionLockProbe(session *Session) sessionLockProbe {
	session.mu.Lock()
	defer session.mu.Unlock()
	return sessionLockProbe{
		state:           session.state,
		directoryClaims: len(session.directoryClaims),
		metadataBytes:   session.metadataBytes,
	}
}

func TestDirectoryAdmissionCoalescesWithoutHoldingSessionLock(t *testing.T) {
	materializeStarted := make(chan struct{})
	releaseMaterialize := make(chan struct{})
	coalesced := make(chan struct{}, 1)
	var (
		observedSession *Session
		eventsMu        sync.Mutex
		events          []TraceEvent
		probes          []sessionLockProbe
	)
	recordLockProbe := func() {
		probe := captureSessionLockProbe(observedSession)
		eventsMu.Lock()
		probes = append(probes, probe)
		eventsMu.Unlock()
	}
	fixture := newTestFixture(t, func(config *Config) {
		authority := config.Directories.(*fakeDirectoryAuthority)
		authority.canonicalLocatorKey = func(path string) (string, error) {
			recordLockProbe()
			if path == "" {
				return "locator:root", nil
			}
			return "locator:" + path, nil
		}
		authority.materialize = func(context.Context, DirectoryClaim) (DirectoryMaterialization, error) {
			recordLockProbe()
			select {
			case <-materializeStarted:
			default:
				close(materializeStarted)
			}
			<-releaseMaterialize
			return DirectoryMaterialization{Cut: MutationStable, Disposition: DirectoryCallerProvidedRoot}, nil
		}
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			// Reentrant inspection would deadlock immediately if tracing happened
			// while the state transition mutex was retained.
			recordLockProbe()
			eventsMu.Lock()
			events = append(events, event)
			eventsMu.Unlock()
			if event.Decision == TraceCoalesced {
				select {
				case coalesced <- struct{}{}:
				default:
				}
			}
		})
	})
	observedSession = fixture.session

	type result struct {
		admission transfer.DirectoryAdmission
		err       error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() {
		admission, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory)
		first <- result{admission: admission, err: err}
	}()
	mustSignal(t, materializeStarted)
	go func() {
		admission, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory)
		second <- result{admission: admission, err: err}
	}()
	mustSignal(t, coalesced)
	select {
	case result := <-second:
		t.Fatalf("coalesced admission returned before materialization: %+v", result)
	default:
	}
	close(releaseMaterialize)
	firstResult := mustResult(t, first)
	secondResult := mustResult(t, second)
	if firstResult.err != nil || secondResult.err != nil || !firstResult.admission.Equal(secondResult.admission) {
		t.Fatalf("first=(%v,%v) second=(%v,%v)", firstResult.admission.IsZero(), firstResult.err, secondResult.admission.IsZero(), secondResult.err)
	}
	materialized, _ := fixture.directories.counts()
	if materialized != 1 {
		t.Fatalf("materialize calls=%d want=1", materialized)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if len(events) < 3 {
		t.Fatalf("trace events=%d want reservation/coalescing/admission", len(events))
	}
	if len(probes) < 3 {
		t.Fatalf("after-unlock probes=%d want at least canonicalization/materialization/trace", len(probes))
	}
	sawReservedClaim := false
	for _, probe := range probes {
		if probe.state != sessionOpen {
			t.Fatalf("after-unlock probe state=%d want open", probe.state)
		}
		if probe.directoryClaims == 1 && probe.metadataBytes > 0 {
			sawReservedClaim = true
		}
	}
	if !sawReservedClaim {
		t.Fatal("after-unlock probes never observed the reserved directory ownership")
	}
}

func TestConflictingPendingDirectoryFailsBeforeBlockedMaterializationReturns(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).materialize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryMaterialization, error) {
			close(started)
			<-release
			return DirectoryMaterialization{Cut: MutationStable, Disposition: DirectoryCallerProvidedRoot}, nil
		}
	})
	first := make(chan error, 1)
	go func() {
		_, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory)
		first <- err
	}()
	mustSignal(t, started)
	conflict := fixture.rootDirectory
	conflict.DirectoryID = identity[catalog.DirectoryID](77)
	conflictResult := make(chan error, 1)
	go func() {
		_, err := fixture.session.AdmitDirectory(context.Background(), conflict)
		conflictResult <- err
	}()
	err := mustResult(t, conflictResult)
	if !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("conflict error=%v", err)
	}
	close(release)
	if err := mustResult(t, first); err != nil {
		t.Fatalf("already-mutating exact claim should reach its stable admission: %v", err)
	}
}

func TestDirectoryReservationRollsBackExactlyAtStableNoChangeCut(t *testing.T) {
	calls := 0
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).materialize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryMaterialization, error) {
			calls++
			if calls == 1 {
				return DirectoryMaterialization{Cut: MutationNoChange}, context.Canceled
			}
			return DirectoryMaterialization{Cut: MutationStable, Disposition: DirectoryCallerProvidedRoot}, nil
		}
	})
	if _, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory); !errors.Is(err, context.Canceled) {
		t.Fatalf("first admission error=%v", err)
	}
	fixture.session.mu.Lock()
	if len(fixture.session.directoryClaims) != 0 || len(fixture.session.nodeClaims) != 0 ||
		len(fixture.session.pathClaims) != 0 || len(fixture.session.locatorClaims) != 0 ||
		fixture.session.metadataBytes != 0 || fixture.session.rootClaim != 0 {
		fixture.session.mu.Unlock()
		t.Fatal("failed no-change admission retained authority or budget")
	}
	fixture.session.mu.Unlock()
	if admission, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory); err != nil || admission.IsZero() {
		t.Fatalf("retry admission zero=%v err=%v", admission.IsZero(), err)
	}
}

func TestDirectoryClaimBudgetRejectsBeforeMaterialization(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.DirectoryClaims = 1
		config.Limits.NodeClaims = 2
		config.Limits.ActiveFileClaims = 1
	})
	root := fixture.admitRoot(context.Background())
	if _, err := fixture.session.AdmitDirectory(
		context.Background(),
		fixture.childDirectory(root, 43, "child"),
	); !errors.Is(err, ErrResourceBudget) {
		t.Fatalf("directory-count budget error=%v", err)
	}
	materialized, _ := fixture.directories.counts()
	fixture.session.mu.Lock()
	claims := len(fixture.session.directoryClaims)
	fixture.session.mu.Unlock()
	if materialized != 1 || claims != 1 {
		t.Fatalf("over-budget directory mutated: materializations=%d claims=%d", materialized, claims)
	}
}

func TestInvalidUTF8LocatorFailsBeforeClaimReservation(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Locator.(*fakeDirectoryAuthority).canonicalLocatorKey = func(string) (string, error) {
			return string([]byte{0xff}), nil
		}
	})
	if _, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("invalid locator error=%v", err)
	}
	fixture.session.mu.Lock()
	claims := len(fixture.session.directoryClaims)
	fixture.session.mu.Unlock()
	materialized, _ := fixture.directories.counts()
	if claims != 0 || materialized != 0 {
		t.Fatalf("invalid locator retained claims=%d materializations=%d", claims, materialized)
	}
}

func TestDirectoryMetadataBudgetChargesLocatorBeforeMaterialization(t *testing.T) {
	rootLocator := "locator:root"
	root := transfer.OutputDirectory{
		DirectoryID: identity[catalog.DirectoryID](21),
		Generation:  identity[catalog.DirectoryGeneration](31),
	}
	rootCharge := directoryMetadataBytes(root, rootLocator)
	fixture := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.DirectoryMetadataBytes = rootCharge + 128
	})
	rootAdmission := fixture.admitRoot(context.Background())
	child := fixture.childDirectory(rootAdmission, 44, "child")
	childCharge := directoryMetadataBytes(child, "locator:child")
	fixture.session.mu.Lock()
	fixture.session.limits.DirectoryMetadataBytes = rootCharge + childCharge - 1
	fixture.session.mu.Unlock()
	if _, err := fixture.session.AdmitDirectory(context.Background(), child); !errors.Is(err, ErrResourceBudget) {
		t.Fatalf("child budget error=%v", err)
	}
	materialized, _ := fixture.directories.counts()
	if materialized != 1 {
		t.Fatalf("over-budget child reached materialization: calls=%d", materialized)
	}
	fixture.session.mu.Lock()
	defer fixture.session.mu.Unlock()
	if len(fixture.session.directoryClaims) != 1 || fixture.session.metadataBytes != rootCharge {
		t.Fatalf("claims=%d metadata=%d want claims=1 metadata=%d",
			len(fixture.session.directoryClaims), fixture.session.metadataBytes, rootCharge)
	}
}

func TestDirectoryMetadataBudgetAcceptsExactBoundaryAndRejectsOneByteOver(t *testing.T) {
	root := transfer.OutputDirectory{
		DirectoryID: identity[catalog.DirectoryID](21),
		Generation:  identity[catalog.DirectoryGeneration](31),
	}
	const rootCharge uint64 = 92
	if charge := directoryMetadataBytes(root, "locator:root"); charge != rootCharge {
		t.Fatalf("root metadata charge=%d want=%d", charge, rootCharge)
	}

	exact := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.DirectoryMetadataBytes = rootCharge
	})
	if _, err := exact.session.AdmitDirectory(context.Background(), exact.rootDirectory); err != nil {
		t.Fatalf("exact-boundary admission: %v", err)
	}
	exact.session.mu.Lock()
	exactBytes := exact.session.metadataBytes
	exact.session.mu.Unlock()
	if exactBytes != rootCharge {
		t.Fatalf("retained metadata=%d want=%d", exactBytes, rootCharge)
	}

	over := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.DirectoryMetadataBytes = rootCharge - 1
	})
	if _, err := over.session.AdmitDirectory(context.Background(), over.rootDirectory); !errors.Is(err, ErrResourceBudget) {
		t.Fatalf("one-byte-over admission error=%v", err)
	}
	over.session.mu.Lock()
	retainedClaims := len(over.session.directoryClaims)
	retainedBytes := over.session.metadataBytes
	over.session.mu.Unlock()
	materialized, _ := over.directories.counts()
	if retainedClaims != 0 || retainedBytes != 0 || materialized != 0 {
		t.Fatalf("one-byte-over rollback claims=%d bytes=%d materializations=%d",
			retainedClaims, retainedBytes, materialized)
	}
}

func TestDirectoryMetadataChargeCountsUTF8FieldsAndFixedIndexesOnce(t *testing.T) {
	fixture := newTestFixture(t, nil)
	parent := fixture.admitRoot(context.Background())
	child := fixture.childDirectory(parent, 52, "\u76ee\u5f55")
	const childCharge uint64 = 132
	if charge := directoryMetadataBytes(child, "locator:\u76ee\u5f55"); charge != childCharge {
		t.Fatalf("UTF-8 metadata charge=%d want=%d", charge, childCharge)
	}
}

func TestDirectoryFinalizationSealsThenWaitsForActiveFile(t *testing.T) {
	finalizeCalled := make(chan struct{}, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).finalize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryFinalization, error) {
			finalizeCalled <- struct{}{}
			return FinalizedDirectory(), nil
		}
	})
	ctx := testContext()
	rootAdmission := fixture.admitRoot(ctx)
	child := fixture.childDirectory(rootAdmission, 40, "child")
	childAdmission, err := fixture.session.AdmitDirectory(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	file := fixture.outputFile(childAdmission, 70, "child/file.bin")
	start, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active transaction")
	}
	finalized := make(chan error, 1)
	go func() {
		_, err := fixture.session.FinalizeDirectory(context.Background(), childAdmission)
		finalized <- err
	}()
	waitForDirectoryState(t, fixture.session, childAdmission, directorySettling)
	select {
	case <-finalizeCalled:
		t.Fatal("directory executor ran before the active file reached a terminal cut")
	default:
	}
	if _, err := transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mustSignal(t, finalizeCalled)
	if err := mustResult(t, finalized); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryFinalizationSealRejectsNewFileBeforeFileIO(t *testing.T) {
	finalizeStarted := make(chan struct{})
	releaseFinalize := make(chan struct{})
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).finalize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryFinalization, error) {
			close(finalizeStarted)
			<-releaseFinalize
			return FinalizedDirectory(), nil
		}
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	finalized := make(chan error, 1)
	go func() {
		_, err := fixture.session.FinalizeDirectory(ctx, root)
		finalized <- err
	}()
	mustSignal(t, finalizeStarted)

	_, beginErr := fixture.session.BeginFile(ctx, fixture.outputFile(root, 73, "late.bin"))
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	close(releaseFinalize)
	finalizeErr := mustResult(t, finalized)

	if !errors.Is(beginErr, ErrDirectoryBinding) {
		t.Fatalf("begin after seal error=%v", beginErr)
	}
	if beginCalls != 0 {
		t.Fatalf("file executor calls after directory seal=%d", beginCalls)
	}
	if finalizeErr != nil {
		t.Fatalf("already-running finalization did not reach its stable cut: %v", finalizeErr)
	}
}

func TestParentFinalizationRequiresChildSettlementAndCachesIsolatedResult(t *testing.T) {
	metadataFault, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).finalize = func(
			_ context.Context,
			claim DirectoryClaim,
		) (DirectoryFinalization, error) {
			if claim.Directory().Path == "child" {
				return IsolatedDirectory(metadataFault)
			}
			return FinalizedDirectory(), nil
		}
	})
	ctx := context.Background()
	rootAdmission := fixture.admitRoot(ctx)
	child := fixture.childDirectory(rootAdmission, 51, "child")
	childAdmission, err := fixture.session.AdmitDirectory(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.FinalizeDirectory(ctx, rootAdmission); !errors.Is(err, ErrDirectoryChildrenUnsettled) {
		t.Fatalf("premature root finalization error=%v", err)
	}
	first, err := fixture.session.FinalizeDirectory(ctx, childAdmission)
	if err != nil || first.Kind() != transfer.DirectoryIsolatedFailure {
		t.Fatalf("child settlement kind=%v err=%v", first.Kind(), err)
	}
	second, err := fixture.session.FinalizeDirectory(ctx, childAdmission)
	if err != nil || second != first {
		t.Fatalf("cached settlement changed: first=%v second=%v err=%v", first.Kind(), second.Kind(), err)
	}
	rootSettlement, err := fixture.session.FinalizeDirectory(ctx, rootAdmission)
	if err != nil || rootSettlement.Kind() != transfer.DirectoryFinalized {
		t.Fatalf("root settlement kind=%v err=%v", rootSettlement.Kind(), err)
	}
	_, finalizeCalls := fixture.directories.counts()
	if finalizeCalls != 2 {
		t.Fatalf("finalize calls=%d want child once and root once", finalizeCalls)
	}
}

func TestDirectoryAdmissionAmbiguityRetainsClaimForAttention(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).materialize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryMaterialization, error) {
			return DirectoryMaterialization{Cut: MutationAmbiguous}, errors.New("create result unknown")
		}
	})
	if _, err := fixture.session.AdmitDirectory(context.Background(), fixture.rootDirectory); !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("admission ambiguity error=%v", err)
	}
	fixture.session.mu.Lock()
	entry := fixture.session.directoryClaims[fixture.session.rootClaim]
	retained := entry != nil && entry.uncertain && fixture.session.metadataBytes != 0 &&
		len(fixture.session.nodeClaims) == 1 && len(fixture.session.locatorClaims) == 1
	fixture.session.mu.Unlock()
	if !retained {
		t.Fatal("ambiguous admission did not retain its full reservation")
	}
	settlement, err := fixture.session.PauseJob(context.Background(), transfer.JobPauseOutputFailure)
	if err != nil || settlement.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("pause settlement=%v err=%v", settlement.Kind(), err)
	}
}

func TestDirectoryFinalizationCoalescesAndRetriesStableNoChange(t *testing.T) {
	finalizeStarted := make(chan struct{})
	releaseFinalize := make(chan struct{})
	coalesced := make(chan struct{}, 1)
	var calls int
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).finalize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryFinalization, error) {
			calls++
			if calls == 1 {
				close(finalizeStarted)
				<-releaseFinalize
				return DirectoryFinalization{Cut: MutationNoChange}, context.Canceled
			}
			return FinalizedDirectory(), nil
		}
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationFinalizeDirectory && event.Decision == TraceCoalesced {
				select {
				case coalesced <- struct{}{}:
				default:
				}
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	type result struct {
		settlement transfer.DirectorySettlement
		err        error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() {
		settlement, err := fixture.session.FinalizeDirectory(ctx, root)
		first <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, finalizeStarted)
	go func() {
		settlement, err := fixture.session.FinalizeDirectory(ctx, root)
		second <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, coalesced)
	close(releaseFinalize)
	if result := mustResult(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("owner error=%v", result.err)
	}
	if result := mustResult(t, second); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("coalesced error=%v", result.err)
	}
	settlement, err := fixture.session.FinalizeDirectory(ctx, root)
	if err != nil || settlement.Kind() != transfer.DirectoryFinalized || calls != 2 {
		t.Fatalf("retry settlement=%v calls=%d err=%v", settlement.Kind(), calls, err)
	}
}

func TestAmbiguousDirectoryFinalizationCannotBecomeCachedSettlement(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Directories.(*fakeDirectoryAuthority).finalize = func(
			context.Context,
			DirectoryClaim,
		) (DirectoryFinalization, error) {
			return DirectoryFinalization{Cut: MutationAmbiguous}, errors.New("metadata observation unavailable")
		}
	})
	root := fixture.admitRoot(context.Background())
	if _, err := fixture.session.FinalizeDirectory(context.Background(), root); !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("finalization ambiguity error=%v", err)
	}
	fixture.session.mu.Lock()
	entry := fixture.session.directoryClaims[fixture.session.rootClaim]
	uncertain := entry != nil && entry.state == directorySettling && entry.uncertain && entry.settlement.Kind() == 0
	fixture.session.mu.Unlock()
	if !uncertain {
		t.Fatal("ambiguous finalization was not retained as an unsettled sealed claim")
	}
}

func waitForDirectoryState(
	t *testing.T,
	session *Session,
	admission transfer.DirectoryAdmission,
	want directoryState,
) {
	t.Helper()
	deadline := time.After(concurrencyTestTimeout)
	for {
		session.mu.Lock()
		claimID := session.receiptClaims[receiptKey(admission)]
		entry := session.directoryClaims[claimID]
		matched := entry != nil && entry.state == want
		var changed <-chan struct{}
		if entry != nil {
			changed = entry.changed
		}
		session.mu.Unlock()
		if matched {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("directory did not reach state %d", want)
		}
	}
}

func mustSignal(t *testing.T, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(concurrencyTestTimeout):
		t.Fatal("timed out waiting for deterministic barrier")
	}
}

func mustResult[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case result := <-channel:
		return result
	case <-time.After(concurrencyTestTimeout):
		t.Fatal("timed out waiting for deterministic result")
		var zero T
		return zero
	}
}
