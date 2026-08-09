package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type coverageResumeRepository struct {
	list       []ResumeStateRepositorySnapshot
	listErr    error
	lease      ResumeStateRepositoryLease
	acquireErr error
}

func (repository *coverageResumeRepository) List(
	context.Context,
) ([]ResumeStateRepositorySnapshot, error) {
	return append([]ResumeStateRepositorySnapshot(nil), repository.list...), repository.listErr
}

func (repository *coverageResumeRepository) Acquire(
	context.Context,
	receivecontract.OperationID,
) (ResumeStateRepositoryLease, error) {
	return repository.lease, repository.acquireErr
}

type coverageResumeLease struct {
	snapshot ResumeStateRepositorySnapshot
	recovery ResumeStateRecoveryEvidence
	cleanup  ResumeStateDiscardEvidence

	snapshotErr error
	recoveryErr error
	cleanupErr  error
	installErr  error
	replaceErr  error
	closeErr    error
}

func (lease *coverageResumeLease) Snapshot(context.Context) (ResumeStateRepositorySnapshot, error) {
	return lease.snapshot, lease.snapshotErr
}

func (lease *coverageResumeLease) ObserveRecovery(context.Context) (ResumeStateRecoveryEvidence, error) {
	return lease.recovery, lease.recoveryErr
}

func (lease *coverageResumeLease) CleanupOwned(context.Context) (ResumeStateDiscardEvidence, error) {
	return lease.cleanup, lease.cleanupErr
}

func (lease *coverageResumeLease) InstallReceipt(context.Context, []byte) error {
	return lease.installErr
}

func (lease *coverageResumeLease) ReplaceLifecycle(context.Context, []byte, []byte) error {
	return lease.replaceErr
}

func (lease *coverageResumeLease) Close() error { return lease.closeErr }

func TestResumeStateAuthorityW6RejectsMalformedFreshProcessImages(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0x81)
	snapshot, err := decodeResumeSnapshot(fixture.snapshot)
	if err != nil || snapshot.Operation().OperationID() != fixture.operation ||
		snapshot.Lifecycle().Phase() != checkpointmodel.LifecycleReceiving {
		t.Fatalf("decoded snapshot = (%+v, %v)", snapshot, err)
	}

	corruptOperation := fixture.snapshot
	corruptOperation.OperationRecord = []byte("not-an-operation")
	if _, err := decodeResumeSnapshot(corruptOperation); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("corrupt operation error = %v", err)
	}
	corruptLifecycle := fixture.snapshot
	corruptLifecycle.LifecycleRecord = []byte("not-a-lifecycle")
	if _, err := decodeResumeSnapshot(corruptLifecycle); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("corrupt lifecycle error = %v", err)
	}
	corruptBridge := repositoryBridge{repository: &coverageResumeRepository{
		list: []ResumeStateRepositorySnapshot{corruptOperation},
	}}
	if _, err := corruptBridge.List(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("unidentifiable inventory error = %v", err)
	}

	receipt, err := decodeOptionalReceipt(fixture.cleanup)
	if err != nil || receipt.Kind() != checkpointmodel.ReceiptCleanup {
		t.Fatalf("cleanup receipt = (%d, %v)", receipt.Kind(), err)
	}
	if empty, err := decodeOptionalReceipt(nil); err != nil || empty.Valid() {
		t.Fatalf("empty receipt = (%+v, %v)", empty, err)
	}
	if _, err := decodeOptionalReceipt([]byte("not-a-receipt")); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("corrupt receipt error = %v", err)
	}
}

func TestResumeStateAuthorityW6BridgePropagatesLeaseFailures(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0x91)
	operationErr := errors.New("operation failed")

	listBridge := repositoryBridge{repository: &coverageResumeRepository{listErr: operationErr}}
	if _, err := listBridge.List(context.Background()); !errors.Is(err, operationErr) {
		t.Fatalf("list error = %v", err)
	}
	acquireBridge := repositoryBridge{repository: &coverageResumeRepository{acquireErr: operationErr}}
	if _, err := acquireBridge.Acquire(context.Background(), fixture.operation); !errors.Is(err, operationErr) {
		t.Fatalf("acquire error = %v", err)
	}
	nilLeaseBridge := repositoryBridge{repository: &coverageResumeRepository{}}
	if _, err := nilLeaseBridge.Acquire(context.Background(), fixture.operation); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil acquired lease error = %v", err)
	}

	lease := &coverageResumeLease{snapshot: fixture.snapshot}
	bridge := &leaseBridge{lease: lease}
	lease.snapshotErr = operationErr
	if _, err := bridge.Snapshot(context.Background()); !errors.Is(err, operationErr) {
		t.Fatalf("snapshot error = %v", err)
	}
	lease.snapshotErr = nil

	lease.recoveryErr = operationErr
	if _, err := bridge.ObserveRecovery(context.Background()); !errors.Is(err, operationErr) {
		t.Fatalf("recovery observation error = %v", err)
	}
	lease.recoveryErr = nil
	lease.recovery = ResumeStateRecoveryEvidence{TerminalReceipt: []byte("bad")}
	if _, err := bridge.ObserveRecovery(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("terminal receipt error = %v", err)
	}
	lease.recovery = ResumeStateRecoveryEvidence{ExpiryReceipt: []byte("bad")}
	if _, err := bridge.ObserveRecovery(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("expiry receipt error = %v", err)
	}

	lease.cleanupErr = operationErr
	if _, err := bridge.CleanupOwned(context.Background()); !errors.Is(err, operationErr) {
		t.Fatalf("cleanup error = %v", err)
	}
	lease.cleanupErr = nil
	lease.cleanup = ResumeStateDiscardEvidence{Receipt: []byte("bad")}
	if _, err := bridge.CleanupOwned(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("cleanup receipt error = %v", err)
	}

	if err := bridge.ReplaceLifecycle(
		context.Background(), checkpointmodel.ReceiveLifecycleState{}, checkpointmodel.ReceiveLifecycleState{},
	); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("invalid lifecycle replacement error = %v", err)
	}
	lease.installErr = operationErr
	if err := bridge.InstallReceipt(context.Background(), receiptFixtureForPublicBridge(t, fixture)); !errors.Is(err, operationErr) {
		t.Fatalf("receipt install error = %v", err)
	}
	lease.closeErr = operationErr
	if err := bridge.Close(); !errors.Is(err, operationErr) {
		t.Fatalf("lease close error = %v", err)
	}
}

func receiptFixtureForPublicBridge(
	t *testing.T,
	fixture publicResumeFixture,
) checkpointmodel.DirectTreeReceipt {
	t.Helper()
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(fixture.cleanup)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestResumeStateAuthorityW6NilReceiverAndSummaryVocabulary(t *testing.T) {
	fixture := newPublicResumeFixture(t, 0xa1)
	lease := &coverageResumeLease{
		snapshot: fixture.snapshot,
		recovery: ResumeStateRecoveryEvidence{
			TargetOwnership: ResumeEvidenceProven, Checkpoints: ResumeEvidenceProven,
			Cleanup: ResumeCleanupPending,
		},
	}
	authority, err := NewResumeStateAuthority(&coverageResumeRepository{lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := authority.Recover(context.Background(), fixture.operation, 100)
	if err != nil || summary.OperationID() != fixture.operation ||
		summary.ReceiveIntentDigest() != fixture.intent ||
		summary.Phase() != uint8(checkpointmodel.LifecycleResumableReceive) ||
		summary.StateGeneration() != 3 || summary.ExpiresAtMillis() == 0 ||
		summary.SuccessCount() != 1 || summary.FailureCount() != 0 || !summary.Resumable() {
		t.Fatalf("summary vocabulary = (%+v, %v)", summary, err)
	}

	var nilAuthority *RepositoryResumeStateAuthority
	if _, err := nilAuthority.ListResumeState(context.Background()); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil list authority error = %v", err)
	}
	if _, err := nilAuthority.Recover(context.Background(), fixture.operation, 1); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil recover authority error = %v", err)
	}
	if _, err := nilAuthority.Discard(context.Background(), fixture.operation); !errors.Is(err, ErrResumeStateContract) {
		t.Fatalf("nil discard authority error = %v", err)
	}
}
