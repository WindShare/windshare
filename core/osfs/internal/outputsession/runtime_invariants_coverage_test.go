package outputsession

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func TestExecutorContractContradictionsRetainAmbiguousAuthority(t *testing.T) {
	t.Run("directory admission", func(t *testing.T) {
		ambiguous := make(chan TraceEvent, 1)
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).materialize = func(
				context.Context,
				DirectoryClaim,
			) (DirectoryMaterialization, error) {
				return DirectoryMaterialization{Cut: MutationStable}, errors.New("stable materialization lost its result")
			}
			config.Trace = TraceSinkFunc(func(event TraceEvent) {
				if event.Operation == OperationAdmitDirectory && event.Decision == TraceAmbiguous {
					ambiguous <- event
				}
			})
		})

		_, err := fixture.admitDirectory(context.Background(), fixture.rootDirectory)
		if !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("contradictory admission error=%v", err)
		}
		event := mustResult(t, ambiguous)
		fixture.session.mu.Lock()
		entry := fixture.session.directoryClaims[event.ClaimID]
		retained := entry != nil && entry.uncertain && len(fixture.session.nodeClaims) == 1 &&
			len(fixture.session.locatorClaims) == 1 && fixture.session.metadataBytes != 0
		fixture.session.mu.Unlock()
		if !retained {
			t.Fatal("ambiguous materialization discarded reserved namespace authority")
		}
	})

	t.Run("directory finalization", func(t *testing.T) {
		ambiguous := make(chan TraceEvent, 1)
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).finalize = func(
				context.Context,
				DirectoryClaim,
			) (DirectoryFinalization, error) {
				return DirectoryFinalization{Cut: MutationStable}, errors.New("stable finalization lost its settlement")
			}
			config.Trace = TraceSinkFunc(func(event TraceEvent) {
				if event.Operation == OperationFinalizeDirectory && event.Decision == TraceAmbiguous {
					ambiguous <- event
				}
			})
		})
		root := fixture.admitRoot(context.Background())

		_, err := fixture.session.FinalizeDirectory(context.Background(), root)
		if !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("contradictory finalization error=%v", err)
		}
		event := mustResult(t, ambiguous)
		fixture.session.mu.Lock()
		entry := fixture.session.directoryClaims[event.ClaimID]
		retained := entry != nil && entry.state == directorySettling && entry.uncertain && entry.settlement.Kind() == 0
		fixture.session.mu.Unlock()
		if !retained {
			t.Fatal("ambiguous finalization became replayable or cached")
		}
	})

	t.Run("file begin", func(t *testing.T) {
		ambiguous := make(chan TraceEvent, 1)
		fixture := newTestFixture(t, func(config *Config) {
			config.Files.(*fakeFileEngine).begin = func(
				context.Context,
				FileClaim,
			) (FileBeginObservation, error) {
				return FileBeginObservation{Cut: MutationStable}, errors.New("stable begin lost its transaction")
			}
			config.Trace = TraceSinkFunc(func(event TraceEvent) {
				if event.Operation == OperationBeginFile && event.Decision == TraceAmbiguous {
					ambiguous <- event
				}
			})
		})
		root := fixture.admitRoot(context.Background())

		_, err := fixture.session.BeginFile(
			context.Background(),
			fixture.outputFile(root, 141, "contradictory.bin"),
		)
		if !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("contradictory begin error=%v", err)
		}
		event := mustResult(t, ambiguous)
		fixture.session.mu.Lock()
		entry := fixture.session.fileClaims[event.ClaimID]
		retained := entry != nil && entry.uncertain && fixture.session.fileSlots == 1 &&
			len(fixture.session.nodeClaims) == 2 && len(fixture.session.locatorClaims) == 2
		fixture.session.mu.Unlock()
		if !retained {
			t.Fatal("ambiguous file begin released authority needed for reconciliation")
		}
	})
}

func TestSuccessfulExecutorResultsMustCarryReplayAuthority(t *testing.T) {
	t.Run("directory materialization cut", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).materialize = func(
				context.Context,
				DirectoryClaim,
			) (DirectoryMaterialization, error) {
				return DirectoryMaterialization{Cut: MutationNoChange}, nil
			}
		})
		if _, err := fixture.admitDirectory(
			context.Background(), fixture.rootDirectory,
		); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid success cut error=%v", err)
		}
	})

	t.Run("directory settlement payload", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).finalize = func(
				context.Context,
				DirectoryClaim,
			) (DirectoryFinalization, error) {
				return DirectoryFinalization{
					Cut: MutationStable, Kind: DirectoryFinalizationFinalized,
					Failure: fault.DependencyContractFault(),
				}, nil
			}
		})
		root := fixture.admitRoot(context.Background())
		if _, err := fixture.session.FinalizeDirectory(
			context.Background(), root,
		); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid finalized payload error=%v", err)
		}
	})

	t.Run("transaction durable binding", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			engine := config.Files.(*fakeFileEngine)
			engine.begin = func(_ context.Context, claim FileClaim) (FileBeginObservation, error) {
				transaction := newFakeTransaction(t, claim.File().Target())
				engine.mu.Lock()
				engine.last = transaction
				engine.mu.Unlock()
				return FileBeginObservation{Cut: MutationStable, Transaction: transaction}, nil
			}
		})
		root := fixture.admitRoot(context.Background())
		if _, err := fixture.session.BeginFile(
			context.Background(), fixture.outputFile(root, 148, "missing-durable.bin"),
		); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("missing durable binding error=%v", err)
		}
	})

	t.Run("immediate settlement cannot carry durable transaction state", func(t *testing.T) {
		fixture := newTestFixture(t, func(config *Config) {
			engine := config.Files.(*fakeFileEngine)
			engine.begin = func(_ context.Context, claim FileClaim) (FileBeginObservation, error) {
				transaction := newFakeTransaction(t, claim.File().Target())
				settlement, err := transfer.NewCollisionFileSettlement(claim.File().Target())
				return FileBeginObservation{
					Cut: MutationStable, Durable: transaction.emptyCheckpoint, Settlement: settlement,
				}, err
			}
		})
		root := fixture.admitRoot(context.Background())
		if _, err := fixture.session.BeginFile(
			context.Background(), fixture.outputFile(root, 149, "settled-with-durable.bin"),
		); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("settlement with durable state error=%v", err)
		}
	})
}

func TestTransactionResultsMustAuthorizeRetryOrSettlement(t *testing.T) {
	t.Run("nonterminal success cut", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		root := fixture.admitRoot(context.Background())
		start, err := fixture.session.BeginFile(
			context.Background(), fixture.outputFile(root, 150, "invalid-write-cut.bin"),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, _ := start.Transaction()
		fixture.files.transaction().write = func(context.Context, uint64, []byte) (MutationCut, error) {
			return MutationNoChange, nil
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte{1}); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid write success error=%v", err)
		}
	})

	t.Run("checkpoint binding", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		root := fixture.admitRoot(context.Background())
		start, err := fixture.session.BeginFile(
			context.Background(), fixture.outputFile(root, 151, "invalid-checkpoint.bin"),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, _ := start.Transaction()
		fixture.files.transaction().checkpoint = func(
			context.Context,
		) (transfer.VerifiedDurableRanges, MutationCut, error) {
			return transfer.VerifiedDurableRanges{}, MutationStable, nil
		}
		if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid checkpoint binding error=%v", err)
		}
	})

	t.Run("terminal settlement kind", func(t *testing.T) {
		fixture := newTestFixture(t, nil)
		root := fixture.admitRoot(context.Background())
		start, err := fixture.session.BeginFile(
			context.Background(), fixture.outputFile(root, 152, "invalid-commit.bin"),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, _ := start.Transaction()
		executor := fixture.files.transaction()
		executor.commit = func(context.Context) (transfer.FileSettlement, MutationCut, error) {
			settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, executor.emptyCheckpoint)
			return settlement, MutationStable, err
		}
		if _, err := transaction.Commit(context.Background()); !errors.Is(err, ErrMutationAmbiguous) || !errors.Is(err, ErrExecutorContract) {
			t.Fatalf("invalid commit settlement error=%v", err)
		}
	})
}

func TestStableAdmissionRetryUsesCachedReceiptWithoutExecutorReplay(t *testing.T) {
	fixture := newTestFixture(t, nil)
	first, err := fixture.admitDirectory(context.Background(), fixture.rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.admitDirectory(context.Background(), fixture.rootDirectory)
	if err != nil || !first.Equal(second) {
		t.Fatalf("cached admission equal=%v err=%v", first.Equal(second), err)
	}
	materializeCalls, _ := fixture.directories.counts()
	if materializeCalls != 1 {
		t.Fatalf("cached admission replayed executor: calls=%d", materializeCalls)
	}
}

func TestCoalescedBeginCancellationDoesNotCancelTransactionOwner(t *testing.T) {
	beginStarted := make(chan struct{})
	releaseBegin := make(chan struct{})
	coalesced := make(chan struct{}, 1)
	fixture := newTestFixture(t, func(config *Config) {
		engine := config.Files.(*fakeFileEngine)
		engine.begin = func(_ context.Context, claim FileClaim) (FileBeginObservation, error) {
			close(beginStarted)
			<-releaseBegin
			transaction := newFakeTransaction(t, claim.File().Target())
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
	root := fixture.admitRoot(context.Background())
	file := fixture.outputFile(root, 153, "cancel-waiter.bin")

	type result struct {
		start transfer.FileStart
		err   error
	}
	owner := make(chan result, 1)
	go func() {
		start, err := fixture.session.BeginFile(context.Background(), file)
		owner <- result{start: start, err: err}
	}()
	mustSignal(t, beginStarted)
	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiter := make(chan result, 1)
	go func() {
		start, err := fixture.session.BeginFile(waiterContext, file)
		waiter <- result{start: start, err: err}
	}()
	mustSignal(t, coalesced)
	cancelWaiter()
	if result := mustResult(t, waiter); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled begin waiter error=%v", result.err)
	}

	close(releaseBegin)
	ownerResult := mustResult(t, owner)
	if ownerResult.err != nil {
		t.Fatal(ownerResult.err)
	}
	if _, _, ok := ownerResult.start.Transaction(); !ok {
		t.Fatal("waiter cancellation prevented owner transaction")
	}
	fixture.files.mu.Lock()
	beginCalls := len(fixture.files.beginCalls)
	fixture.files.mu.Unlock()
	if beginCalls != 1 {
		t.Fatalf("canceled waiter replayed begin: calls=%d", beginCalls)
	}
}

func TestCoalescedTerminalCancellationDoesNotCancelSettlementOwner(t *testing.T) {
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
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
	root := fixture.admitRoot(context.Background())
	start, err := fixture.session.BeginFile(
		context.Background(),
		fixture.outputFile(root, 142, "coalesced.bin"),
	)
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active transaction")
	}
	executor := fixture.files.transaction()
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
	owner := make(chan result, 1)
	go func() {
		settlement, err := transaction.Commit(context.Background())
		owner <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, commitStarted)

	waiterContext, cancelWaiter := context.WithCancel(context.Background())
	waiter := make(chan result, 1)
	go func() {
		settlement, err := transaction.Commit(waiterContext)
		waiter <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, coalesced)
	cancelWaiter()
	if result := mustResult(t, waiter); !errors.Is(result.err, context.Canceled) || result.settlement.Kind() != 0 {
		t.Fatalf("canceled waiter settlement=%v err=%v", result.settlement.Kind(), result.err)
	}

	close(releaseCommit)
	ownerResult := mustResult(t, owner)
	if ownerResult.err != nil || ownerResult.settlement.Kind() != transfer.FilePublished {
		t.Fatalf("owner settlement=%v err=%v", ownerResult.settlement.Kind(), ownerResult.err)
	}
	retry, err := transaction.Commit(context.Background())
	if err != nil || !reflect.DeepEqual(retry, ownerResult.settlement) {
		t.Fatalf("terminal retry settlement=%v err=%v", retry.Kind(), err)
	}
	executor.mu.Lock()
	commitCalls := executor.commitCalls
	executor.mu.Unlock()
	if commitCalls != 1 {
		t.Fatalf("waiter cancellation replayed commit: calls=%d", commitCalls)
	}
}

func TestConcurrentCloseRequestsSettleAndReleaseExactlyOnce(t *testing.T) {
	releaseStarted := make(chan struct{})
	releaseResources := make(chan struct{})
	fixture := newTestFixture(t, func(config *Config) {
		config.Resources.(*fakeResources).release = func(context.Context) error {
			close(releaseStarted)
			<-releaseResources
			return nil
		}
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
		t.Fatal(err)
	}

	type result struct {
		settlement transfer.DirectTreeSettlement
		err        error
	}
	owner := make(chan result, 1)
	exactRetry := make(chan result, 1)
	conflictingRetry := make(chan result, 1)
	go func() {
		settlement, err := fixture.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
		owner <- result{settlement: settlement, err: err}
	}()
	mustSignal(t, releaseStarted)
	go func() {
		settlement, err := fixture.session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
		exactRetry <- result{settlement: settlement, err: err}
	}()
	go func() {
		settlement, err := fixture.session.PauseTree(ctx, transfer.JobPauseShutdown)
		conflictingRetry <- result{settlement: settlement, err: err}
	}()

	select {
	case result := <-exactRetry:
		t.Fatalf("exact retry crossed the pending resource release: %+v", result)
	default:
	}
	select {
	case result := <-conflictingRetry:
		t.Fatalf("conflicting retry observed a partial close record: %+v", result)
	default:
	}
	close(releaseResources)

	ownerResult := mustResult(t, owner)
	exactResult := mustResult(t, exactRetry)
	conflictResult := mustResult(t, conflictingRetry)
	if ownerResult.err != nil || ownerResult.settlement.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("owner settlement=%v err=%v", ownerResult.settlement.Kind(), ownerResult.err)
	}
	if exactResult.err != nil || exactResult.settlement != ownerResult.settlement {
		t.Fatalf("exact retry settlement=%v err=%v", exactResult.settlement.Kind(), exactResult.err)
	}
	if !errors.Is(conflictResult.err, ErrConflictingSettlement) || conflictResult.settlement.Kind() != 0 {
		t.Fatalf("conflicting retry settlement=%v err=%v", conflictResult.settlement.Kind(), conflictResult.err)
	}
	fixture.resources.mu.Lock()
	releaseCalls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	if releaseCalls != 1 {
		t.Fatalf("concurrent closes released resources %d times", releaseCalls)
	}
}

func TestAmbiguousActiveFileClosesWithoutReplayingItsExecutor(t *testing.T) {
	fixture := newTestFixture(t, nil)
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 143, "ambiguous.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	executor.write = func(context.Context, uint64, []byte) (MutationCut, error) {
		return MutationAmbiguous, errors.New("write completion is unknowable")
	}
	if err := transaction.WriteRange(ctx, 0, []byte{1}); !errors.Is(err, ErrMutationAmbiguous) {
		t.Fatalf("ambiguous write error=%v", err)
	}

	settlement, err := fixture.session.PauseTree(ctx, transfer.JobPauseOutputFailure)
	if err == nil || settlement.Kind() != transfer.DirectTreeSettlementFailed {
		t.Fatalf("close settlement=%v err=%v", settlement.Kind(), err)
	}
	executor.mu.Lock()
	pauseCalls := executor.pauseCalls
	executor.mu.Unlock()
	fixture.resources.mu.Lock()
	releaseCalls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	if pauseCalls != 0 || releaseCalls != 1 {
		t.Fatalf("ambiguous file was replayed during close: pauses=%d releases=%d", pauseCalls, releaseCalls)
	}
}

func TestConcurrentNonterminalOperationIsRejectedBeforeExecutorReplay(t *testing.T) {
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	rejected := make(chan TraceEvent, 1)
	fixture := newTestFixture(t, func(config *Config) {
		config.Trace = TraceSinkFunc(func(event TraceEvent) {
			if event.Operation == OperationCheckpointFile && event.Decision == TraceRejected {
				rejected <- event
			}
		})
	})
	ctx := context.Background()
	root := fixture.admitRoot(ctx)
	start, err := fixture.session.BeginFile(ctx, fixture.outputFile(root, 144, "serialized.bin"))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, _ := start.Transaction()
	executor := fixture.files.transaction()
	executor.write = func(context.Context, uint64, []byte) (MutationCut, error) {
		close(writeStarted)
		<-releaseWrite
		return MutationStable, nil
	}

	writeResult := make(chan error, 1)
	go func() { writeResult <- transaction.WriteRange(ctx, 0, []byte{1}) }()
	mustSignal(t, writeStarted)
	if _, err := transaction.Checkpoint(ctx); !errors.Is(err, ErrFileAlreadyActive) {
		t.Fatalf("concurrent checkpoint error=%v", err)
	}
	event := mustResult(t, rejected)
	if event.ClaimID == 0 || event.From != ClaimActive || event.To != ClaimActive {
		t.Fatalf("concurrent rejection trace=%+v", event)
	}
	executor.mu.Lock()
	checkpointCalls := executor.checkpointCalls
	executor.mu.Unlock()
	if checkpointCalls != 0 {
		t.Fatalf("rejected checkpoint reached executor %d times", checkpointCalls)
	}
	close(releaseWrite)
	if err := mustResult(t, writeResult); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackRestoresAncestorSettlementAccounting(t *testing.T) {
	t.Run("child directory admission", func(t *testing.T) {
		failed := false
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).materialize = func(
				_ context.Context,
				claim DirectoryClaim,
			) (DirectoryMaterialization, error) {
				if claim.Source().SourcePath.String() == "child" && !failed {
					failed = true
					return DirectoryMaterialization{Cut: MutationNoChange}, context.Canceled
				}
				disposition := DirectoryAuthorityCreatedDescendant
				if claim.IsSessionRoot() {
					disposition = DirectoryCallerProvidedRoot
				}
				return DirectoryMaterialization{Cut: MutationStable, Disposition: disposition}, nil
			}
		})
		ctx := context.Background()
		root := fixture.admitRoot(ctx)
		if _, err := fixture.admitDirectory(
			ctx,
			fixture.childDirectory(root, 145, "child"),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("child rollback error=%v", err)
		}
		fixture.session.mu.Lock()
		rootEntry := fixture.session.directoryClaims[fixture.session.rootClaim]
		balanced := rootEntry.activeDescendants == 0 && rootEntry.directUnsettledChildren == 0 &&
			len(fixture.session.directoryClaims) == 1
		fixture.session.mu.Unlock()
		if !balanced {
			t.Fatal("child admission rollback left the parent artificially active")
		}
		if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
			t.Fatalf("parent finalization after child rollback: %v", err)
		}
	})

	t.Run("file begin", func(t *testing.T) {
		failed := false
		fixture := newTestFixture(t, func(config *Config) {
			config.Files.(*fakeFileEngine).begin = func(
				context.Context,
				FileClaim,
			) (FileBeginObservation, error) {
				if !failed {
					failed = true
					return FileBeginObservation{Cut: MutationNoChange}, context.Canceled
				}
				panic("unexpected replay")
			}
		})
		ctx := context.Background()
		root := fixture.admitRoot(ctx)
		if _, err := fixture.session.BeginFile(
			ctx,
			fixture.outputFile(root, 146, "rolled-back.bin"),
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("file rollback error=%v", err)
		}
		fixture.session.mu.Lock()
		rootEntry := fixture.session.directoryClaims[fixture.session.rootClaim]
		balanced := rootEntry.activeDescendants == 0 && fixture.session.fileSlots == 0 &&
			len(fixture.session.fileClaims) == 0
		fixture.session.mu.Unlock()
		if !balanced {
			t.Fatal("file begin rollback retained an active descendant or file slot")
		}
		if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
			t.Fatalf("parent finalization after file rollback: %v", err)
		}
	})

	t.Run("child finalization", func(t *testing.T) {
		failed := false
		fixture := newTestFixture(t, func(config *Config) {
			config.Directories.(*fakeDirectoryAuthority).finalize = func(
				_ context.Context,
				claim DirectoryClaim,
			) (DirectoryFinalization, error) {
				if claim.Source().SourcePath.String() == "child" && !failed {
					failed = true
					return DirectoryFinalization{Cut: MutationNoChange}, context.Canceled
				}
				return FinalizedDirectory(), nil
			}
		})
		ctx := context.Background()
		root := fixture.admitRoot(ctx)
		child, err := fixture.admitDirectory(ctx, fixture.childDirectory(root, 147, "child"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.session.FinalizeDirectory(ctx, child); !errors.Is(err, context.Canceled) {
			t.Fatalf("child finalization rollback error=%v", err)
		}
		fixture.session.mu.Lock()
		rootEntry := fixture.session.directoryClaims[fixture.session.rootClaim]
		activeDescendants := rootEntry.activeDescendants
		fixture.session.mu.Unlock()
		if activeDescendants != 0 {
			t.Fatalf("stable no-change finalization retained %d active descendants", activeDescendants)
		}
		if _, err := fixture.session.FinalizeDirectory(ctx, child); err != nil {
			t.Fatalf("child finalization retry: %v", err)
		}
		if _, err := fixture.session.FinalizeDirectory(ctx, root); err != nil {
			t.Fatalf("root finalization after child retry: %v", err)
		}
	})
}

func TestCanceledResourceReleaseSettlesAsNeedsAttention(t *testing.T) {
	fixture := newTestFixture(t, func(config *Config) {
		config.Resources.(*fakeResources).release = func(ctx context.Context) error {
			return ctx.Err()
		}
	})
	root := fixture.admitRoot(context.Background())
	if _, err := fixture.session.FinalizeDirectory(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	settlement, err := fixture.session.FinalizeTree(canceled, transfer.DirectTreeOutcomeSuccess)
	if !errors.Is(err, context.Canceled) || settlement.Kind() != transfer.DirectTreeSettlementFailed {
		t.Fatalf("canceled release settlement=%v err=%v", settlement.Kind(), err)
	}
	fixture.resources.mu.Lock()
	calls := fixture.resources.calls
	fixture.resources.mu.Unlock()
	if calls != 1 {
		t.Fatalf("canceled release calls=%d want=1", calls)
	}
}
