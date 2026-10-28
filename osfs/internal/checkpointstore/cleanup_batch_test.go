package checkpointstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"

	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func cleanupBatchFixture(t *testing.T) (*FileExecutionStore, []checkpointmodel.Record) {
	t.Helper()
	_, registry, lease, repository, ownership, intent := openRepositoryFixture(t, 0xa1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	store, err := NewFreshFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	var records []checkpointmodel.Record
	for index := range 2 {
		seed := checkpointRecordFixture(t, ownership, intent, byte(0xb1+index))
		object := seed.OwnedObjectID().Bytes()
		object[0] = 0xc1 // Distinct objects share one directory durability barrier.
		record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
			OperationID: seed.OperationID(), ReceiveIntentDigest: seed.ReceiveIntentDigest(),
			MaterializationBindingDigest: seed.MaterializationBindingDigest(),
			FileID:                       seed.FileID(), FileRevision: seed.FileRevision(),
			CanonicalPath: fmt.Sprintf("folder/file-%d.bin", index), ExactSize: seed.ExactSize(),
			MaterializerKind: seed.MaterializerKind(), AuthorityRef: seed.AuthorityRef().Bytes(),
			OwnedObjectID: object, StateGeneration: 1,
			Phase: checkpointmodel.PhaseActive, CommitState: checkpointmodel.CommitCandidate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.installInitialRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		file, _, err := store.CreateOwnedFile(context.Background(), nil, record.OwnedObjectID(), record.ExactSize())
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return store, records
}

func observeCleanupBarriers(root outputcap.Directory, observe func() error) outputcap.Directory {
	return &faultDirectory{Directory: root, openDirectory: func(name string, private bool) (outputcap.Directory, error) {
		directory, err := root.OpenDirectory(name, private)
		if err != nil {
			return nil, err
		}
		return &faultDirectory{Directory: directory, sync: observe}, nil
	}}
}

func TestCleanupBatchPreservesRecoveryBarriers(t *testing.T) {
	for _, failedPhase := range []string{"", "stages", "anchors", "records"} {
		t.Run("failure="+failedPhase, func(t *testing.T) {
			store, records := cleanupBatchFixture(t)
			injected := errors.New("cleanup durability failed")
			var barriers []string
			barrierCounts := make(map[string]int)
			fail := true
			wrap := func(root outputcap.Directory, phase string) outputcap.Directory {
				return observeCleanupBarriers(root, func() error {
					barriers = append(barriers, phase)
					barrierCounts[phase]++
					if fail && phase == failedPhase {
						return injected
					}
					return nil
				})
			}
			store.repository.stages = wrap(store.repository.stages, "stages")
			store.repository.anchors = wrap(store.repository.anchors, "anchors")
			store.repository.records = wrap(store.repository.records, "records")
			err := store.CleanupOwned(context.Background())
			if failedPhase == "" {
				if err != nil || store.RecordCount() != 0 {
					t.Fatalf("cleanup = %v, retained records = %d", err, store.RecordCount())
				}
				if barrierCounts["stages"] != 1 || barrierCounts["anchors"] != 1 {
					t.Fatalf("shared directory barriers = %v", barriers)
				}
			} else {
				if !errors.Is(err, injected) || store.RecordCount() != uint64(len(records)) {
					t.Fatalf("failed cleanup = %v, retained authority = %d", err, store.RecordCount())
				}
				if failedPhase == "stages" && slices.Contains(barriers, "anchors") ||
					failedPhase != "records" && slices.Contains(barriers, "records") {
					t.Fatalf("cleanup crossed a failed durability barrier: %v", barriers)
				}
				for _, record := range records {
					if failedPhase != "records" {
						if _, err := store.repository.Reopen(record.RecordID()); err != nil {
							t.Fatalf("failure discarded recovery evidence: %v", err)
						}
					}
					if failedPhase == "stages" {
						shard, name := ownedObjectLocation(record.OwnedObjectID(), ownedAnchorSuffix)
						directory, err := OpenShard(store.repository.anchors, shard, false)
						if err != nil {
							t.Fatal(err)
						}
						kind, exact, err := directory.ClassifyExactEntry(name)
						_ = directory.Close()
						if err != nil || !exact || kind != outputcap.EntryRegularFile {
							t.Fatalf("stage barrier failure removed anchor: %v, %v", kind, err)
						}
					}
				}
				fail = false
				if failedPhase == "records" {
					if err := store.CleanupOwned(context.Background()); !errors.Is(err, errFileExecutionAuthorityUnsettled) {
						t.Fatalf("ambiguous record deletion remained writable: %v", err)
					}
					// A new lease starts a new authority; reopening an adapter on the failed lease cannot clear uncertainty.
					store.repository.authority = newFileExecutionAuthority()
					store, err = NewFileExecutionStore(store.repository)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := store.CleanupOwned(context.Background()); err != nil {
					t.Fatalf("reconciled cleanup = %v", err)
				}
			}
			for _, record := range records {
				if _, err := store.repository.Reopen(record.RecordID()); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("cleanup retained physical record: %v", err)
				}
			}
		})
	}
}

func TestCleanupBatchCancellationAfterRecordRemovalRequiresReconciliation(t *testing.T) {
	store, _ := cleanupBatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := store.repository.records
	store.repository.records = &faultDirectory{Directory: root, openDirectory: func(name string, private bool) (outputcap.Directory, error) {
		directory, err := root.OpenDirectory(name, private)
		if err != nil {
			return nil, err
		}
		return &faultDirectory{Directory: directory, openObserved: func(name string, private bool) (outputcap.ObservedFile, error) {
			file, err := directory.OpenObservedFile(name, private)
			cancel()
			return file, err
		}}, nil
	}}
	if err := store.CleanupOwned(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup = %v", err)
	}
	if err := store.CleanupOwned(context.Background()); !errors.Is(err, errFileExecutionAuthorityUnsettled) {
		t.Fatalf("partially deleted checkpoint authority remained writable: %v", err)
	}
}

func TestCleanupBatchPreservesReplacedCheckpointEvidence(t *testing.T) {
	store, records := cleanupBatchFixture(t)
	original := records[0]
	replacement, err := checkpointmodel.PromoteInitialCandidate(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.repository.Replace(original, replacement); err != nil {
		t.Fatal(err)
	}
	if err := store.CleanupOwned(context.Background()); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("changed checkpoint accepted as deletion authority: %v", err)
	}
	observed, err := store.repository.Reopen(replacement.RecordID())
	if err != nil || observed.CommitState() != replacement.CommitState() {
		t.Fatalf("replaced recovery evidence was removed: %v", err)
	}
	if err := store.CleanupOwned(context.Background()); !errors.Is(err, errFileExecutionAuthorityUnsettled) {
		t.Fatalf("changed checkpoint left stale authority writable: %v", err)
	}
}
