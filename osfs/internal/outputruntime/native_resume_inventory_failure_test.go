package outputruntime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/resumeauthority"
)

func TestNativeResumeInventoryFailurePreservesActiveOperationAndProgress(t *testing.T) {
	for _, prepare := range []struct {
		name string
		open func(*testing.T, *NativeResumeLease, transfer.ReceiveIntent)
	}{
		{name: "initial inventory"},
		{name: "lazy inventory", open: openUninventoriedResumeStore},
	} {
		t.Run(prepare.name, func(t *testing.T) {
			ctx := context.Background()
			root := newRuntimeTestRootSpec(t).path
			fixture := openOrdinaryResumeSession(t, root, 0x61, 4)
			savedContent := []byte("ab")
			if err := fixture.transaction.WriteRange(ctx, 0, savedContent); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.transaction.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			pauseOrdinaryResumeFixture(t, fixture)
			repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
			if err != nil {
				t.Fatal(err)
			}
			acquired, err := repository.Acquire(ctx, fixture.intent.OperationID())
			if err != nil {
				t.Fatal(err)
			}
			lease := acquired.(*NativeResumeLease)
			t.Cleanup(func() { _ = lease.Close() })
			if prepare.open != nil {
				prepare.open(t, lease, fixture.intent)
			}
			failure := errors.New("checkpoint directory temporarily unreadable")
			restore := failResumeCheckpointEnumeration(t, lease, failure)
			before := lease.operation.Record()
			snapshot, err := lease.Snapshot(ctx)
			if !errors.Is(err, failure) || checkpointstore.ErrorCodeFor(err) != checkpointstore.ErrorStateIO ||
				snapshot.Valid() {
				t.Fatalf("failed inventory = (valid=%t, %v), want original I/O failure", snapshot.Valid(), err)
			}
			after := lease.operation.Record()
			if after.Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
				after.LifecycleGeneration() != before.LifecycleGeneration() ||
				after.ClosedReason() != checkpointmodel.OrdinaryReasonNone {
				t.Fatalf("failed inventory changed operation: lifecycle=%s generation=%d reason=%s",
					after.Lifecycle(), after.LifecycleGeneration(), after.ClosedReason())
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			restore()

			reacquired, err := repository.Acquire(ctx, fixture.intent.OperationID())
			if err != nil {
				t.Fatal(err)
			}
			restarted := reacquired.(*NativeResumeLease)
			t.Cleanup(func() { _ = restarted.Close() })
			snapshot, err = restarted.Snapshot(ctx)
			if err != nil || !snapshot.Valid() {
				t.Fatalf("retry inventory = (%+v, %v)", snapshot, err)
			}
			items := snapshot.Items()
			if snapshot.Header().Record().Lifecycle() != checkpointmodel.OrdinaryOperationActive ||
				len(items) != 1 || items[0].State() != resumeauthority.ItemResumable {
				t.Fatalf("retry lost resumable operation: header=%+v items=%+v", snapshot.Header(), items)
			}
			slots, attention, err := restarted.store.Inventory()
			if err != nil || len(attention) != 0 || len(slots) != 1 {
				t.Fatalf("retry checkpoint inventory = (%+v, %+v, %v)", slots, attention, err)
			}
			record, exact := slots[0].Record()
			want := []checkpointmodel.Range{{Offset: 0, End: uint64(len(savedContent))}}
			if !exact || !slices.Equal(record.VerifiedRanges(), want) {
				t.Fatalf("retry changed saved ranges: exact=%t ranges=%+v", exact, record.VerifiedRanges())
			}
		})
	}
}

// The consumer must preserve errors even when no earlier eager inventory has
// succeeded. Opening a lazy store exercises that contract independently of setup.
func openUninventoriedResumeStore(t *testing.T, lease *NativeResumeLease, intent transfer.ReceiveIntent) {
	t.Helper()
	disposition, err := ordinaryResumeRootDisposition(intent)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := lease.runtime.destination.FileCheckpointOwnership(disposition)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(ownership, intent.OperationID(), intent.Digest(), intent.BindingDigest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := checkpointstore.OpenOrdinaryFileRepository(lease.operation, binding, false)
	if err != nil {
		t.Fatal(err)
	}
	lease.repository = &repository
	lease.store, err = checkpointstore.NewFileExecutionStoreWithStagePolicy(&repository, checkpointstore.InheritFinalParent)
	if err != nil {
		t.Fatal(err)
	}
}

func failResumeCheckpointEnumeration(t *testing.T, lease *NativeResumeLease, cause error) func() {
	t.Helper()
	files, err := lease.operation.OpenFileState(false)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	checkpoints, err := files.OpenDirectory(checkpointstore.CheckpointsDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer checkpoints.Close()
	records, err := checkpoints.OpenDirectory(checkpointstore.RecordsDirectory, true)
	if err != nil {
		t.Fatal(err)
	}
	defer records.Close()
	directory := records.(*portableRuntimeDirectory)
	path, err := directory.currentPath()
	if err != nil {
		t.Fatal(err)
	}
	filesystem := directory.filesystem
	filesystem.mu.Lock()
	if filesystem.enumerationErrors == nil {
		filesystem.enumerationErrors = make(map[string]error)
	}
	filesystem.enumerationErrors[path] = cause
	filesystem.mu.Unlock()
	restore := func() {
		filesystem.mu.Lock()
		delete(filesystem.enumerationErrors, path)
		filesystem.mu.Unlock()
	}
	t.Cleanup(restore)
	return restore
}
