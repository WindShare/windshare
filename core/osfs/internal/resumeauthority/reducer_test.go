package resumeauthority

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type authorityFixture struct {
	operation checkpointmodel.ReceiveOperation
	intent    transfer.ReceiveIntent
	reference checkpointmodel.FileCheckpointReference
	evidence  checkpointmodel.AggregateDigest
}

func newAuthorityFixture(t *testing.T, seed byte) authorityFixture {
	t.Helper()
	var share catalog.ShareInstance
	var root catalog.DirectoryID
	share[0], root[0] = seed, seed+1
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operationID, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{seed + 2}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{seed + 3}, receivecontract.StableIdentityBytes))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{seed + 4}, receivecontract.AuthorityRefBytes))
	reservation, err := receivecontract.NewNativeContainerRootReservation(
		operationID, reservationID, artifact, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := checkpointmodel.NewReceiveOperation(intent, checkpointmodel.NoReopenKey())
	if err != nil {
		t.Fatal(err)
	}
	recordID, _ := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{seed + 5}, 32))
	reference, err := checkpointmodel.FileCheckpointReferenceFromIdentity(recordID, 2)
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{seed + 6}, 32))
	return authorityFixture{operation: operation, intent: intent, reference: reference, evidence: evidence}
}

func lifecycleState(
	t *testing.T,
	fixture authorityFixture,
	generation uint64,
	phase checkpointmodel.LifecyclePhase,
) checkpointmodel.ReceiveLifecycleState {
	t.Helper()
	spec := checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: generation, Phase: phase,
	}
	switch phase {
	case checkpointmodel.LifecycleReceiving, checkpointmodel.LifecycleFinalizingTree:
		spec.CheckpointRefs = []checkpointmodel.FileCheckpointReference{fixture.reference}
		spec.SuccessCount = 1
	case checkpointmodel.LifecycleResumableReceive:
		spec.CheckpointRefs = []checkpointmodel.FileCheckpointReference{fixture.reference}
		spec.ExpiresAtMillis = 1_000
		spec.SuccessCount = 1
	}
	state, err := checkpointmodel.NewReceiveLifecycleState(spec)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func receiptFixture(
	t *testing.T,
	fixture authorityFixture,
	kind checkpointmodel.DirectTreeReceiptKind,
) checkpointmodel.DirectTreeReceipt {
	t.Helper()
	spec := checkpointmodel.DirectTreeReceiptSpec{
		Kind: kind, OperationID: fixture.intent.OperationID(),
		ReceiveIntent: fixture.intent.Digest(), ReservationDigest: fixture.intent.BindingDigest(),
		EvidenceDigest: fixture.evidence,
	}
	switch kind {
	case checkpointmodel.ReceiptTreeCompletion:
		spec.CheckpointRefs = []checkpointmodel.FileCheckpointReference{fixture.reference}
		spec.SuccessCount = 1
	case checkpointmodel.ReceiptPartialDirectory:
		spec.CheckpointRefs = []checkpointmodel.FileCheckpointReference{fixture.reference}
		spec.SuccessCount, spec.FailureCount = 1, 2
		spec.PartialReason = checkpointmodel.PartialDirectoryFailures
	case checkpointmodel.ReceiptCleanup:
		spec.CleanupGeneration = 1
		spec.RemovedObjectCount, spec.RemovedRecordCount = 1, 2
	case checkpointmodel.ReceiptExpiry:
		spec.CheckpointRefs = []checkpointmodel.FileCheckpointReference{fixture.reference}
		spec.SuccessCount = 1
		spec.CleanupGeneration = 1
	}
	receipt, err := checkpointmodel.NewDirectTreeReceipt(spec)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestRecoveryReducerNeverConvertsUnknownOwnershipIntoResumeState(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x21)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	for name, evidence := range map[string]RecoveryEvidence{
		"target": {
			TargetOwnership: EvidenceUnknown, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
		},
		"checkpoints": {
			TargetOwnership: EvidenceProven, Checkpoints: EvidenceUnknown, Cleanup: CleanupPending,
		},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceRecovery(receiving, evidence, 10)
			if err != nil {
				t.Fatal(err)
			}
			next, ok := decision.Next()
			if !ok || next.Phase() != checkpointmodel.LifecycleNeedsAttention ||
				next.AttentionReason() != checkpointmodel.AttentionTargetOwnershipUnknown {
				t.Fatalf("unknown evidence decision = (%d, %+v)", decision.Action(), next)
			}
			if next.AttentionReason().String() != "target-ownership-unknown" {
				t.Fatalf("attention reason = %q", next.AttentionReason().String())
			}
		})
	}
	decision, err := ReduceRecovery(receiving, RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
	}, 50)
	if err != nil {
		t.Fatal(err)
	}
	next, ok := decision.Next()
	if !ok || next.Phase() != checkpointmodel.LifecycleResumableReceive ||
		next.ExpiresAtMillis() != 50+checkpointmodel.StableRetentionMilliseconds ||
		next.CheckpointReferences()[0] != fixture.reference {
		t.Fatalf("verified recovery = %+v", next)
	}
}

func TestFinalizingRecoveryUsesImmutableReceiptsNotDuplicateRanges(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x31)
	finalizing := lifecycleState(t, fixture, 3, checkpointmodel.LifecycleFinalizingTree)
	for name, receipt := range map[string]checkpointmodel.DirectTreeReceipt{
		"complete": receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion),
		"partial":  receiptFixture(t, fixture, checkpointmodel.ReceiptPartialDirectory),
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceRecovery(finalizing, RecoveryEvidence{
				TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven,
				Cleanup: CleanupPending, TerminalReceipt: receipt,
			}, 100)
			if err != nil {
				t.Fatal(err)
			}
			next, ok := decision.Next()
			if !ok || next.ReceiptDigest() != receipt.Digest() ||
				next.CheckpointReferences()[0] != fixture.reference {
				t.Fatalf("terminal recovery = %+v", next)
			}
			if name == "complete" && next.Phase() != checkpointmodel.LifecyclePublished {
				t.Fatalf("completion phase = %d", next.Phase())
			}
			if name == "partial" && (next.Phase() != checkpointmodel.LifecyclePartialDirectory ||
				next.PartialReason() != checkpointmodel.PartialDirectoryFailures) {
				t.Fatalf("partial phase = %+v", next)
			}
		})
	}
}

func TestExpiryAndCleanupUnknownUseOnlyCleanupAttention(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x41)
	resumable := lifecycleState(t, fixture, 4, checkpointmodel.LifecycleResumableReceive)
	decision, err := ReduceRecovery(resumable, RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupUnknown,
	}, resumable.ExpiresAtMillis())
	if err != nil {
		t.Fatal(err)
	}
	next, _ := decision.Next()
	if next.AttentionReason() != checkpointmodel.AttentionCleanupUnknown ||
		next.AttentionReason().String() != "cleanup-unknown" {
		t.Fatalf("expired unknown cleanup = %+v", next)
	}

	expiryReceipt := receiptFixture(t, fixture, checkpointmodel.ReceiptExpiry)
	decision, err = ReduceRecovery(resumable, RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven,
		Cleanup: CleanupPending, ExpiryReceipt: expiryReceipt,
	}, resumable.ExpiresAtMillis())
	if err != nil {
		t.Fatal(err)
	}
	next, _ = decision.Next()
	if next.Phase() != checkpointmodel.LifecycleExpired ||
		next.ReceiptDigest() != expiryReceipt.Digest() ||
		next.PriorStableState() != checkpointmodel.LifecycleResumableReceive {
		t.Fatalf("expired state = %+v", next)
	}
}

func TestDiscardReducerPreservesSuccessfulOutputsAndOrdersCleanup(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x51)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	cleanupReceipt := receiptFixture(t, fixture, checkpointmodel.ReceiptCleanup)
	for name, test := range map[string]struct {
		target  EvidenceState
		cleanup DiscardEvidence
		want    DecisionAction
		reason  checkpointmodel.NeedsAttentionReason
	}{
		"ownership-unknown": {
			EvidenceUnknown, DiscardEvidence{State: CleanupPending}, DecisionReplace,
			checkpointmodel.AttentionTargetOwnershipUnknown,
		},
		"cleanup-unknown": {
			EvidenceProven, DiscardEvidence{State: CleanupUnknown}, DecisionReplace,
			checkpointmodel.AttentionCleanupUnknown,
		},
		"cleanup-required": {
			EvidenceProven, DiscardEvidence{State: CleanupPending}, DecisionCleanupRequired, 0,
		},
		"discarded": {
			EvidenceProven, DiscardEvidence{State: CleanupComplete, Receipt: cleanupReceipt}, DecisionReplace, 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceDiscard(receiving, test.target, test.cleanup)
			if err != nil || decision.Action() != test.want {
				t.Fatalf("discard = (%d, %v), want %d", decision.Action(), err, test.want)
			}
			if next, ok := decision.Next(); ok {
				if test.reason != 0 && next.AttentionReason() != test.reason {
					t.Fatalf("discard attention = %d", next.AttentionReason())
				}
				if test.reason == 0 && next.Phase() != checkpointmodel.LifecycleDiscarded {
					t.Fatalf("discard phase = %d", next.Phase())
				}
			}
		})
	}
	partialReceipt := receiptFixture(t, fixture, checkpointmodel.ReceiptPartialDirectory)
	partial, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 5, Phase: checkpointmodel.LifecyclePartialDirectory,
		CheckpointRefs: partialReceipt.CheckpointReferences(), ReceiptDigest: partialReceipt.Digest(),
		SuccessCount: 1, FailureCount: 2, PartialReason: checkpointmodel.PartialDirectoryFailures,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := ReduceDiscard(
		partial, EvidenceProven, DiscardEvidence{State: CleanupComplete, Receipt: cleanupReceipt},
	)
	if err != nil || decision.Action() != DecisionNoChange {
		t.Fatalf("partial discard = (%d, %v)", decision.Action(), err)
	}
	expiryReceipt := receiptFixture(t, fixture, checkpointmodel.ReceiptExpiry)
	expired, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 5, Phase: checkpointmodel.LifecycleExpired,
		CheckpointRefs: expiryReceipt.CheckpointReferences(), ReceiptDigest: expiryReceipt.Digest(),
		ExpiresAtMillis: 1_000, SuccessCount: 1, CleanupState: checkpointmodel.OwnedCleanupPending,
		PriorStableState: checkpointmodel.LifecycleResumableReceive,
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err = ReduceDiscard(
		expired, EvidenceProven, DiscardEvidence{State: CleanupComplete, Receipt: cleanupReceipt},
	)
	next, replaced := decision.Next()
	if err != nil || !replaced || next.Phase() != checkpointmodel.LifecycleExpired ||
		next.CleanupState() != checkpointmodel.OwnedCleanupClean ||
		next.ReceiptDigest() != expiryReceipt.Digest() {
		t.Fatalf("expired discard cleanup = (%+v, %v)", next, err)
	}
	decision, err = ReduceDiscard(
		expired, EvidenceUnknown, DiscardEvidence{State: CleanupPending},
	)
	next, replaced = decision.Next()
	if err != nil || !replaced || next.Phase() != checkpointmodel.LifecycleNeedsAttention ||
		next.AttentionReason() != checkpointmodel.AttentionTargetOwnershipUnknown {
		t.Fatalf("expired unknown ownership = (%+v, %v)", next, err)
	}
}

type memoryAuthorityStore struct {
	list  []Snapshot
	lease *memoryOperationLease
	err   error
}

func (store *memoryAuthorityStore) List(context.Context) ([]Snapshot, error) {
	return append([]Snapshot(nil), store.list...), store.err
}
func (store *memoryAuthorityStore) Acquire(
	_ context.Context,
	operation receivecontract.OperationID,
) (OperationLease, error) {
	if store.err != nil {
		return nil, store.err
	}
	if store.lease == nil || store.lease.snapshot.operation.OperationID() != operation {
		return nil, ErrInvalidContract
	}
	return store.lease, nil
}

type memoryOperationLease struct {
	snapshot     Snapshot
	recovery     RecoveryEvidence
	cleanup      DiscardEvidence
	replacements int
	cleanupCalls int
	installed    []checkpointmodel.AggregateDigest
	closed       bool
}

func (lease *memoryOperationLease) Snapshot(context.Context) (Snapshot, error) {
	return lease.snapshot, nil
}
func (lease *memoryOperationLease) ObserveRecovery(context.Context) (RecoveryEvidence, error) {
	return lease.recovery, nil
}
func (lease *memoryOperationLease) CleanupOwned(context.Context) (DiscardEvidence, error) {
	lease.cleanupCalls++
	return lease.cleanup, nil
}
func (lease *memoryOperationLease) InstallReceipt(
	_ context.Context,
	receipt checkpointmodel.DirectTreeReceipt,
) error {
	lease.installed = append(lease.installed, receipt.Digest())
	return nil
}
func (lease *memoryOperationLease) ReplaceLifecycle(
	_ context.Context,
	previous checkpointmodel.ReceiveLifecycleState,
	next checkpointmodel.ReceiveLifecycleState,
) error {
	if lease.snapshot.lifecycle.StateGeneration() != previous.StateGeneration() {
		return ErrInvalidContract
	}
	lease.snapshot.lifecycle = next
	lease.replacements++
	return nil
}
func (lease *memoryOperationLease) Close() error {
	lease.closed = true
	return nil
}

func TestAuthorityRevalidatesSnapshotsBeforeRecoverAndDiscard(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x61)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	snapshot, err := NewSnapshot(fixture.operation, receiving)
	if err != nil {
		t.Fatal(err)
	}
	lease := &memoryOperationLease{
		snapshot: snapshot,
		recovery: RecoveryEvidence{
			TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
		},
		cleanup: DiscardEvidence{
			State: CleanupComplete, Receipt: receiptFixture(t, fixture, checkpointmodel.ReceiptCleanup),
		},
	}
	store := &memoryAuthorityStore{list: []Snapshot{snapshot}, lease: lease}
	authority, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.List(context.Background())
	if err != nil || inventory.Status() != ListReady || len(inventory.Summaries()) != 1 ||
		inventory.Summaries()[0].OperationID() != fixture.intent.OperationID() {
		t.Fatalf("inventory = (%+v, %v)", inventory, err)
	}
	summary, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 100)
	if err != nil || !summary.Resumable() || lease.replacements != 1 || !lease.closed {
		t.Fatalf("recover = (%+v, %v), replacements=%d closed=%t", summary, err, lease.replacements, lease.closed)
	}

	lease.closed = false
	lease.recovery = RecoveryEvidence{
		TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: CleanupPending,
	}
	summary, err = authority.Discard(context.Background(), fixture.intent.OperationID())
	if err != nil || summary.Phase() != checkpointmodel.LifecycleDiscarded ||
		lease.cleanupCalls != 1 || len(lease.installed) != 1 || lease.replacements != 2 {
		t.Fatalf("discard = (%+v, %v), cleanup=%d receipts=%d replacements=%d",
			summary, err, lease.cleanupCalls, len(lease.installed), lease.replacements)
	}
}

func TestAuthorityInstallsOnlyReceiptSelectedByRecovery(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x68)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	snapshot, _ := NewSnapshot(fixture.operation, receiving)
	terminal := receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion)
	lease := &memoryOperationLease{
		snapshot: snapshot,
		recovery: RecoveryEvidence{
			TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven,
			Cleanup: CleanupPending, TerminalReceipt: terminal,
		},
	}
	authority, _ := New(&memoryAuthorityStore{lease: lease})
	if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 100); err != nil {
		t.Fatal(err)
	}
	if len(lease.installed) != 0 {
		t.Fatalf("receiving recovery installed irrelevant receipts = %x", lease.installed)
	}

	finalizing := lifecycleState(t, fixture, 3, checkpointmodel.LifecycleFinalizingTree)
	lease.snapshot, _ = NewSnapshot(fixture.operation, finalizing)
	lease.installed = nil
	lease.recovery.ExpiryReceipt = receiptFixture(t, fixture, checkpointmodel.ReceiptExpiry)
	if _, err := authority.Recover(context.Background(), fixture.intent.OperationID(), 100); err != nil {
		t.Fatal(err)
	}
	if len(lease.installed) != 1 || lease.installed[0] != terminal.Digest() {
		t.Fatalf("selected recovery receipts = %x", lease.installed)
	}
}

func TestAuthorityRejectsDuplicateOrForeignSnapshots(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x71)
	receiving := lifecycleState(t, fixture, 2, checkpointmodel.LifecycleReceiving)
	snapshot, _ := NewSnapshot(fixture.operation, receiving)
	authority, _ := New(&memoryAuthorityStore{list: []Snapshot{snapshot, snapshot}})
	inventory, err := authority.List(context.Background())
	if err != nil || inventory.Status() != ListNeedsAttention ||
		len(inventory.Summaries()) != 0 || len(inventory.Attention()) != 1 ||
		inventory.Attention()[0].Reason() != checkpointmodel.AttentionTargetOwnershipUnknown {
		t.Fatalf("ambiguous inventory = (%+v, %v)", inventory, err)
	}
	if _, err := New(nil); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("nil store error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authority.List(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list error = %v", err)
	}
}

func TestPublishedCleanupRemainsPublishedUnlessOwnershipIsUnknown(t *testing.T) {
	fixture := newAuthorityFixture(t, 0x81)
	receipt := receiptFixture(t, fixture, checkpointmodel.ReceiptTreeCompletion)
	published, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 4, Phase: checkpointmodel.LifecyclePublished,
		CheckpointRefs: receipt.CheckpointReferences(), ReceiptDigest: receipt.Digest(),
		SuccessCount: receipt.SuccessCount(), CleanupState: checkpointmodel.OwnedCleanupPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, cleanup := range map[string]CleanupEvidenceState{
		"pending":  CleanupPending,
		"complete": CleanupComplete,
		"unknown":  CleanupUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			decision, err := ReduceRecovery(published, RecoveryEvidence{
				TargetOwnership: EvidenceProven, Checkpoints: EvidenceProven, Cleanup: cleanup,
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			next, replaced := decision.Next()
			switch cleanup {
			case CleanupPending:
				if decision.Action() != DecisionNoChange {
					t.Fatalf("pending cleanup action = %d", decision.Action())
				}
			case CleanupComplete:
				if !replaced || next.Phase() != checkpointmodel.LifecyclePublished ||
					next.CleanupState() != checkpointmodel.OwnedCleanupClean {
					t.Fatalf("completed cleanup = %+v", next)
				}
			case CleanupUnknown:
				if !replaced || next.AttentionReason() != checkpointmodel.AttentionCleanupUnknown {
					t.Fatalf("unknown cleanup = %+v", next)
				}
			}
		})
	}

	snapshot, _ := NewSnapshot(fixture.operation, published)
	if snapshot.Operation().OperationID() != fixture.intent.OperationID() ||
		snapshot.Lifecycle().Phase() != checkpointmodel.LifecyclePublished {
		t.Fatal("snapshot accessors lost operation state")
	}
	summary := summaryFromSnapshot(snapshot)
	if summary.ReceiveIntentDigest() != fixture.intent.Digest() ||
		summary.StateGeneration() != 4 || summary.ExpiresAtMillis() != 0 ||
		summary.SuccessCount() != 1 || summary.FailureCount() != 0 ||
		summary.NeedsAttentionReason() != 0 {
		t.Fatalf("summary accessors = %+v", summary)
	}
	attention, err := NewAttention(fixture.intent.OperationID(), checkpointmodel.AttentionCleanupUnknown)
	if err != nil || attention.OperationID() != fixture.intent.OperationID() {
		t.Fatalf("attention accessor = %+v, %v", attention, err)
	}
}
