package outputsession

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestPauseClosesGateDrainsWriteAndSettlesActiveFileOnce(t *testing.T) {
	traceDraining := make(chan struct{}, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationPauseJob && event.Decision == TraceDraining {
				select {
				case traceDraining <- struct{}{}:
				default:
				}
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 121, "file.bin")
	start, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	executor.write = func(context.Context, uint64, []byte) (MutationCut, error) {
		close(writeStarted)
		<-releaseWrite
		return MutationStable, nil
	}
	writeResult := make(chan error, 1)
	go func() { writeResult <- transaction.WriteRange(ctx, 0, []byte{1}) }()
	mustSignal(t, writeStarted)
	type pauseResult struct {
		settlement transfer.JobSettlement
		err        error
	}
	paused := make(chan pauseResult, 1)
	go func() {
		settlement, err := fixture.session.PauseJob(ctx, transfer.JobPauseInterrupted)
		paused <- pauseResult{settlement: settlement, err: err}
	}()
	waitForGateCloseRequest(t, &fixture.session.gate)
	mustSignal(t, traceDraining)
	fixture.resources.mu.Lock()
	resourceCalls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	executor.mu.Lock()
	pauseCalls := executor.pauseCalls
	executor.mu.Unlock()
	if resourceCalls != 0 || pauseCalls != 0 {
		t.Fatalf("close crossed in-flight write: resources=%d pause=%d", resourceCalls, pauseCalls)
	}
	if _, err := fixture.session.AdmitDirectory(ctx, fixture.childDirectory(root, 40, "late")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("operation admitted after close request: %v", err)
	}
	close(releaseWrite)
	if err := mustResult(t, writeResult); err != nil {
		t.Fatal(err)
	}
	result := mustResult(t, paused)
	if result.err != nil || result.settlement.Kind() != transfer.JobPaused {
		t.Fatalf("pause settlement=%v err=%v", result.settlement.Kind(), result.err)
	}
	executor.mu.Lock()
	pauseCalls = executor.pauseCalls
	executor.mu.Unlock()
	fixture.resources.mu.Lock()
	resourceCalls = fixture.resources.calls
	fixture.resources.mu.Unlock()
	if pauseCalls != 1 || resourceCalls != 1 {
		t.Fatalf("pause calls=%d resources=%d", pauseCalls, resourceCalls)
	}
	cached, err := fixture.session.PauseJob(ctx, transfer.JobPauseInterrupted)
	if err != nil || cached.Kind() != transfer.JobPaused {
		t.Fatalf("cached pause=%v err=%v", cached.Kind(), err)
	}
	fixture.resources.mu.Lock()
	resourceCalls = fixture.resources.calls
	fixture.resources.mu.Unlock()
	if resourceCalls != 1 {
		t.Fatalf("cached pause released resources again: %d", resourceCalls)
	}
	if _, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded); !errors.Is(err, ErrConflictingSettlement) {
		t.Fatalf("conflicting complete error=%v", err)
	}
}

func TestPauseInterruptsWaitingFinalizationAtNoMutationCut(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	if _, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 123, "active.bin")); err != nil {
		t.Fatal(err)
	}

	finalized := make(chan error, 1)
	go func() {
		_, err := fixture.session.FinalizeDirectory(ctx, root)
		finalized <- err
	}()
	waitForDirectoryState(t, fixture.session, root, directorySettling)

	type pauseResult struct {
		settlement transfer.JobSettlement
		err        error
	}
	paused := make(chan pauseResult, 1)
	go func() {
		settlement, err := fixture.session.PauseJob(ctx, transfer.JobPauseInterrupted)
		paused <- pauseResult{settlement: settlement, err: err}
	}()
	if err := mustResult(t, finalized); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("waiting finalization close error=%v", err)
	}
	result := mustResult(t, paused)
	if result.err != nil || result.settlement.Kind() != transfer.JobPaused {
		t.Fatalf("pause settlement=%v err=%v", result.settlement.Kind(), result.err)
	}
	_, finalizeCalls := fixture.directories.counts()
	if finalizeCalls != 0 {
		t.Fatalf("waiting finalization crossed into executor: calls=%d", finalizeCalls)
	}
}

func TestCompleteDrainsInFlightWriteBeforeStableFallback(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 125, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	executor.write = func(context.Context, uint64, []byte) (MutationCut, error) {
		close(writeStarted)
		<-releaseWrite
		return MutationStable, nil
	}
	writeResult := make(chan error, 1)
	go func() { writeResult <- transaction.WriteRange(ctx, 0, []byte{1}) }()
	mustSignal(t, writeStarted)

	type closeResult struct {
		settlement transfer.JobSettlement
		err        error
	}
	closed := make(chan closeResult, 1)
	go func() {
		settlement, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
		closed <- closeResult{settlement: settlement, err: err}
	}()
	waitForGateCloseRequest(t, &fixture.session.gate)
	executor.mu.Lock()
	pauseCalls := executor.pauseCalls
	executor.mu.Unlock()
	fixture.resources.mu.Lock()
	releaseCalls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	if pauseCalls != 0 || releaseCalls != 0 {
		t.Fatalf("complete crossed in-flight write: pauses=%d releases=%d", pauseCalls, releaseCalls)
	}

	close(releaseWrite)
	if err := mustResult(t, writeResult); err != nil {
		t.Fatal(err)
	}
	result := mustResult(t, closed)
	if !errors.Is(result.err, ErrConflictingSettlement) ||
		result.settlement.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("complete fallback settlement=%v err=%v", result.settlement.Kind(), result.err)
	}
	executor.mu.Lock()
	pauseCalls = executor.pauseCalls
	executor.mu.Unlock()
	fixture.resources.mu.Lock()
	releaseCalls = fixture.resources.calls
	fixture.resources.mu.Unlock()
	if pauseCalls != 1 || releaseCalls != 1 {
		t.Fatalf("complete fallback pauses=%d releases=%d", pauseCalls, releaseCalls)
	}
}

func TestCompleteRequiresSettledRootAndCachesClosedResult(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
		t.Fatal(err)
	}
	settlement, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
	if err != nil || settlement.Kind() != transfer.JobClosed {
		t.Fatalf("complete settlement=%v err=%v", settlement.Kind(), err)
	}
	cached, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
	if err != nil || cached.Kind() != transfer.JobClosed {
		t.Fatalf("cached complete=%v err=%v", cached.Kind(), err)
	}
	fixture.resources.mu.Lock()
	calls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	if calls != 1 {
		t.Fatalf("resource releases=%d want=1", calls)
	}
	fixture.session.mu.Lock()
	ledgerReleased := allZero(fixture.session.secret[:]) &&
		fixture.session.directoryClaims == nil && fixture.session.fileClaims == nil &&
		fixture.session.nodeClaims == nil && fixture.session.locatorClaims == nil
	fixture.session.mu.Unlock()
	if !ledgerReleased {
		t.Fatal("closed session retained claim authority or receipt key material")
	}
	if _, err := fixture.session.FinalizeDirectory(ctx, root); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("operation after completion error=%v", err)
	}
}

func TestPublishBlockedSettlementClosesNeedsAttention(t *testing.T) {
	tests := []struct {
		name       string
		settleFile func(*testing.T, testFixture, transfer.OutputFile)
	}{
		{name: "immediate begin settlement", settleFile: func(t *testing.T, fixture testFixture, file transfer.OutputFile) {
			fixture.files.begin = func(_ context.Context, claim FileClaim) (FileBeginObservation, error) {
				transaction := newFakeTransaction(t, claim.File().Target)
				settlement, err := transfer.NewVerifiedFileSettlement(
					transfer.FilePublishBlocked,
					transaction.fullCheckpoint,
				)
				return FileBeginObservation{Cut: MutationStable, Settlement: settlement}, err
			}
			if _, err := fixture.session.BeginFile(context.Background(), file); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "transaction settlement", settleFile: func(t *testing.T, fixture testFixture, file transfer.OutputFile) {
			start, err := fixture.session.BeginFile(context.Background(), file)
			if err != nil {
				t.Fatal(err)
			}
			transaction, _, _ := start.Transaction()
			executor := fixture.files.transaction()
			executor.commit = func(context.Context) (transfer.FileSettlement, MutationCut, error) {
				settlement, err := transfer.NewVerifiedFileSettlement(
					transfer.FilePublishBlocked,
					executor.fullCheckpoint,
				)
				return settlement, MutationStable, err
			}
			if _, err := transaction.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTestFixture(t, nil)
			root := fixture.admitRoot(context.Background())
			test.settleFile(t, fixture, fixture.outputFile(root, 129, "file.bin"))
			if _, err := fixture.session.FinalizeDirectory(context.Background(), root); err != nil {
				t.Fatal(err)
			}
			settlement, err := fixture.session.CompleteJob(
				context.Background(),
				transfer.JobCompletedWithErrors,
			)
			if err != nil || settlement.Kind() != transfer.JobPausedNeedsAttention {
				t.Fatalf("complete settlement=%v err=%v", settlement.Kind(), err)
			}
		})
	}
}

func TestInvalidCompleteFallsBackToStablePausedNeedsAttention(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 131, "active.bin")
	if _, err := fixture.session.BeginFile(ctx, file); err != nil {
		t.Fatal(err)
	}
	settlement, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
	if !errors.Is(err, ErrConflictingSettlement) || settlement.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("invalid complete settlement=%v err=%v", settlement.Kind(), err)
	}
	executor := fixture.files.transaction()
	executor.mu.Lock()
	pauseCalls := executor.pauseCalls
	executor.mu.Unlock()
	if pauseCalls != 1 {
		t.Fatalf("active transaction pause calls=%d want=1", pauseCalls)
	}
	fixture.session.mu.Lock()
	state := fixture.session.state
	fixture.session.mu.Unlock()
	if state != sessionPaused {
		t.Fatalf("session state=%d want paused", state)
	}
}

func TestResourceReleaseFailureIsNormalizedAndCachedWithoutRawCause(t *testing.T) {
	raw := errors.New("native lease handle failed")
	fixture := newTestFixture(t, func(config *Config) {
		outputState, err := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
		if err != nil {
			t.Fatal(err)
		}
		config.Resources.(*fakeResources).release = func(context.Context) error {
			return fault.Wrap(outputState, raw)
		}
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
		t.Fatal(err)
	}
	settlement, err := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
	if err == nil || settlement.Kind() != transfer.JobPausedNeedsAttention || errors.Is(err, raw) {
		t.Fatalf("release settlement=%v err=%v raw-retained=%v", settlement.Kind(), err, errors.Is(err, raw))
	}
	cached, cachedErr := fixture.session.CompleteJob(ctx, transfer.JobSucceeded)
	if cached != settlement || cachedErr != err {
		t.Fatalf("cached close changed settlement/error: settlement=%v/%v err=%v/%v",
			settlement.Kind(), cached.Kind(), err, cachedErr)
	}
}

func waitForGateCloseRequest(t *testing.T, gate *operationGate) {
	t.Helper()
	gate.mu.Lock()
	requested := gate.closeRequested
	gate.mu.Unlock()
	mustSignal(t, requested)
}
