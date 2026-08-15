package checkpointstore

import (
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
)

func TestPrivateStateRecyclerRemovesEmptyInfrastructureAndStableLockCarriers(t *testing.T) {
	control, registry, journal := recyclerFixture(t)
	activeName := strings.Repeat("1", 64)
	activeChild, err := registry.active.CreateDirectory(activeName, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := activeChild.Close(); err != nil {
		t.Fatal(err)
	}
	lock, _, err := registry.leases.AcquireLock(strings.Repeat("2", 32)+ordinaryOperationLockSuffix, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	empty, err := RecyclePrivateState(control)
	if err != nil || !empty {
		t.Fatalf("recycle empty state = (%t, %v)", empty, err)
	}
	if len(control.dirs) != 0 || len(control.files) != 0 {
		t.Fatalf("recycled control entries = dirs %v files %v", control.dirs, control.files)
	}
}

func TestPrivateStateRecyclerPreservesRecoveryUnknownAndBusyState(t *testing.T) {
	t.Run("operation record", func(t *testing.T) {
		control, registry, journal := recyclerFixture(t)
		operation, err := registry.operations.CreateDirectory(strings.Repeat("3", 32), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := operation.CreateFile(ordinaryOperationRecordFile, true, 1); err != nil {
			t.Fatal(err)
		}
		closeRecyclerFixture(t, &registry, &journal)
		if empty, err := RecyclePrivateState(control); err != nil || empty {
			t.Fatalf("operation-bearing recycle = (%t, %v)", empty, err)
		}
	})

	t.Run("cleanup stage", func(t *testing.T) {
		control, registry, journal := recyclerFixture(t)
		proof := control.dirs[checkpointmodel.LiveCleanupNamespaceV1]
		if _, err := proof.CreateFile("stage-unknown", true, 0); err != nil {
			t.Fatal(err)
		}
		closeRecyclerFixture(t, &registry, &journal)
		if empty, err := RecyclePrivateState(control); err != nil || empty {
			t.Fatalf("stage-bearing recycle = (%t, %v)", empty, err)
		}
	})

	t.Run("live stable lock", func(t *testing.T) {
		control, registry, journal := recyclerFixture(t)
		lock, _, err := registry.leases.AcquireLock(strings.Repeat("4", 64)+ordinaryActiveLockSuffix, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		if empty, err := RecyclePrivateState(control); err != nil || empty {
			t.Fatalf("busy-lock recycle = (%t, %v)", empty, err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func recyclerFixture(t *testing.T) (*memoryDirectory, OperationRegistry, LiveCleanupJournal) {
	t.Helper()
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := OpenLiveCleanupJournal(control)
	if err != nil {
		t.Fatal(err)
	}
	return control, registry, journal
}

func closeRecyclerFixture(t *testing.T, registry *OperationRegistry, journal *LiveCleanupJournal) {
	t.Helper()
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
}
