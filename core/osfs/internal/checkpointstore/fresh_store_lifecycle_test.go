package checkpointstore

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/transfer"
)

func TestFreshFileExecutionStoreIndexesOnlyCurrentOperationAndCleansExactState(t *testing.T) {
	ctx := context.Background()
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xb1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	if records, attention := store.Snapshot(); len(records) != 0 || len(attention) != 0 {
		t.Fatalf("fresh snapshot = (%d records, %d attention)", len(records), len(attention))
	}
	record := checkpointRecordFixture(t, ownership, intent, 0xb3)
	object := record.OwnedObjectID()
	if observation, err := store.ObserveOwnedObject(ctx, object, record.ExactSize()); err != nil ||
		observation.Condition() != fileexecution.OwnedAbsent {
		t.Fatalf("initial owned observation = (%d, %v)", observation.Condition(), err)
	}
	owned, observation, err := store.CreateOwnedFile(ctx, nil, object, record.ExactSize())
	if err != nil || owned == nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("create owned = (%T, %d, %v)", owned, observation.Condition(), err)
	}
	if owned.ObjectID() != object {
		t.Fatal("owned file lost its authenticated object identity")
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Store(ctx, nil, record); err != nil {
		t.Fatal(err)
	}
	records, attention := store.Snapshot()
	if len(records) != 1 || records[0].RecordID() != record.RecordID() || len(attention) != 0 {
		t.Fatalf("current-operation snapshot = (%+v, %+v)", records, attention)
	}
	if err := store.Abandon(ctx, record); err != nil {
		t.Fatal(err)
	}
	if records, _ := store.Snapshot(); len(records) != 0 {
		t.Fatalf("abandoned checkpoint remained resumable: %+v", records)
	}
	if err := store.CleanupOwned(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Reopen(record.RecordID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cleanup retained checkpoint row: %v", err)
	}
	if observation, err := store.ObserveOwnedObject(ctx, object, record.ExactSize()); err != nil ||
		observation.Condition() != fileexecution.OwnedAbsent {
		t.Fatalf("cleanup retained owned object = (%d, %v)", observation.Condition(), err)
	}
}

func TestFreshFileExecutionStoreCleanupRejectsUncertainOrCanceledAuthority(t *testing.T) {
	if store, err := NewFreshFileExecutionStore(nil); store != nil ||
		!errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil fresh repository = (%T, %v)", store, err)
	}
	_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0xc1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFreshFileExecutionStoreWithProfile(&repository, checkpointmodel.LiveCleanupWindowsNTFSV1)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CleanupOwned(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup = %v", err)
	}
	store.attention = []Attention{{}}
	if err := store.CleanupOwned(context.Background()); ErrorCodeFor(err) != ErrorUnsafeInstall {
		t.Fatalf("uncertain cleanup = %v", err)
	}
	var nilStore *FileExecutionStore
	if records, attention := nilStore.Snapshot(); records != nil || attention != nil {
		t.Fatalf("nil snapshot = (%+v, %+v)", records, attention)
	}
	if err := nilStore.CleanupOwned(context.Background()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil cleanup = %v", err)
	}
}

func TestRecoveryArtifactLocationsRoundTripOnlyCanonicalOwnedObjects(t *testing.T) {
	object, err := checkpointmodel.ObjectIDFromBytes(makeNonzeroBytes(0xd1))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []RecoveryArtifactKind{RecoveryStage, RecoveryAnchor} {
		shard, name, err := RecoveryArtifactLocation(object, kind)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseRecoveryArtifactLocation(shard, name, kind)
		if err != nil || parsed != object {
			t.Fatalf("round trip kind %d = (%x, %v)", kind, parsed.Bytes(), err)
		}
		if _, err := ParseRecoveryArtifactLocation(shard, strings.ToUpper(name), kind); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("uppercase recovery name kind %d = %v", kind, err)
		}
		if _, err := ParseRecoveryArtifactLocation("ff", name, kind); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("substituted shard kind %d = %v", kind, err)
		}
	}
	if _, _, err := RecoveryArtifactLocation(checkpointmodel.ObjectID{}, RecoveryStage); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("zero object location = %v", err)
	}
	if _, _, err := RecoveryArtifactLocation(object, 0); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("invalid recovery kind = %v", err)
	}
	if _, err := ParseRecoveryArtifactLocation("", "", 0); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("invalid recovery parse = %v", err)
	}
}
