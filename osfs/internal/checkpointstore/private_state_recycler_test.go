package checkpointstore

import (
	"strings"
	"testing"
)

func TestPrivateStateRecyclerRemovesEmptyInfrastructureAndStableLockCarriers(t *testing.T) {
	control, registry := recyclerFixture(t)
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
		control, registry := recyclerFixture(t)
		operation, err := registry.operations.CreateDirectory(strings.Repeat("3", 32), true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := operation.CreateFile(ordinaryOperationRecordFile, true, 1); err != nil {
			t.Fatal(err)
		}
		closeRecyclerFixture(t, &registry)
		if empty, err := RecyclePrivateState(control); err != nil || empty {
			t.Fatalf("operation-bearing recycle = (%t, %v)", empty, err)
		}
	})

	t.Run("live stable lock", func(t *testing.T) {
		control, registry := recyclerFixture(t)
		lock, _, err := registry.leases.AcquireLock(strings.Repeat("4", 64)+ordinaryActiveLockSuffix, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil {
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

func recyclerFixture(t *testing.T) (*memoryDirectory, OperationRegistry) {
	t.Helper()
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	return control, registry
}

func closeRecyclerFixture(t *testing.T, registry *OperationRegistry) {
	t.Helper()
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}
