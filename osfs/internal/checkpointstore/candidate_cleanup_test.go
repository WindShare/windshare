package checkpointstore

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func retainCheckpointCandidate(t *testing.T, repository *Repository, record checkpointmodel.Record) string {
	t.Helper()
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	shardName, target := recordLocation(record.RecordID())
	shard, err := OpenShard(repository.records, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	file, name, err := repository.installer(shard).prepareCandidate(target, encoded, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func reopenCandidateStore(t *testing.T, repository *Repository) *FileExecutionStore {
	t.Helper()
	// A restarted operation has a new lease authority and the same durable files.
	repository.authority = newFileExecutionAuthority()
	store, err := NewFileExecutionStore(repository)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func requireCandidateCleanup(t *testing.T, store *FileExecutionStore, candidate checkpointmodel.Record, name string) {
	t.Helper()
	if err := store.CleanupOwned(context.Background()); err != nil {
		t.Fatal(err)
	}
	shardName, target := recordLocation(candidate.RecordID())
	shard, err := OpenShard(store.repository.records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	for _, name := range []string{name, target} {
		if _, err := ReadFile(shard, name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("checkpoint image remains at %s: %v", name, err)
		}
	}
	for _, entry := range []struct {
		root   outputcap.Directory
		suffix string
	}{{store.repository.stages, ownedStageSuffix}, {store.repository.anchors, ownedAnchorSuffix}} {
		shardName, name := ownedObjectLocation(candidate.OwnedObjectID(), entry.suffix)
		shard, err := OpenShard(entry.root, shardName, false)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		kind, exact, err := shard.ClassifyExactEntry(name)
		_ = shard.Close()
		if err != nil || !exact || kind != outputcap.EntryAbsent {
			t.Fatalf("owned data remains after cleanup: kind=%v exact=%t error=%v", kind, exact, err)
		}
	}
	if err := reopenCandidateStore(t, store.repository).CleanupOwned(context.Background()); err != nil {
		t.Fatalf("cleanup did not converge across reopen: %v", err)
	}
}

func TestLazyResumeReclaimsSupersededCheckpointCandidates(t *testing.T) {
	for _, inventoryFirst := range []bool{false, true} {
		name := "terminal cleanup"
		if inventoryFirst {
			name = "management inventory"
		}
		t.Run(name, func(t *testing.T) {
			store, repository, initial := candidateReconciliationFixture(t)
			previous, err := checkpointmodel.PromoteInitialCandidate(initial)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Replace(context.Background(), initial, previous); err != nil {
				t.Fatal(err)
			}
			candidate, err := checkpointmodel.AdvanceDurableRanges(previous, []checkpointmodel.Range{{Offset: 0, End: 8}})
			if err != nil {
				t.Fatal(err)
			}
			temporaryName := retainCheckpointCandidate(t, repository, candidate)
			root := repository.records
			repository.records = &faultDirectory{Directory: root, names: func(int) ([]string, error) {
				return nil, errors.New("ordinary resume enumerated the repository")
			}}
			store = reopenCandidateStore(t, repository)
			for _, end := range []uint64{16, 32} {
				next, err := checkpointmodel.AdvanceDurableRanges(previous, []checkpointmodel.Range{{Offset: 0, End: end}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.Replace(context.Background(), previous, next); err != nil {
					t.Fatal(err)
				}
				previous = next
			}
			repository.records = root
			if inventoryFirst {
				slots, attention, err := store.Inventory()
				if err != nil || len(slots) != 1 || len(attention) != 0 {
					t.Fatalf("resumed inventory: slots=%d attention=%v error=%v", len(slots), attention, err)
				}
				current, found := slots[0].Record()
				if !found || current.CheckpointGeneration() != previous.CheckpointGeneration() {
					t.Fatal("obsolete candidate changed the installed progress")
				}
				if store.ReclaimedCandidateCount() != 1 {
					t.Fatalf("reclaimed candidates=%d, want 1", store.ReclaimedCandidateCount())
				}
			}
			requireCandidateCleanup(t, store, candidate, temporaryName)
		})
	}
}

func TestCleanupRetiresCandidateWithoutGrantingRecoveryAuthority(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "conflicting successor"
		if orphan {
			name = "orphan after record deletion"
		}
		t.Run(name, func(t *testing.T) {
			store, repository, initial := candidateReconciliationFixture(t)
			previous, err := checkpointmodel.PromoteInitialCandidate(initial)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Replace(context.Background(), initial, previous); err != nil {
				t.Fatal(err)
			}
			candidate := previous
			for _, end := range []uint64{8, 16} {
				candidate, err = checkpointmodel.AdvanceDurableRanges(candidate, []checkpointmodel.Range{{Offset: 0, End: end}})
				if err != nil {
					t.Fatal(err)
				}
			}
			temporaryName := retainCheckpointCandidate(t, repository, candidate)
			wantAttention := AttentionConflictingCandidate
			wantRecords := 1
			if orphan {
				if err := repository.Remove(previous); err != nil {
					t.Fatal(err)
				}
				wantAttention, wantRecords = AttentionOrphanedCandidate, 0
			}
			store = reopenCandidateStore(t, repository)
			slots, attention, err := store.Inventory()
			if err != nil || len(slots) != wantRecords || len(attention) != 1 ||
				attention[0].Code() != wantAttention || !RecoveryBlocked(attention) {
				t.Fatalf("candidate acquired recovery authority: slots=%d attention=%v error=%v", len(slots), attention, err)
			}
			requireCandidateCleanup(t, store, candidate, temporaryName)
		})
	}
}

func TestCandidateCleanupRetriesAfterRemovalFailure(t *testing.T) {
	store, repository, initial := candidateReconciliationFixture(t)
	previous, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replace(context.Background(), initial, previous); err != nil {
		t.Fatal(err)
	}
	candidate, err := checkpointmodel.AdvanceDurableRanges(previous, []checkpointmodel.Range{{Offset: 0, End: 8}})
	if err != nil {
		t.Fatal(err)
	}
	temporaryName := retainCheckpointCandidate(t, repository, candidate)
	if err := repository.Remove(previous); err != nil {
		t.Fatal(err)
	}
	store = reopenCandidateStore(t, repository)
	if _, _, err := store.Inventory(); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("candidate deletion interrupted")
	root := repository.records
	repository.records = &faultDirectory{Directory: root, openDirectory: func(name string, private bool) (outputcap.Directory, error) {
		shard, err := root.OpenDirectory(name, private)
		if err != nil {
			return nil, err
		}
		return candidateRemoveFailure{Directory: shard, failure: injected}, nil
	}}
	if err := store.CleanupOwned(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("cleanup failure=%v, want injected removal failure", err)
	}
	if err := store.CleanupOwned(context.Background()); !errors.Is(err, errFileExecutionAuthorityUnsettled) {
		t.Fatalf("failed removal did not freeze authority: %v", err)
	}
	repository.records = root
	requireCandidateCleanup(t, reopenCandidateStore(t, repository), candidate, temporaryName)
}

func TestCandidateCleanupPreservesUnrelatedAttention(t *testing.T) {
	_, repository, initial := candidateReconciliationFixture(t)
	previous, err := checkpointmodel.PromoteInitialCandidate(initial)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := checkpointmodel.AdvanceDurableRanges(previous, []checkpointmodel.Range{{Offset: 0, End: 8}})
	if err != nil {
		t.Fatal(err)
	}
	ownedName := retainCheckpointCandidate(t, repository, candidate)
	if err := repository.Remove(initial); err != nil {
		t.Fatal(err)
	}
	shardName, targetName := recordLocation(initial.RecordID())
	shard, err := OpenShard(repository.records, shardName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	foreignName := TemporaryName(targetName, []byte("foreign"), 0)
	writeInterruptedCandidate(t, shard, foreignName, []byte("foreign"))
	store := reopenCandidateStore(t, repository)
	if err := store.CleanupOwned(context.Background()); ErrorCodeFor(err) != ErrorUnsafeInstall {
		t.Fatalf("unowned candidate lost cleanup attention: %v", err)
	}
	if _, err := ReadFile(shard, ownedName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("owned candidate survived cleanup: %v", err)
	}
	if image, err := ReadFile(shard, foreignName); err != nil || string(image) != "foreign" {
		t.Fatalf("unowned candidate was changed: %q, %v", image, err)
	}
	_, attention, err := store.Inventory()
	if err != nil || len(attention) != 1 || attention[0].Code() != AttentionInvalidCandidate {
		t.Fatalf("remaining attention=%v, error=%v", attention, err)
	}
}
