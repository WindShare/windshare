package resumeauthority

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type coverageAuthorityStore struct {
	list       []Snapshot
	listErr    error
	lease      OperationLease
	acquireErr error
}

func (store *coverageAuthorityStore) List(context.Context) ([]Snapshot, error) {
	return append([]Snapshot(nil), store.list...), store.listErr
}

func (store *coverageAuthorityStore) Acquire(
	context.Context,
	receivecontract.OperationID,
) (OperationLease, error) {
	return store.lease, store.acquireErr
}

type coverageOperationLease struct {
	snapshot Snapshot
	recovery RecoveryEvidence
	cleanup  DiscardEvidence

	snapshotErr error
	recoveryErr error
	cleanupErr  error
	installErr  error
	replaceErr  error
	closeErr    error

	cleanupCalls int
	installCalls int
	replaceCalls int
}

func (lease *coverageOperationLease) Snapshot(context.Context) (Snapshot, error) {
	return lease.snapshot, lease.snapshotErr
}

func (lease *coverageOperationLease) ObserveRecovery(context.Context) (RecoveryEvidence, error) {
	return lease.recovery, lease.recoveryErr
}

func (lease *coverageOperationLease) CleanupOwned(context.Context) (DiscardEvidence, error) {
	lease.cleanupCalls++
	return lease.cleanup, lease.cleanupErr
}

func (lease *coverageOperationLease) InstallReceipt(
	context.Context,
	checkpointmodel.DirectTreeReceipt,
) error {
	lease.installCalls++
	return lease.installErr
}

func (lease *coverageOperationLease) ReplaceLifecycle(
	_ context.Context,
	_ checkpointmodel.ReceiveLifecycleState,
	next checkpointmodel.ReceiveLifecycleState,
) error {
	lease.replaceCalls++
	if lease.replaceErr == nil {
		lease.snapshot.lifecycle = next
	}
	return lease.replaceErr
}

func (lease *coverageOperationLease) Close() error { return lease.closeErr }

func newCoverageAuthority(
	t *testing.T,
	phase checkpointmodel.LifecyclePhase,
) (authorityFixture, Snapshot, *coverageOperationLease, *Authority) {
	t.Helper()
	fixture := newAuthorityFixture(t, byte(0x90+phase))
	state := lifecycleState(t, fixture, 2, phase)
	snapshot, err := NewSnapshot(fixture.operation, state)
	if err != nil {
		t.Fatal(err)
	}
	lease := &coverageOperationLease{
		snapshot: snapshot,
		recovery: RecoveryEvidence{
			TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
		},
		cleanup: DiscardEvidence{
			State: CleanupComplete, Receipt: receiptFixture(t, fixture, checkpointmodel.ReceiptCleanup),
		},
	}
	authority, err := New(&coverageAuthorityStore{list: []Snapshot{snapshot}, lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	return fixture, snapshot, lease, authority
}

func TestAuthorityInventoryProjectsCorruptionAndAmbiguityAsAttention(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x31)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	snapshot, err := NewSnapshot(fixture.operation, receiving)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := CorruptSnapshot(fixture.operation)
	if corrupt.Valid() || CorruptSnapshot(checkpointmodel.ReceiveOperation{}).operation.Valid() {
		t.Fatal("corrupt snapshot acquired mutation authority")
	}
	authority, err := New(&coverageAuthorityStore{list: []Snapshot{snapshot, corrupt}})
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.List(context.Background())
	if err != nil || inventory.Status() != ListNeedsAttention || len(inventory.Summaries()) != 1 ||
		len(inventory.Attention()) != 1 ||
		inventory.Attention()[0].Reason() != checkpointmodel.AttentionTargetOwnershipUnknown {
		t.Fatalf("corrupt inventory = (%+v, %v)", inventory, err)
	}

	operationErr := errors.New("list failed")
	authority, _ = New(&coverageAuthorityStore{listErr: operationErr})
	if _, err := authority.List(context.Background()); !errors.Is(err, operationErr) {
		t.Fatalf("list failure = %v", err)
	}
	if _, err := (&Authority{}).List(context.TODO()); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("missing authority store error = %v", err)
	}
	if _, err := NewAttention(receivecontract.OperationID{}, checkpointmodel.AttentionCleanupUnknown); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("zero attention operation error = %v", err)
	}
	if _, err := NewAttention(fixture.intent.OperationID(), 0); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("open attention reason error = %v", err)
	}
}

func TestAuthorityRecoveryRejectsLeaseAndReceiptUncertainty(t *testing.T) {
	operationErr := errors.New("operation failed")

	t.Run("acquire failure", func(t *testing.T) {
		fixture := newAuthorityFixture(t, 0x41)
		authority, _ := New(&coverageAuthorityStore{acquireErr: operationErr})
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) {
			t.Fatalf("acquire error = %v", err)
		}
	})

	t.Run("nil lease", func(t *testing.T) {
		fixture := newAuthorityFixture(t, 0x42)
		authority, _ := New(&coverageAuthorityStore{})
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("nil lease error = %v", err)
		}
	})

	t.Run("snapshot failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.snapshotErr = operationErr
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) || !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("snapshot error = %v", err)
		}
	})

	t.Run("foreign snapshot", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		foreign := newAuthorityFixture(t, 0x43)
		foreignState := lifecycleState(t, foreign, 2, checkpointmodel.LifecycleReceiving)
		lease.snapshot, _ = NewSnapshot(foreign.operation, foreignState)
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("foreign snapshot error = %v", err)
		}
	})

	t.Run("observation failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.recoveryErr = operationErr
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) {
			t.Fatalf("observation error = %v", err)
		}
	})

	t.Run("invalid evidence", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.recovery = RecoveryEvidence{}
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("invalid evidence error = %v", err)
		}
	})

	t.Run("foreign aggregate receipt", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleFinalizingTree)
		foreign := newAuthorityFixture(t, 0x44)
		lease.recovery.TerminalReceipt = receiptFixture(t, foreign, checkpointmodel.ReceiptTreeCompletion)
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("foreign receipt error = %v", err)
		}
	})

	t.Run("selected receipt install failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleFinalizingTree)
		lease.recovery.TerminalReceipt = receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion)
		lease.installErr = operationErr
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) {
			t.Fatalf("receipt install error = %v", err)
		}
		if lease.installCalls != 1 || lease.replaceCalls != 0 {
			t.Fatalf("install=%d replace=%d", lease.installCalls, lease.replaceCalls)
		}
	})

	t.Run("expiry overflow", func(t *testing.T) {
		fixture, _, _, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), math.MaxUint64); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("expiry overflow error = %v", err)
		}
	})

	t.Run("replace failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.replaceErr = operationErr
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) {
			t.Fatalf("replace error = %v", err)
		}
	})

	t.Run("close failure joins success", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.closeErr = operationErr
		if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 1); !errors.Is(err, operationErr) {
			t.Fatalf("close error = %v", err)
		}
	})
}

func TestAuthorityDiscardOrdersCleanupAndStopsOnUnknownEvidence(t *testing.T) {
	operationErr := errors.New("operation failed")

	t.Run("unknown ownership skips cleanup", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.recovery.TargetOwnership = EvidenceUnknown
		summary, err := authority.Discard(context.Background(), fixture.intent.OperationID())
		if err != nil || summary.NeedsAttentionReason() != checkpointmodel.AttentionTargetOwnershipUnknown ||
			lease.cleanupCalls != 0 || lease.replaceCalls != 1 {
			t.Fatalf("unknown discard = (%+v, %v), cleanup=%d replace=%d", summary, err, lease.cleanupCalls, lease.replaceCalls)
		}
	})

	t.Run("cleanup failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.cleanupErr = operationErr
		if _, err := authority.Discard(context.Background(), fixture.intent.OperationID()); !errors.Is(err, operationErr) {
			t.Fatalf("cleanup error = %v", err)
		}
	})

	t.Run("invalid cleanup evidence", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.cleanup = DiscardEvidence{}
		if _, err := authority.Discard(context.Background(), fixture.intent.OperationID()); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("invalid cleanup evidence error = %v", err)
		}
	})

	t.Run("unknown cleanup becomes attention", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.cleanup = DiscardEvidence{State: CleanupUnknown}
		summary, err := authority.Discard(context.Background(), fixture.intent.OperationID())
		if err != nil || summary.NeedsAttentionReason() != checkpointmodel.AttentionCleanupUnknown ||
			lease.installCalls != 0 || lease.replaceCalls != 1 {
			t.Fatalf("unknown cleanup = (%+v, %v), install=%d replace=%d", summary, err, lease.installCalls, lease.replaceCalls)
		}
	})

	t.Run("foreign cleanup receipt", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		foreign := newAuthorityFixture(t, 0x72)
		lease.cleanup = DiscardEvidence{
			State: CleanupComplete, Receipt: receiptFixture(t, foreign, checkpointmodel.ReceiptCleanup),
		}
		if _, err := authority.Discard(context.Background(), fixture.intent.OperationID()); !errors.Is(err, ErrInvalidContract) {
			t.Fatalf("foreign cleanup receipt error = %v", err)
		}
	})

	t.Run("cleanup receipt install failure", func(t *testing.T) {
		fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
		lease.installErr = operationErr
		if _, err := authority.Discard(context.Background(), fixture.intent.OperationID()); !errors.Is(err, operationErr) {
			t.Fatalf("cleanup receipt install error = %v", err)
		}
	})

	t.Run("published output is not a cleanup target", func(t *testing.T) {
		fixture := newAuthorityFixture(t, 0x73)
		receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion)
		published, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
			OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
			StateGeneration: 4, Phase: checkpointmodel.LifecyclePublished,
			CheckpointRefs: receipt.CheckpointReferences(), ReceiptDigest: receipt.Digest(),
			SuccessCount: 1, CleanupState: checkpointmodel.OwnedCleanupPending,
		})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, _ := NewSnapshot(fixture.operation, published)
		lease := &coverageOperationLease{
			snapshot: snapshot,
			recovery: RecoveryEvidence{
				TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
			},
		}
		authority, _ := New(&coverageAuthorityStore{lease: lease})
		summary, err := authority.Discard(context.Background(), fixture.intent.OperationID())
		if err != nil || summary.Phase() != checkpointmodel.LifecyclePublished || lease.cleanupCalls != 0 {
			t.Fatalf("published discard = (%+v, %v), cleanup=%d", summary, err, lease.cleanupCalls)
		}
	})
}

func coverageTerminalLifecycle(
	t *testing.T,
	fixture authorityFixture,
	phase checkpointmodel.LifecyclePhase,
	cleanup checkpointmodel.OwnedCleanupState,
) checkpointmodel.ReceiveLifecycleState {
	t.Helper()
	spec := checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 6, Phase: phase,
	}
	switch phase {
	case checkpointmodel.LifecyclePublished:
		receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion)
		spec.CheckpointRefs, spec.ReceiptDigest = receipt.CheckpointReferences(), receipt.Digest()
		spec.SuccessCount, spec.CleanupState = receipt.SuccessCount(), cleanup
	case checkpointmodel.LifecyclePartialDirectory:
		receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptPartialDirectory)
		spec.CheckpointRefs, spec.ReceiptDigest = receipt.CheckpointReferences(), receipt.Digest()
		spec.SuccessCount, spec.FailureCount = receipt.SuccessCount(), receipt.FailureCount()
		spec.PartialReason = receipt.PartialReason()
	case checkpointmodel.LifecycleDiscarded:
		receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptCleanup)
		spec.ReceiptDigest, spec.CleanupState = receipt.Digest(), checkpointmodel.OwnedCleanupClean
	case checkpointmodel.LifecycleExpired:
		receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptExpiry)
		spec.CheckpointRefs, spec.ReceiptDigest = receipt.CheckpointReferences(), receipt.Digest()
		spec.ExpiresAtMillis, spec.SuccessCount = 1_000, receipt.SuccessCount()
		spec.FailureCount, spec.CleanupState = receipt.FailureCount(), cleanup
		spec.PriorStableState = checkpointmodel.LifecycleResumableReceive
	case checkpointmodel.LifecycleNeedsAttention:
		spec.AttentionReason = checkpointmodel.AttentionTargetOwnershipUnknown
	}
	state, err := checkpointmodel.NewReceiveLifecycleState(spec)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRecoveryReducerKeepsTerminalAuthorityClosed(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x81)
	evidence := RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
	}

	for _, phase := range []checkpointmodel.LifecyclePhase{
		checkpointmodel.LifecycleNeedsAttention,
		checkpointmodel.LifecycleDiscarded,
		checkpointmodel.LifecyclePartialDirectory,
	} {
		state := coverageTerminalLifecycle(t, fixture, phase, checkpointmodel.OwnedCleanupPending)
		decision, err := ReduceRecovery(state, evidence, 500)
		if err != nil || decision.Action() != DecisionNoChange {
			t.Fatalf("terminal phase %d recovery = (%d, %v)", phase, decision.Action(), err)
		}
	}

	resumable := lifecycleState(t, fixture, 4, checkpointmodel.LifecycleResumableReceive)
	if decision, err := ReduceRecovery(resumable, evidence, resumable.ExpiresAtMillis()-1); err != nil ||
		decision.Action() != DecisionNoChange {
		t.Fatalf("unexpired recovery = (%d, %v)", decision.Action(), err)
	}
	if _, err := ReduceRecovery(resumable, evidence, resumable.ExpiresAtMillis()); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("receipt-free expiry error = %v", err)
	}
	if _, err := ReduceRecovery(checkpointmodel.ReceiveLifecycleState{}, evidence, 0); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid lifecycle error = %v", err)
	}
	frozen := lifecycleState(t, fixture, 1, checkpointmodel.LifecycleIntentFrozen)
	if _, err := ReduceRecovery(frozen, evidence, 0); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("non-recoverable phase error = %v", err)
	}
	exhausted := lifecycleState(t, fixture, math.MaxUint64, checkpointmodel.LifecycleReceiving)
	if _, err := ReduceRecovery(exhausted, evidence, 1); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("generation exhaustion error = %v", err)
	}
}

func TestDiscardAndTerminalCleanupReducersPreserveProofState(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x82)
	pendingExpired := coverageTerminalLifecycle(
		t, fixture, checkpointmodel.LifecycleExpired, checkpointmodel.OwnedCleanupPending,
	)
	for name, cleanup := range map[string]DiscardEvidence{
		"unknown": {State: CleanupUnknown},
		"pending": {State: CleanupPending},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceDiscard(pendingExpired, EvidenceProven, cleanup)
			if err != nil || (cleanup.State == CleanupPending && decision.Action() != DecisionCleanupRequired) {
				t.Fatalf("expired discard = (%d, %v)", decision.Action(), err)
			}
			if cleanup.State == CleanupUnknown {
				next, ok := decision.Next()
				if !ok || next.AttentionReason() != checkpointmodel.AttentionCleanupUnknown {
					t.Fatalf("unknown cleanup decision = %+v", next)
				}
			}
		})
	}
	cleanExpired := coverageTerminalLifecycle(
		t, fixture, checkpointmodel.LifecycleExpired, checkpointmodel.OwnedCleanupClean,
	)
	if decision, err := ReduceDiscard(cleanExpired, EvidenceProven, DiscardEvidence{State: CleanupPending}); err != nil || decision.Action() != DecisionNoChange {
		t.Fatalf("clean expiry discard = (%d, %v)", decision.Action(), err)
	}
	cleanPublished := coverageTerminalLifecycle(
		t, fixture, checkpointmodel.LifecyclePublished, checkpointmodel.OwnedCleanupClean,
	)
	if decision, err := ReduceRecovery(cleanPublished, RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupComplete,
	}, 0); err != nil || decision.Action() != DecisionNoChange {
		t.Fatalf("clean published recovery = (%d, %v)", decision.Action(), err)
	}

	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	if _, err := ReduceDiscard(receiving, 0, DiscardEvidence{State: CleanupPending}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("invalid discard evidence error = %v", err)
	}
	if got := cleanupState(CleanupComplete); got != checkpointmodel.OwnedCleanupClean {
		t.Fatalf("complete cleanup projection = %d", got)
	}
}

func TestReplaceDecisionRejectsNonAdjacentLifecycleMutation(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x83)
	previous := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	if _, err := replaceDecision(previous, checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 4, Phase: checkpointmodel.LifecycleReceiving,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{fixture.reference}, SuccessCount: 1,
	}); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("generation gap error = %v", err)
	}
}

func TestInventoryOrderingIsStableAcrossOperationAndReason(t *testing.T) {
	first := newAuthorityFixture(t, 0x10)
	second := newAuthorityFixture(t, 0x20)
	firstSummary := summaryFromSnapshot(func() Snapshot {
		state := lifecycleState(t, first, 2, checkpointmodel.LifecycleReceiving)
		snapshot, _ := NewSnapshot(first.operation, state)
		return snapshot
	}())
	secondSummary := summaryFromSnapshot(func() Snapshot {
		state := lifecycleState(t, second, 2, checkpointmodel.LifecycleReceiving)
		snapshot, _ := NewSnapshot(second.operation, state)
		return snapshot
	}())
	cleanupAttention, _ := NewAttention(first.intent.OperationID(), checkpointmodel.AttentionCleanupUnknown)
	ownershipAttention, _ := NewAttention(first.intent.OperationID(), checkpointmodel.AttentionTargetOwnershipUnknown)
	secondAttention, _ := NewAttention(second.intent.OperationID(), checkpointmodel.AttentionCleanupUnknown)
	inventory := newInventory(
		[]Summary{secondSummary, firstSummary},
		[]Attention{secondAttention, cleanupAttention, ownershipAttention},
	)
	if inventory.Summaries()[0].OperationID() != first.intent.OperationID() ||
		inventory.Attention()[0].OperationID() != first.intent.OperationID() ||
		inventory.Attention()[0].Reason() != checkpointmodel.AttentionTargetOwnershipUnknown ||
		inventory.Attention()[1].Reason() != checkpointmodel.AttentionCleanupUnknown {
		t.Fatal("inventory ordering is not deterministic")
	}
}

func TestAuthorityMutationRejectsCanceledAndInvalidRequests(t *testing.T) {
	fixture, _, lease, authority := newCoverageAuthority(t, checkpointmodel.LifecycleReceiving)
	lease.recoveryErr = errors.New("observation unavailable")
	if _, err := authority.Discard(context.Background(), fixture.intent.OperationID()); !errors.Is(err, lease.recoveryErr) {
		t.Fatalf("discard observation error = %v", err)
	}
	if _, err := authority.Discard(
		context.TODO(), receivecontract.OperationID{},
	); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("zero operation error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.Discard(ctx, fixture.intent.OperationID()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}
