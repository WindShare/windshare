package checkpointstore

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestOrdinaryRepositoryRejectsPredecessorSubstitutionAndRetriesExactCuts(t *testing.T) {
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xe1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	initial := checkpointRecordFixture(t, ownership, intent, 0xe3)
	if err := repository.Create(initial); err != nil {
		t.Fatal(err)
	}
	active, err := checkpointmodel.Promote(
		initial,
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(active); ErrorCodeFor(err) != ErrorUnsafeInstall {
		t.Fatalf("noninitial create without predecessor = %v", err)
	}
	if err := repository.Replace(initial, active); err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(active); err != nil {
		t.Fatalf("exact completed create retry = %v", err)
	}

	foreign := checkpointRecordFixture(t, ownership, intent, 0xe4)
	if err := repository.Replace(active, foreign); ErrorCodeFor(err) != ErrorUnsafeInstall {
		t.Fatalf("foreign record replacement = %v", err)
	}
	if err := repository.Replace(active, initial); ErrorCodeFor(err) != ErrorUnsafeInstall {
		t.Fatalf("generation rollback = %v", err)
	}
	if err := repository.Remove(active); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Reopen(active.RecordID()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed record reopened = %v", err)
	}
	if err := repository.Remove(active); err == nil {
		t.Fatal("second exact removal succeeded without an owned row")
	}

	var nilRepository *Repository
	if err := nilRepository.Create(initial); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil create error = %v", err)
	}
	if err := nilRepository.Replace(initial, active); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil replace error = %v", err)
	}
	if err := nilRepository.Remove(initial); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil remove error = %v", err)
	}
	if _, err := nilRepository.Reopen(initial.RecordID()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reopen error = %v", err)
	}
}

func TestRemoveEmptyShardsDeletesOnlyAuthenticatedEmptyDirectories(t *testing.T) {
	root := newMemoryDirectory()
	for _, name := range []string{"00", "01"} {
		shard, err := OpenShard(root, name, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := shard.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeEmptyShards(root); err != nil {
		t.Fatal(err)
	}
	if names, err := root.Names(ShardLimit); err != nil || len(names) != 0 {
		t.Fatalf("empty shards remained = (%v, %v)", names, err)
	}
	if err := removeEmptyDirectory(nil, "00", root); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil parent cleanup error = %v", err)
	}

	root = newMemoryDirectory()
	root.dirs["foreign"] = newMemoryDirectory()
	if err := removeEmptyShards(root); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("foreign shard cleanup error = %v", err)
	}
	root = newMemoryDirectory()
	shard, err := OpenShard(root, "02", true)
	if err != nil {
		t.Fatal(err)
	}
	writeMemoryFile(t, shard, "foreign", []byte("preserve"))
	if err := shard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeEmptyShards(root); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nonempty shard cleanup error = %v", err)
	}
}
