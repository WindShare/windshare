package osfs

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type publicResumeFixture struct {
	operation receivecontract.OperationID
	intent    transfer.ReceiveIntentDigest
	snapshot  ResumeStateRepositorySnapshot
	cleanup   []byte
}

func newPublicResumeFixture(t *testing.T, seed byte) publicResumeFixture {
	t.Helper()
	share, _ := catalog.ShareInstanceFromBytes(bytes.Repeat([]byte{seed}, catalog.IdentityBytes))
	root, _ := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{seed + 1}, catalog.IdentityBytes))
	rules, _ := transfer.NewSelectionRules(true, nil)
	selection, _ := transfer.NewSelectionSpec(share, root, rules)
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{seed + 2}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{seed + 3}, receivecontract.StableIdentityBytes))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{seed + 4}, receivecontract.AuthorityRefBytes))
	reservation, _ := receivecontract.NewNativeContainerRootReservation(operation, reservationID, artifact, authority)
	plan, _ := receivecontract.NewDirectTreePlan(artifact, reservation)
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	operationRecord, err := checkpointmodel.NewReceiveOperation(intent, checkpointmodel.NoReopenKey())
	if err != nil {
		t.Fatal(err)
	}
	recordID, _ := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{seed + 5}, 32))
	reference, _ := checkpointmodel.FileCheckpointReferenceFromIdentity(recordID, 2)
	lifecycle, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: operation, ReceiveIntent: intent.Digest(), StateGeneration: 2,
		Phase:          checkpointmodel.LifecycleReceiving,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference}, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence, _ := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{seed + 6}, 32))
	cleanup, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptCleanup, OperationID: operation,
		ReceiveIntent: intent.Digest(), ReservationDigest: intent.BindingDigest(),
		EvidenceDigest: evidence, CleanupGeneration: 1, RemovedObjectCount: 1, RemovedRecordCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	operationBytes, _ := checkpointmodel.EncodeReceiveOperation(operationRecord)
	lifecycleBytes, _ := checkpointmodel.EncodeReceiveLifecycleState(lifecycle)
	return publicResumeFixture{
		operation: operation, intent: intent.Digest(),
		snapshot: ResumeStateRepositorySnapshot{OperationRecord: operationBytes, LifecycleRecord: lifecycleBytes},
		cleanup:  cleanup.CanonicalBytes(),
	}
}

type publicResumeRepository struct {
	operation receivecontract.OperationID
	list      []ResumeStateRepositorySnapshot
	lease     *publicResumeLease
}

func (repository *publicResumeRepository) List(context.Context) ([]ResumeStateRepositorySnapshot, error) {
	return slices.Clone(repository.list), nil
}

func (repository *publicResumeRepository) Acquire(
	_ context.Context,
	operation receivecontract.OperationID,
) (ResumeStateRepositoryLease, error) {
	if operation != repository.operation || repository.lease == nil {
		return nil, ErrResumeStateContract
	}
	return repository.lease, nil
}

type publicResumeLease struct {
	snapshot ResumeStateRepositorySnapshot
	recovery ResumeStateRecoveryEvidence
	cleanup  ResumeStateDiscardEvidence
	calls    []string
}

func (lease *publicResumeLease) Snapshot(context.Context) (ResumeStateRepositorySnapshot, error) {
	lease.calls = append(lease.calls, "snapshot")
	return lease.snapshot, nil
}

func (lease *publicResumeLease) ObserveRecovery(context.Context) (ResumeStateRecoveryEvidence, error) {
	lease.calls = append(lease.calls, "observe")
	return lease.recovery, nil
}

func (lease *publicResumeLease) CleanupOwned(context.Context) (ResumeStateDiscardEvidence, error) {
	lease.calls = append(lease.calls, "cleanup")
	return lease.cleanup, nil
}

func (lease *publicResumeLease) InstallReceipt(_ context.Context, receipt []byte) error {
	if len(receipt) == 0 {
		return ErrResumeStateContract
	}
	lease.calls = append(lease.calls, "receipt")
	return nil
}

func (lease *publicResumeLease) ReplaceLifecycle(_ context.Context, previous, next []byte) error {
	if !bytes.Equal(previous, lease.snapshot.LifecycleRecord) || len(next) == 0 {
		return ErrResumeStateContract
	}
	lease.calls = append(lease.calls, "replace")
	lease.snapshot.LifecycleRecord = slices.Clone(next)
	return nil
}

func (lease *publicResumeLease) Close() error {
	lease.calls = append(lease.calls, "close")
	return nil
}

func TestRepositoryResumeStateAuthorityFreshProcessRecoveryAndUnknownOwnership(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0x31)
	lease := &publicResumeLease{
		snapshot: fixture.snapshot,
		recovery: ResumeStateRecoveryEvidence{
			TargetOwnership: ResumeEvidenceProven, Checkpoints: ResumeEvidenceProven,
			Cleanup: ResumeCleanupPending,
		},
	}
	repository := &publicResumeRepository{
		operation: fixture.operation, list: []ResumeStateRepositorySnapshot{fixture.snapshot}, lease: lease,
	}
	authority, err := NewResumeStateAuthority(repository)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListReady || len(inventory.Summaries()) != 1 ||
		inventory.Summaries()[0].ReceiveIntentDigest() != fixture.intent {
		t.Fatalf("inventory = (%+v, %v)", inventory, err)
	}
	summary, err := authority.Recover(context.Background(), fixture.operation, 100)
	if err != nil || !summary.Resumable() ||
		!slices.Equal(lease.calls, []string{"snapshot", "observe", "replace", "close"}) {
		t.Fatalf("recovery = (%+v, %v), calls=%v", summary, err, lease.calls)
	}

	lease.calls = nil
	lease.snapshot = fixture.snapshot
	lease.recovery.TargetOwnership = ResumeEvidenceUnknown
	summary, err = authority.Recover(context.Background(), fixture.operation, 200)
	if err != nil || summary.NeedsAttentionReason() != "target-ownership-unknown" ||
		!slices.Equal(lease.calls, []string{"snapshot", "observe", "replace", "close"}) {
		t.Fatalf("unknown recovery = (%+v, %v), calls=%v", summary, err, lease.calls)
	}
}

func TestRepositoryResumeStateAuthorityOrdersOwnedCleanupBeforeReceiptAndState(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0x51)
	lease := &publicResumeLease{
		snapshot: fixture.snapshot,
		recovery: ResumeStateRecoveryEvidence{
			TargetOwnership: ResumeEvidenceProven, Checkpoints: ResumeEvidenceProven,
			Cleanup: ResumeCleanupPending,
		},
		cleanup: ResumeStateDiscardEvidence{State: ResumeCleanupComplete, Receipt: fixture.cleanup},
	}
	authority, _ := NewResumeStateAuthority(&publicResumeRepository{
		operation: fixture.operation, lease: lease,
	})
	summary, err := authority.Discard(context.Background(), fixture.operation)
	if err != nil || summary.Phase() != uint8(checkpointmodel.LifecycleDiscarded) ||
		!slices.Equal(lease.calls, []string{"snapshot", "observe", "cleanup", "receipt", "replace", "close"}) {
		t.Fatalf("discard = (%+v, %v), calls=%v", summary, err, lease.calls)
	}
}

func TestRepositoryResumeStateAuthorityListsCorruptLifecycleAsAttention(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0x71)
	corrupt := fixture.snapshot
	corrupt.LifecycleRecord = slices.Clone(corrupt.LifecycleRecord)
	corrupt.LifecycleRecord[0] ^= 1
	authority, _ := NewResumeStateAuthority(&publicResumeRepository{list: []ResumeStateRepositorySnapshot{corrupt}})
	inventory, err := authority.ListResumeState(context.Background())
	if err != nil || inventory.Status() != ResumeStateListNeedsAttention ||
		len(inventory.Summaries()) != 0 || len(inventory.Attention()) != 1 ||
		inventory.Attention()[0].Reason() != "target-ownership-unknown" {
		t.Fatalf("corrupt inventory = (%+v, %v)", inventory, err)
	}
	if _, err := NewResumeStateAuthority(nil); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil repository error = %v", err)
	}
}
