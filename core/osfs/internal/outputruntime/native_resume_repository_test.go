package outputruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeResumeRepositoryListsAndReacquiresOneCertifiedOperation(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := repository.List(context.Background())
	if err != nil || len(initial) != 0 {
		t.Fatalf("initial list = (%d, %v)", len(initial), err)
	}
	if _, err := os.Lstat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only list created control state: %v", err)
	}

	authority := newNativeReservationTestAuthority(t, root)
	reserved, err := authority.ReserveDirectTree(
		context.Background(), nativeReservationTestSelection(t, 0x81),
		receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil || reserved.Kind() != NativeDirectTreeReserved {
		t.Fatalf("reserve = (%d, %v)", reserved.Kind(), err)
	}
	intent, ok := reserved.ReceiveIntent()
	if !ok {
		t.Fatal("reservation exposed no receive intent")
	}

	snapshots, err := repository.List(context.Background())
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("reserved list = (%d, %v)", len(snapshots), err)
	}
	operation, err := checkpointmodel.DecodeReceiveOperation(snapshots[0].OperationRecord)
	if err != nil || operation.OperationID() != intent.OperationID() {
		t.Fatalf("listed operation = (%v, %v)", operation.OperationID(), err)
	}
	lifecycle, err := checkpointmodel.DecodeReceiveLifecycleState(snapshots[0].LifecycleRecord)
	if err != nil || lifecycle.Phase() != checkpointmodel.LifecycleIntentFrozen {
		t.Fatalf("listed lifecycle = (%d, %v)", lifecycle.Phase(), err)
	}

	platform, namespace, held := holdNativeReservationOperation(t, root, intent)
	if _, err := repository.List(context.Background()); !errors.Is(err, ErrNativeResumeBusy) {
		t.Fatalf("busy list error = %v", err)
	}
	if _, err := repository.Acquire(context.Background(), intent.OperationID()); !errors.Is(err, ErrNativeResumeBusy) {
		t.Fatalf("busy acquire error = %v", err)
	}
	if err := errors.Join(held.Close(), namespace.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}

	lease, err := repository.Acquire(context.Background(), intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operation, err = checkpointmodel.DecodeReceiveOperation(snapshot.OperationRecord)
	if err != nil || operation.OperationID() != intent.OperationID() {
		t.Fatalf("leased operation = (%v, %v)", operation.OperationID(), err)
	}
	evidence, err := lease.ObserveRecovery(context.Background())
	if err != nil || evidence.TargetOwnership != NativeResumeEvidenceProven ||
		evidence.Checkpoints != NativeResumeEvidenceProven || evidence.Cleanup != NativeResumeCleanupPending ||
		len(evidence.TerminalReceipt) != 0 || len(evidence.ExpiryReceipt) != 0 {
		t.Fatalf("frozen evidence = (%+v, %v)", evidence, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent close = %v", err)
	}
	unknown := incrementalTestIdentity16[receivecontract.OperationID](0xfe)
	if _, err := repository.Acquire(context.Background(), unknown); err == nil {
		t.Fatal("unknown operation unexpectedly acquired a lease")
	}
}

func TestNativeResumeRepositoryCleansPausedObjectsAndOnlyEmptyOwnedDirectories(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	intent := nativeTestIntent(t, root, 0x91, 0x92)
	session := openNativeCompositionSession(t, root, false, intent, nil)
	rootAdmission := admitNativeRoot(t, session, intent, 0x93, catalog.ModifiedTime{})
	emptyAdmission := admitNativeResumeDirectory(t, session, rootAdmission, "empty", 0x94, 0x95)
	keptAdmission := admitNativeResumeDirectory(t, session, rootAdmission, "kept", 0x96, 0x97)

	zero := nativeCompositionDescriptor(t, intent, 0x98, 0x99, 0)
	zeroStart, err := session.BeginFile(context.Background(), nativeCompositionFile(
		t, session, zero, "kept/zero.bin", keptAdmission,
	))
	if err != nil {
		t.Fatal(err)
	}
	zeroTransaction, durable, ok := zeroStart.Transaction()
	if !ok || !durable.Ranges().IsEmpty() {
		t.Fatalf("zero-byte start = (transaction=%T, ranges=%v)", zeroTransaction, durable.Ranges().Ranges())
	}
	if settlement, err := zeroTransaction.Commit(context.Background()); err != nil ||
		settlement.Kind() != transfer.FilePublished {
		t.Fatalf("zero-byte commit = (%d, %v)", settlement.Kind(), err)
	}

	paused := nativeCompositionDescriptor(t, intent, 0x9a, 0x9b, 4)
	pausedStart, err := session.BeginFile(context.Background(), nativeCompositionFile(
		t, session, paused, "empty/paused.bin", emptyAdmission,
	))
	if err != nil {
		t.Fatal(err)
	}
	pausedTransaction, _, ok := pausedStart.Transaction()
	if !ok {
		t.Fatal("paused file settled before receiving content")
	}
	if err := pausedTransaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := pausedTransaction.Checkpoint(context.Background())
	if err != nil || len(checkpoint.Ranges().Ranges()) != 1 {
		t.Fatalf("paused checkpoint = (%v, %v)", checkpoint.Ranges().Ranges(), err)
	}
	if settlement, err := pausedTransaction.Pause(
		context.Background(), transfer.FilePauseInterrupted,
	); err != nil || settlement.Kind() != transfer.FilePaused {
		t.Fatalf("file pause = (%d, %v)", settlement.Kind(), err)
	}
	if settlement, err := session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || settlement.Kind() != transfer.DirectTreeSettlementResumable {
		t.Fatalf("tree pause = (%d, %v)", settlement.Kind(), err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(context.Background(), intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	snapshot, err := lease.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := checkpointmodel.DecodeReceiveLifecycleState(snapshot.LifecycleRecord)
	if err != nil || lifecycle.Phase() != checkpointmodel.LifecycleResumableReceive ||
		len(lifecycle.CheckpointReferences()) != 2 {
		t.Fatalf("paused lifecycle = (phase=%d refs=%d err=%v)", lifecycle.Phase(), len(lifecycle.CheckpointReferences()), err)
	}
	evidence, err := lease.ObserveRecovery(context.Background())
	if err != nil || evidence.TargetOwnership != NativeResumeEvidenceProven ||
		evidence.Checkpoints != NativeResumeEvidenceProven || evidence.Cleanup != NativeResumeCleanupPending {
		t.Fatalf("paused evidence = (%+v, %v)", evidence, err)
	}
	expiry, err := checkpointmodel.DecodeDirectTreeReceipt(evidence.ExpiryReceipt)
	if err != nil || expiry.Kind() != checkpointmodel.ReceiptExpiry {
		t.Fatalf("expiry receipt = (%d, %v)", expiry.Kind(), err)
	}

	cleanup, err := lease.CleanupOwned(context.Background())
	if err != nil || cleanup.State != NativeResumeCleanupComplete {
		t.Fatalf("cleanup = (%+v, %v)", cleanup, err)
	}
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(cleanup.Receipt)
	if err != nil || receipt.Kind() != checkpointmodel.ReceiptCleanup || receipt.RemovedObjectCount() != 3 {
		t.Fatalf("cleanup receipt = (kind=%d removed=%d err=%v)", receipt.Kind(), receipt.RemovedObjectCount(), err)
	}
	if _, err := os.Lstat(filepath.Join(root, "empty")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty owned directory survived cleanup: %v", err)
	}
	if info, err := os.Stat(filepath.Join(root, "kept", "zero.bin")); err != nil || info.Size() != 0 {
		t.Fatalf("published zero-byte file = (%v, %v)", info, err)
	}
	if err := lease.InstallReceipt(context.Background(), cleanup.Receipt); err != nil {
		t.Fatal(err)
	}
	next, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: intent.OperationID(), ReceiveIntent: intent.Digest(),
		StateGeneration: lifecycle.StateGeneration() + 1, Phase: checkpointmodel.LifecycleDiscarded,
		ReceiptDigest: receipt.Digest(), CleanupState: checkpointmodel.OwnedCleanupClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	nextBytes, err := checkpointmodel.EncodeReceiveLifecycleState(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.ReplaceLifecycle(context.Background(), snapshot.LifecycleRecord, nextBytes); err != nil {
		t.Fatal(err)
	}
	updated, err := lease.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	updatedLifecycle, err := checkpointmodel.DecodeReceiveLifecycleState(updated.LifecycleRecord)
	if err != nil || updatedLifecycle.Phase() != checkpointmodel.LifecycleDiscarded {
		t.Fatalf("discarded lifecycle = (%d, %v)", updatedLifecycle.Phase(), err)
	}
}

func TestNativeResumeRepositoryObservesPartialReceiptWithoutReplacingCollisions(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	if err := os.WriteFile(filepath.Join(root, "collision.bin"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	intent := nativeTestIntent(t, root, 0xa1, 0xa2)
	session := openNativeCompositionSession(t, root, false, intent, nil)
	rootAdmission := admitNativeRoot(t, session, intent, 0xa3, catalog.ModifiedTime{})

	success := nativeCompositionDescriptor(t, intent, 0xa4, 0xa5, 4)
	start, err := session.BeginFile(context.Background(), nativeCompositionFile(
		t, session, success, "success.bin", rootAdmission,
	))
	if err != nil {
		t.Fatal(err)
	}
	transaction, _, ok := start.Transaction()
	if !ok {
		t.Fatal("successful file settled before receiving content")
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("good")); err != nil {
		t.Fatal(err)
	}
	if settlement, err := transaction.Commit(context.Background()); err != nil ||
		settlement.Kind() != transfer.FilePublished {
		t.Fatalf("successful commit = (%d, %v)", settlement.Kind(), err)
	}

	collision := nativeCompositionDescriptor(t, intent, 0xa6, 0xa7, 4)
	collisionStart, err := session.BeginFile(context.Background(), nativeCompositionFile(
		t, session, collision, "collision.bin", rootAdmission,
	))
	if err != nil {
		t.Fatal(err)
	}
	collisionSettlement, immediate := collisionStart.ImmediateSettlement()
	if !immediate || collisionSettlement.Kind() != transfer.FileCollision {
		t.Fatalf("collision settlement = (%d, immediate=%t)", collisionSettlement.Kind(), immediate)
	}
	if _, err := session.FinalizeDirectory(context.Background(), rootAdmission); err != nil {
		t.Fatal(err)
	}
	if settlement, err := session.FinalizeTree(
		context.Background(), transfer.DirectTreeOutcomePartialDirectory,
	); err != nil || settlement.Kind() != transfer.DirectTreeSettlementPartialDirectory {
		t.Fatalf("partial tree = (%d, %v)", settlement.Kind(), err)
	}

	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.Acquire(context.Background(), intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	evidence, err := lease.ObserveRecovery(context.Background())
	if err != nil || evidence.TargetOwnership != NativeResumeEvidenceProven ||
		evidence.Checkpoints != NativeResumeEvidenceProven {
		t.Fatalf("partial evidence = (%+v, %v)", evidence, err)
	}
	receipt, err := checkpointmodel.DecodeDirectTreeReceipt(evidence.TerminalReceipt)
	if err != nil || receipt.Kind() != checkpointmodel.ReceiptPartialDirectory ||
		receipt.SuccessCount() != 1 || receipt.FailureCount() != 1 {
		t.Fatalf("partial receipt = (kind=%d successes=%d failures=%d err=%v)",
			receipt.Kind(), receipt.SuccessCount(), receipt.FailureCount(), err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "success.bin")); err != nil || string(content) != "good" {
		t.Fatalf("successful prefix = (%q, %v)", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "collision.bin")); err != nil || string(content) != "keep" {
		t.Fatalf("collision target = (%q, %v)", content, err)
	}
}

func TestNativeResumeRepositoryProjectsUnknownNamespaceAndDirectoryOwnership(t *testing.T) {
	t.Run("foreign checkpoint namespace remains read-only", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		intent := nativeTestIntent(t, root, 0xb1, 0xb2)
		markerPath := filepath.Join(
			root, checkpointstore.ControlDirectory, checkpointstore.CheckpointDirectory,
			checkpointstore.OwnershipDirectory, checkpointstore.OwnershipFile,
		)
		foreign, err := os.ReadFile(markerPath)
		if err != nil {
			t.Fatal(err)
		}
		foreign[0] ^= 0xff
		if err := os.WriteFile(markerPath, foreign, 0o600); err != nil {
			t.Fatal(err)
		}

		repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
		if err != nil {
			t.Fatal(err)
		}
		snapshots, err := repository.List(context.Background())
		if err != nil || len(snapshots) != 1 || len(snapshots[0].LifecycleRecord) != 1 ||
			snapshots[0].LifecycleRecord[0] != 0 {
			t.Fatalf("foreign list = (%+v, %v)", snapshots, err)
		}
		operation, err := checkpointmodel.DecodeReceiveOperation(snapshots[0].OperationRecord)
		if err != nil || operation.OperationID() != intent.OperationID() {
			t.Fatalf("foreign operation identity = (%v, %v)", operation.OperationID(), err)
		}
		after, err := os.ReadFile(markerPath)
		if err != nil || string(after) != string(foreign) {
			t.Fatalf("foreign marker was mutated: %v", err)
		}
		if _, err := repository.Acquire(context.Background(), intent.OperationID()); err == nil {
			t.Fatal("foreign namespace unexpectedly yielded mutation authority")
		}
	})

	t.Run("replaced admitted directory stops cleanup", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		intent := nativeTestIntent(t, root, 0xc1, 0xc2)
		session := openNativeCompositionSession(t, root, false, intent, nil)
		rootAdmission := admitNativeRoot(t, session, intent, 0xc3, catalog.ModifiedTime{})
		_ = admitNativeResumeDirectory(t, session, rootAdmission, "owned", 0xc4, 0xc5)
		if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
			t.Fatal(err)
		}
		ownedPath := filepath.Join(root, "owned")
		if err := os.Remove(ownedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(ownedPath, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignPath := filepath.Join(ownedPath, "foreign.txt")
		if err := os.WriteFile(foreignPath, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}

		repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := repository.Acquire(context.Background(), intent.OperationID())
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		evidence, err := lease.ObserveRecovery(context.Background())
		if err != nil || evidence.TargetOwnership != NativeResumeEvidenceUnknown ||
			evidence.Checkpoints != NativeResumeEvidenceUnknown || evidence.Cleanup != NativeResumeCleanupUnknown {
			t.Fatalf("replacement evidence = (%+v, %v)", evidence, err)
		}
		cleanup, err := lease.CleanupOwned(context.Background())
		if err != nil || cleanup.State != NativeResumeCleanupUnknown || len(cleanup.Receipt) != 0 {
			t.Fatalf("replacement cleanup = (%+v, %v)", cleanup, err)
		}
		if content, err := os.ReadFile(foreignPath); err != nil || string(content) != "preserve" {
			t.Fatalf("foreign replacement was mutated = (%q, %v)", content, err)
		}
	})
}

func admitNativeResumeDirectory(
	t *testing.T,
	session transfer.DirectTreeSession,
	parent transfer.DirectoryAdmission,
	path string,
	directoryID byte,
	generation byte,
) transfer.DirectoryAdmission {
	t.Helper()
	admission, err := session.AdmitDirectory(context.Background(), transfer.MaterializationDirectory{
		DirectoryID:     incrementalTestIdentity16[catalog.DirectoryID](directoryID),
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](generation),
		ParentAdmission: parent, Path: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return admission
}
