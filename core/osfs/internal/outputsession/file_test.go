package outputsession

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestFileBeginCoalescesExecutorButNeverSharesActiveTransaction(t *testing.T) {
	beginStarted := make(chan struct{})
	releaseBegin := make(chan struct{})
	coalesced := make(chan struct{}, 1)
	var beginCalls atomic.Int32
	fixture := newTestFixture(t, func(config *Config) {
		engine := config.Files.(*fakeFileEngine)
		engine.begin = func(_ context.Context, claim FileClaim) (FileBeginObservation, error) {
			beginCalls.Add(1)
			close(beginStarted)
			<-releaseBegin
			transaction := newFakeTransaction(t, claim.File().Target)
			engine.mu.Lock()
			engine.last = transaction
			engine.mu.Unlock()
			return FileBeginObservation{
				Cut: MutationStable, Transaction: transaction, Durable: transaction.emptyCheckpoint,
			}, nil
		}
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationBeginFile && event.Decision == TraceCoalesced {
				select {
				case coalesced <- struct{}{}:
				default:
				}
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 80, "file.bin")
	type result struct {
		start transfer.FileStart
		err   error
	}
	owner := make(chan result, 1)
	retry := make(chan result, 1)
	go func() {
		start, err := fixture.session.BeginFile(ctx, file)
		owner <- result{start: start, err: err}
	}()
	mustSignal(t, beginStarted)
	go func() {
		start, err := fixture.session.BeginFile(ctx, file)
		retry <- result{start: start, err: err}
	}()
	mustSignal(t, coalesced)
	close(releaseBegin)
	ownerResult := mustResult(t, owner)
	if ownerResult.err != nil {
		t.Fatal(ownerResult.err)
	}
	if _, _, ok := ownerResult.start.Transaction(); !ok {
		t.Fatal("begin owner did not receive the transaction")
	}
	retryResult := mustResult(t, retry)
	if !errors.Is(retryResult.err, ErrFileAlreadyActive) {
		t.Fatalf("concurrent retry error=%v", retryResult.err)
	}
	if beginCalls.Load() != 1 {
		t.Fatalf("file executor calls=%d want=1", beginCalls.Load())
	}
	if _, err := fixture.session.BeginFile(ctx, file); !errors.Is(err, ErrFileAlreadyActive) {
		t.Fatalf("active retry error=%v", err)
	}
}

func TestTerminalFileOperationCoalescesAndBeginReturnsCachedSettlement(t *testing.T) {
	coalesced := make(chan struct{}, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationCommitFile && event.Decision == TraceCoalesced {
				select {
				case coalesced <- struct{}{}:
				default:
				}
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 81, "file.bin")
	start, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected transaction")
	}
	executor := fixture.files.transaction()
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
	executor.commit = func(context.Context) (transfer.FileSettlement, MutationCut, error) {
		close(commitStarted)
		<-releaseCommit
		settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, executor.fullCheckpoint)
		return settlement, MutationStable, err
	}
	type result struct {
		settlement transfer.FileSettlement
		err        error
	}
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() {
		settlement, err := transaction.Commit(ctx)
		first <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, commitStarted)
	go func() {
		settlement, err := transaction.Commit(ctx)
		second <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, coalesced)
	close(releaseCommit)
	firstResult := mustResult(t, first)
	secondResult := mustResult(t, second)
	if firstResult.err != nil || secondResult.err != nil ||
		firstResult.settlement.Kind() != transfer.FilePublished || secondResult.settlement.Kind() != transfer.FilePublished {
		t.Fatalf("first=(%v,%v) second=(%v,%v)", firstResult.settlement.Kind(), firstResult.err,
			secondResult.settlement.Kind(), secondResult.err)
	}
	executor.mu.Lock()
	commitCalls := executor.commitCalls
	executor.mu.Unlock()
	if commitCalls != 1 {
		t.Fatalf("commit calls=%d want=1", commitCalls)
	}
	cached, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	settlement, ok := cached.ImmediateSettlement()
	if !ok || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("cached begin settlement kind=%v ok=%v", settlement.Kind(), ok)
	}
	if replay, err := transaction.Commit(ctx); err != nil || replay.Kind() != transfer.FilePublished {
		t.Fatalf("transaction terminal retry kind=%v err=%v", replay.Kind(), err)
	}
	if _, err := transaction.Pause(ctx, transfer.FilePauseInterrupted); !errors.Is(err, ErrTransactionOperationConflict) {
		t.Fatalf("conflicting terminal retry error=%v", err)
	}
}

func TestLocatorAndNodeIndexesRejectFileDirectoryAliasesBeforeFileIO(t *testing.T) {
	locatorRejection := make(chan TraceEvent, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Locator.(*fakeDirectoryAuthority).canonicalLocatorKey = func(path string) (string, error) {
			if path == "" {
				return "root", nil
			}
			return "same-platform-locator", nil
		}
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationBeginFile && event.Decision == TraceRejected {
				locatorRejection <- event
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	child := fixture.childDirectory(root, 55, "directory")
	if _, err := fixture.session.AdmitDirectory(ctx, child); err != nil {
		t.Fatal(err)
	}
	alias := fixture.outputFile(root, 90, "file.bin")
	if _, err := fixture.session.BeginFile(ctx, alias); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("locator alias error=%v", err)
	}
	rejection := mustResult(t, locatorRejection)
	code, output := rejection.Fault.OutputCode()
	if !output || code != fault.OutputDirectoryBinding || rejection.ClaimID == 0 ||
		rejection.NodeClaims != 2 || rejection.DirectoryClaims != 2 || rejection.ActiveFileClaims != 0 ||
		rejection.ReservedFileSlots != 0 {
		t.Fatalf("locator rejection trace=%+v", rejection)
	}
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	if beginCalls != 0 {
		t.Fatalf("file executor ran %d times after locator conflict", beginCalls)
	}

	nodeFixture := newTestFixture(t, nil)
	nodeRoot := nodeFixture.admitRoot(ctx)
	nodeChild := nodeFixture.childDirectory(nodeRoot, 66, "directory")
	if _, err := nodeFixture.session.AdmitDirectory(ctx, nodeChild); err != nil {
		t.Fatal(err)
	}
	conflictingNode := nodeFixture.outputFile(nodeRoot, 66, "other.bin")
	if _, err := nodeFixture.session.BeginFile(ctx, conflictingNode); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("cross-kind node conflict error=%v", err)
	}
}

func TestFileClaimRejectsDescriptorFromForeignIntentBeforeFileIO(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 97, "foreign.bin")
	foreignDescriptor, err := content.NewFileRevisionDescriptor(
		identity[catalog.ShareInstance](210),
		file.Descriptor.FileID(),
		file.Descriptor.FileRevision(),
		file.Descriptor.Geometry(),
		file.Descriptor.ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	foreignTarget, err := transfer.NewFileMaterializationTarget(
		fixture.sessionID,
		foreignDescriptor,
		file.Target.Locator(),
	)
	if err != nil {
		t.Fatal(err)
	}
	file.Descriptor = foreignDescriptor
	file.Target = foreignTarget

	if _, err := fixture.session.BeginFile(ctx, file); !errors.Is(err, ErrDirectoryBinding) {
		t.Fatalf("foreign-intent file error=%v", err)
	}
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	if beginCalls != 0 {
		t.Fatalf("foreign-intent file reached executor %d times", beginCalls)
	}
}

func TestNodeClaimBudgetRejectsFileBeforeExecutorIO(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.DirectoryClaims = 1
		config.Limits.NodeClaims = 1
		config.Limits.ActiveFileClaims = 1
	})
	root := fixture.admitRoot(context.Background())
	if _, err := fixture.session.BeginFile(
		context.Background(),
		fixture.outputFile(root, 100, "file.bin"),
	); !errors.Is(err, ErrResourceBudget) {
		t.Fatalf("node-count budget error=%v", err)
	}
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	fixture.session.mu.Lock()
	fileClaims := len(fixture.session.fileClaims)
	fixture.session.mu.Unlock()
	if beginCalls != 0 || fileClaims != 0 {
		t.Fatalf("over-budget file mutated: begins=%d claims=%d", beginCalls, fileClaims)
	}
}

func TestActiveFileBudgetIsReservedBeforeExecutorIO(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Limits = DefaultLimits()
		config.Limits.ActiveFileClaims = 1
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	first := fixture.outputFile(root, 101, "first.bin")
	if _, err := fixture.session.BeginFile(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := fixture.outputFile(root, 105, "second.bin")
	if _, err := fixture.session.BeginFile(ctx, second); !errors.Is(err, ErrResourceBudget) {
		t.Fatalf("second begin error=%v", err)
	}
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	if beginCalls != 1 {
		t.Fatalf("file executor calls=%d want=1", beginCalls)
	}
	fixture.session.mu.Lock()
	defer fixture.session.mu.Unlock()
	if len(fixture.session.fileClaims) != 1 || fixture.session.fileSlots != 1 || fixture.session.activeFiles != 1 {
		t.Fatalf("claims=%d slots=%d active=%d", len(fixture.session.fileClaims),
			fixture.session.fileSlots, fixture.session.activeFiles)
	}
}

func TestAmbiguousTransactionOperationForcesPauseWithoutTerminalCache(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 111, "file.bin")
	start, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	executor.write = func(context.Context, uint64, []byte) (MutationCut, error) {
		return MutationAmbiguous, errors.New("short native write")
	}
	if err := transaction.WriteRange(ctx, 0, []byte{1}); !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("write error=%v", err)
	}
	if _, err := transaction.Checkpoint(ctx); !errors.Is(err, ErrSessionRequiresPause) {
		t.Fatalf("operation after ambiguity error=%v", err)
	}
	fixture.session.mu.Lock()
	if !fixture.session.requiredFault.Valid() || !fixture.session.attention {
		fixture.session.mu.Unlock()
		t.Fatal("ambiguous transaction did not retain pause/attention state")
	}
	fixture.session.mu.Unlock()
}

func TestAmbiguousMutationPreservesSessionTerminalFault(t *testing.T) {
	terminal, err := fault.NewOutput(fault.ScopeSessionTerminal, fault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan TraceEvent, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationWriteRange && event.Decision == TraceAmbiguous {
				observed <- event
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 113, "terminal.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	fixture.files.transaction().write = func(context.Context, uint64, []byte) (MutationCut, error) {
		return MutationAmbiguous, fault.Wrap(terminal, errors.New("terminal native failure"))
	}

	writeErr := transaction.WriteRange(ctx, 0, []byte{1})
	if !errors.Is(writeErr, ErrMutationAmbiguous) {
		t.Fatalf("ambiguous terminal write error=%v", writeErr)
	}
	normalized, ok := fault.NormalizeBoundary(ctx, writeErr).Fault()
	if !ok || normalized != terminal {
		t.Fatalf("returned fault=%v ok=%v want=%v", normalized, ok, terminal)
	}
	event := mustResult(t, observed)
	if event.Fault != terminal {
		t.Fatalf("trace fault=%v want=%v", event.Fault, terminal)
	}
	fixture.session.mu.Lock()
	required := fixture.session.requiredFault
	fixture.session.mu.Unlock()
	if required != terminal {
		t.Fatalf("required fault=%v want=%v", required, terminal)
	}
}

func TestFileBeginRollbackThenCachesImmediateSettlement(t *testing.T) {
	calls := 0
	fixture := newTestFixture(t, func(config *Config) {
		config.Files.(*fakeFileEngine).begin = func(
			_ context.Context,
			claim FileClaim,
		) (FileBeginObservation, error) {
			calls++
			if calls == 1 {
				return FileBeginObservation{Cut: MutationNoChange}, context.Canceled
			}
			settlement, err := transfer.NewCollisionFileSettlement(claim.File().Target)
			return FileBeginObservation{Cut: MutationStable, Settlement: settlement}, err
		}
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 115, "file.bin")
	if _, err := fixture.session.BeginFile(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("first begin error=%v", err)
	}
	fixture.session.mu.Lock()
	if len(fixture.session.fileClaims) != 0 || fixture.session.fileSlots != 0 {
		fixture.session.mu.Unlock()
		t.Fatal("stable no-change file begin retained its reservation")
	}
	fixture.session.mu.Unlock()
	start, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	settlement, ok := start.ImmediateSettlement()
	if !ok || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("immediate settlement kind=%v ok=%v", settlement.Kind(), ok)
	}
	cached, err := fixture.session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	if settlement, ok := cached.ImmediateSettlement(); !ok || settlement.Kind() != transfer.FileCollision || calls != 2 {
		t.Fatalf("cached kind=%v ok=%v executor calls=%d", settlement.Kind(), ok, calls)
	}
}

func TestFileBeginAmbiguityRetainsAllIndexes(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Files.(*fakeFileEngine).begin = func(
			context.Context,
			FileClaim,
		) (FileBeginObservation, error) {
			return FileBeginObservation{Cut: MutationAmbiguous}, errors.New("checkpoint candidate unknown")
		}
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	file := fixture.outputFile(root, 116, "file.bin")
	if _, err := fixture.session.BeginFile(ctx, file); !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("begin ambiguity error=%v", err)
	}
	fixture.session.mu.Lock()
	defer fixture.session.mu.Unlock()
	if len(fixture.session.fileClaims) != 1 || len(fixture.session.locatorClaims) != 2 ||
		fixture.session.fileSlots != 1 || !fixture.session.attention {
		t.Fatalf("files=%d locators=%d slots=%d attention=%v", len(fixture.session.fileClaims),
			len(fixture.session.locatorClaims), fixture.session.fileSlots, fixture.session.attention)
	}
}

func TestFileTransactionCheckpointPauseAndRetire(t *testing.T) {
	t.Run("checkpoint and pause", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		ctx := context.Background()
		root := fixture.admitRoot(ctx)
		start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 117, "file.bin"))
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, _ := start.Transaction()
		checkpoint, err := transaction.Checkpoint(ctx)
		if err != nil || checkpoint.Binding() != transaction.Binding() {
			t.Fatalf("checkpoint binding mismatch err=%v", err)
		}
		settlement, err := transaction.Pause(ctx, transfer.FilePauseInterrupted)
		if err != nil || settlement.Kind() != transfer.FilePaused {
			t.Fatalf("pause settlement=%v err=%v", settlement.Kind(), err)
		}
	})

	t.Run("retire", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		ctx := context.Background()
		root := fixture.admitRoot(ctx)
		start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 118, "file.bin"))
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, _ := start.Transaction()
		settlement, err := transaction.Retire(ctx, transfer.FileRetireInvalidatedRevision)
		if err != nil || settlement.Kind() != transfer.FileRetired {
			t.Fatalf("retire settlement=%v err=%v", settlement.Kind(), err)
		}
	})
}

func TestStableFileOperationFailureCanRetryWithoutSharingAuthority(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 119, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	fileFault, err := fault.NewOutput(fault.ScopeFileLocal, fault.OutputStateIO)
	if err != nil {
		t.Fatal(err)
	}
	executor.commit = func(context.Context) (transfer.FileSettlement, MutationCut, error) {
		executor.mu.Lock()
		calls := executor.commitCalls
		executor.mu.Unlock()
		if calls == 1 {
			return transfer.FileSettlement{}, MutationNoChange, fault.Wrap(fileFault, errors.New("retryable publish"))
		}
		settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePublished, executor.fullCheckpoint)
		return settlement, MutationStable, err
	}
	if _, err := transaction.Commit(ctx); err == nil || errors.Is(err, ErrSessionRequiresPause) {
		t.Fatalf("first commit error=%v", err)
	}
	settlement, err := transaction.Commit(ctx)
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("retry commit settlement=%v err=%v", settlement.Kind(), err)
	}
}

func TestFileOperationStableErrorFailsAmbiguousWithoutReplay(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 120, "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	executor.commit = func(context.Context) (transfer.FileSettlement, MutationCut, error) {
		return transfer.FileSettlement{}, MutationStable, errors.New("stable cut lost its settlement")
	}
	if _, err := transaction.Commit(ctx); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
		t.Fatalf("contradictory stable failure error=%v", err)
	}
	if _, err := transaction.Commit(ctx); !errors.Is(err, ErrSessionRequiresPause) {
		t.Fatalf("contradictory failure replay error=%v", err)
	}
	executor.mu.Lock()
	calls := executor.commitCalls
	executor.mu.Unlock()
	if calls != 1 {
		t.Fatalf("contradictory stable failure replayed executor: calls=%d", calls)
	}
}
