package checkpointstore

import (
	"context"
	"errors"
	"testing"

	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestFailedReplacementFreezesOtherLineagesWithCheckpointFault(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0x91)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)
	candidate := replacementRecordFixture(t, operation.intent, ownership, file, objectIDFixture(t, 0x92))
	previous := checkpointRecordFixture(t, ownership, operation, 0x93)
	if previous.RecordID() == candidate.RecordID() {
		t.Fatal("failure fixture must use a different lineage")
	}
	if _, err := repository.Create(previous); err != nil {
		t.Fatal(err)
	}
	next, err := checkpointmodel.PromoteInitialCandidate(previous)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("replacement directory sync failed")
	repository.records = observeCleanupBarriers(repository.records, func() error { return injected })
	if _, err := store.Replace(context.Background(), previous, next); !errors.Is(err, injected) {
		t.Fatalf("replacement did not reach injected failure: %v", err)
	}
	spec, err := key.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"lookup": func() error { _, err := store.Lookup(context.Background(), key); return err },
		"delivery receipt": func() error {
			_, _, err := store.LookupDeliveredRecord(context.Background(), spec)
			return err
		},
		"initial installation": func() error {
			_, err := store.InstallInitial(context.Background(), key, candidate)
			return err
		},
		"replacement retry": func() error {
			_, err := store.Replace(context.Background(), previous, next)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := operation()
			assertCheckpointBoundary(t, err, transferfault.CheckpointStateIO)
			if !errors.Is(err, errFileExecutionAuthorityUnsettled) {
				t.Fatalf("lost unsettled authority diagnostic: %v", err)
			}
		})
	}
}

func TestCheckpointLookupSeparatesUnreadableAndCorruptRecords(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0x94)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)
	candidate := replacementRecordFixture(t, operation.intent, ownership, file, objectIDFixture(t, 0x95))
	if _, err := repository.Create(candidate); err != nil {
		t.Fatal(err)
	}
	records := repository.records
	injected := errors.New("record shard temporarily unreadable")
	repository.records = &faultDirectory{
		Directory:     records,
		openDirectory: func(string, bool) (outputcap.Directory, error) { return nil, injected },
	}
	if _, err := store.Lookup(context.Background(), key); !errors.Is(err, injected) {
		t.Fatalf("lookup lost read failure: %v", err)
	} else {
		assertCheckpointBoundary(t, err, transferfault.CheckpointStateIO)
	}
	repository.records = records

	shardName, name := recordLocation(candidate.RecordID())
	shard, err := records.OpenDirectory(shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	image, err := shard.OpenMutableFile(name, true)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := image.WriteAt([]byte("corrupt"), 0)
	if err := errors.Join(writeErr, image.Close()); err != nil {
		t.Fatal(err)
	}
	assertInvalidCheckpointResolution(t, store, key)

	// Corruption in an in-memory lineage slot must have the same file-local
	// meaning as a corrupt disk image, without becoming installation authority.
	store.authority.physical[candidate.RecordID()] = checkpointRecordFixture(t, ownership, operation, 0x96)
	assertInvalidCheckpointResolution(t, store, key)
	spec, err := key.CheckpointLineageSpec()
	if err != nil {
		t.Fatal(err)
	}
	delete(store.authority.physical, candidate.RecordID())
	if _, found, err := store.LookupDeliveredRecord(context.Background(), spec); err != nil || found {
		t.Fatalf("corrupt record became a delivery receipt: found=%t err=%v", found, err)
	}
}

func TestCheckpointLookupCapacityDoesNotInvalidateStoredProgress(t *testing.T) {
	_, registry, lease, repository, ownership, operation := openRepositoryFixture(t, 0x97)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	file, session, destination := replacementMaterializationFixture(t, operation.intent)
	key := captureCheckpointKey(t, operation.intent, ownership, session, file, destination, store)
	candidate := replacementRecordFixture(t, operation.intent, ownership, file, objectIDFixture(t, 0x98))
	if _, err := repository.Create(candidate); err != nil {
		t.Fatal(err)
	}
	store.authority.recordLimit = 1
	if err := store.authority.add(checkpointRecordFixture(t, ownership, operation, 0x99)); err != nil {
		t.Fatal(err)
	}
	_, err = store.Lookup(context.Background(), key)
	value, valid := transferfault.NormalizeBoundaryError(err).Fault()
	code, output := value.OutputCode()
	if !errors.Is(err, fileexecution.ErrCheckpointRecordCapacity) ||
		!valid || !output || code != transferfault.OutputResourceBudget || value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("exhausted checkpoint cache = %s from %v, want output-pause/resource-budget", value, err)
	}
	// Once capacity is available, the same checkpoint remains resumable.
	if err := store.authority.rebuild(nil, nil); err != nil {
		t.Fatal(err)
	}
	resolution, err := store.Lookup(context.Background(), key)
	if err != nil || resolution.Decision() != checkpointmodel.CheckpointLineageDecisionExact {
		t.Fatalf("capacity failure discarded valid progress: %s, %v", resolution.Decision(), err)
	}
}

func assertInvalidCheckpointResolution(t *testing.T, store *FileExecutionStore, key fileexecution.CheckpointKey) {
	t.Helper()
	resolution, err := store.Lookup(context.Background(), key)
	if err != nil || resolution.Decision() != checkpointmodel.CheckpointLineageDecisionInvalid {
		t.Fatalf("corrupt lookup = (%s, %v), want a file-local invalid decision", resolution.Decision(), err)
	}
}

func assertCheckpointBoundary(t *testing.T, err error, expected transferfault.CheckpointCode) {
	t.Helper()
	value, valid := transferfault.NormalizeBoundaryError(err).Fault()
	code, checkpoint := value.CheckpointCode()
	if !valid || !checkpoint || code != expected || value.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("checkpoint boundary = %s from %v, want output-pause/%s", value, err, expected)
	}
}
