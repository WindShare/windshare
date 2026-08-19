package checkpointstore

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func (file *memoryFile) SetModifiedTime(catalog.ModifiedTime) error {
	if file == nil || file.data == nil {
		return outputcap.ErrUnsafeNamespace
	}
	return nil
}

func (file *memoryFile) MetadataMatches(exactSize uint64, _ catalog.ModifiedTime) (bool, error) {
	if file == nil || file.data == nil {
		return false, outputcap.ErrUnsafeNamespace
	}
	size, err := file.Size()
	return size == exactSize, err
}

func TestFileExecutionStoreOwnsCheckpointAndObjectLifecycle(t *testing.T) {
	root, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x81)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	if store.RecordCount() != 0 {
		t.Fatal("empty repository was indexed as non-empty")
	}
	record := checkpointRecordFixture(t, ownership, intent, 0x83)
	object := record.OwnedObjectID()

	owned, observation, err := store.CreateOwnedFile(context.Background(), nil, object, record.ExactSize())
	if err != nil || owned == nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("create owned file = (%T, %d, %v)", owned, observation.Condition(), err)
	}
	if count, err := owned.WriteAt([]byte("windshare"), 0); err != nil || count != len("windshare") {
		t.Fatalf("write owned file = (%d, %v)", count, err)
	}
	if err := owned.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := owned.SetModifiedTime(catalog.ModifiedTime{}); err != nil {
		t.Fatal(err)
	}
	if matches, err := owned.MetadataMatches(record.ExactSize(), catalog.ModifiedTime{}); err != nil || !matches {
		t.Fatalf("owned metadata = (%t, %v)", matches, err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := owned.WriteAt(nil, 0); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed owned write = %v", err)
	}
	if err := owned.Sync(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed owned sync = %v", err)
	}
	if _, err := owned.MetadataMatches(record.ExactSize(), catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed owned metadata = %v", err)
	}

	opened, observation, err := store.OpenOwnedFile(context.Background(), object, record.ExactSize(), true)
	if err != nil || opened == nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("open owned file = (%T, %d, %v)", opened, observation.Condition(), err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	duplicate, collision, err := store.CreateOwnedFile(context.Background(), nil, object, record.ExactSize())
	if err != nil || duplicate != nil || collision.Condition() != fileexecution.OwnedObjectCollision {
		t.Fatalf("duplicate owned file = (%T, %d, %v)", duplicate, collision.Condition(), err)
	}

	initial, err := store.installInitialRecord(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := initial.Resolution()
	if observed, ok := checkpoint.Record(); !ok || observed.RecordID() != record.RecordID() {
		t.Fatalf("created checkpoint observation = (%v, %t)", observed.RecordID(), ok)
	}
	if store.RecordCount() != 1 {
		t.Fatalf("record count = %d, want 1", store.RecordCount())
	}

	// Reconstructing the adapter exercises startup reconciliation. The initial
	// candidate may be promoted only because its exact stage/anchor pair is ready.
	reconciled, err := NewFileExecutionStore(&repository)
	if err != nil || reconciled.RecordCount() != 1 {
		t.Fatalf("reconciled store = (%d, %v)", reconciled.RecordCount(), err)
	}
	promoted, err := repository.Reopen(record.RecordID())
	if err != nil {
		t.Fatal(err)
	}
	checkpointCandidate, err := checkpointmodel.AdvanceGeneration(
		promoted, []checkpointmodel.Range{{Offset: 0, End: uint64(len("windshare"))}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace(promoted, checkpointCandidate); err != nil {
		t.Fatal(err)
	}
	// The candidate is a process-restart cut, so release every process-scoped
	// handle before proving that a fresh authority can adopt it.
	if err := errors.Join(repository.Close(), lease.Close(), namespace.Close()); err != nil {
		t.Fatal(err)
	}
	namespace, err = OpenOperationRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	lease, err = namespace.AcquireOperationLease(intent.intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(
		ownership, intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = OpenOrdinaryFileRepository(lease, binding, false)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err = NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatalf("range candidate crash recovery = %v", err)
	}
	recovered, err := repository.Reopen(record.RecordID())
	recoveredRanges := recovered.VerifiedRanges()
	if err != nil || recovered.CommitState() != checkpointmodel.CommitVerified ||
		recovered.CheckpointGeneration() != checkpointCandidate.CheckpointGeneration() ||
		len(recoveredRanges) != 1 || recoveredRanges[0].Offset != 0 ||
		recoveredRanges[0].End != uint64(len("windshare")) {
		t.Fatalf("recovered range candidate = (%+v, %v)", recovered, err)
	}

	final, err := reconciled.PublishOwnedNoReplace(
		context.Background(), object, record.ExactSize(), root, "published.bin",
	)
	if err != nil || final == nil {
		t.Fatalf("publish owned = (%T, %v)", final, err)
	}
	if matched, err := reconciled.FinalMatchesOwned(
		context.Background(), object, record.ExactSize(), final,
	); err != nil || !matched {
		t.Fatalf("compare published owned file = (%t, %v)", matched, err)
	}
	if linked, err := reconciled.PublishOwnedNoReplace(
		context.Background(), object, record.ExactSize(), root, "published.bin",
	); err == nil || linked != nil {
		t.Fatalf("replacement publication = (%T, %v)", linked, err)
	}

	wantConditions := []struct {
		step fileexecution.RetirementStep
		want fileexecution.OwnedCondition
	}{
		{fileexecution.RetirementRemoveStage, fileexecution.OwnedStageMissing},
		{fileexecution.RetirementSyncStageNamespace, fileexecution.OwnedStageMissing},
		{fileexecution.RetirementRemoveAnchor, fileexecution.OwnedAbsent},
		{fileexecution.RetirementSyncAnchorNamespace, fileexecution.OwnedAbsent},
	}
	for _, expected := range wantConditions {
		observation, err := reconciled.ApplyRetirement(context.Background(), object, expected.step)
		if err != nil || observation.Condition() != expected.want {
			t.Fatalf("retirement step %d = (%d, %v), want %d", expected.step, observation.Condition(), err, expected.want)
		}
		if expected.step == fileexecution.RetirementSyncStageNamespace {
			matched, compareErr := reconciled.FinalMatchesOwned(
				context.Background(), object, record.ExactSize(), final,
			)
			if compareErr != nil || !matched {
				t.Fatalf("published witness after stage retirement = (%t, %v)", matched, compareErr)
			}
		}
	}
	if matched, err := reconciled.FinalMatchesOwned(
		context.Background(), object, record.ExactSize(), final,
	); err != nil || matched {
		t.Fatalf("comparison after anchor retirement = (%t, %v)", matched, err)
	}
	if err := final.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileExecutionStoreRejectsInvalidAndCanceledAuthority(t *testing.T) {
	if store, err := NewFileExecutionStore(nil); err == nil || store != nil {
		t.Fatalf("nil repository = (%T, %v)", store, err)
	}
	var nilStore *FileExecutionStore
	if nilStore.RecordCount() != 0 {
		t.Fatal("nil store reported records")
	}
	if _, err := nilStore.Lookup(context.Background(), fileexecution.CheckpointKey{}); err == nil {
		t.Fatalf("nil lookup = %v", err)
	}

	_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0xa1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	object, err := checkpointmodel.ObjectIDFromBytes(makeNonzeroBytes(0xa4))
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.CreateOwnedFile(canceled, nil, object, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create = %v", err)
	}
	if _, _, err := store.OpenOwnedFile(canceled, object, 1, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open = %v", err)
	}
	if _, err := store.ApplyRetirement(canceled, object, fileexecution.RetirementRemoveStage); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retirement = %v", err)
	}
	if _, err := store.FinalMatchesOwned(canceled, object, 1, &memoryFile{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled comparison = %v", err)
	}
	if _, err := store.PublishOwnedNoReplace(canceled, object, 1, newMemoryDirectory(), "final"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication = %v", err)
	}
	if _, err := store.ApplyRetirement(context.Background(), object, 0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid retirement = %v", err)
	}
}

func makeNonzeroBytes(fill byte) []byte {
	value := make([]byte, 32)
	for index := range value {
		value[index] = fill
	}
	return value
}
