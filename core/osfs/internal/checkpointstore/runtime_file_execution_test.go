package checkpointstore

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestRuntimeClosureLookupRequiresTheExactSemanticBinding(t *testing.T) {
	fixture := runtimeClosureClaim(t, 64)
	root := newMemoryDirectory()
	namespace, err := Initialize(CertifiedConfig{Root: root, Ownership: fixture.ownership})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := namespace.AcquireIntent(fixture.intent.Digest())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})

	candidate := runtimeClosureRecord(t, fixture, 0x45)
	committed, err := checkpointmodel.PromoteInitialCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Create(committed); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}

	lookupFailure := errors.New("stop after capturing the checkpoint key")
	capture := &runtimeClosureCheckpointCapture{err: lookupFailure}
	engine, err := fileexecution.New(fileexecution.Config{
		Intent: fixture.intent, Ownership: fixture.ownership, SessionID: fixture.sessionID,
		Directories: runtimeClosureDirectoryAuthority{}, Platform: store, Checkpoints: capture,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.BeginFile(context.Background(), fixture.claim); !errors.Is(err, lookupFailure) {
		t.Fatalf("capture checkpoint key = %v", err)
	}
	got, found, err := store.Lookup(context.Background(), capture.key)
	if err != nil || !found || got.RecordID() != committed.RecordID() {
		t.Fatalf("exact lookup = (%v, %t, %v)", got.RecordID(), found, err)
	}

	lookupKey := checkpointLookupKey(
		capture.key.TransferIntentDigest(), capture.key.FileID().Bytes(), capture.key.FileRevision().Bytes(),
		capture.key.CanonicalPath(), capture.key.ExactSize(), capture.key.BackendID(), capture.key.RootIdentity().Bytes(),
	)
	foreign := checkpointRecordFixture(t, fixture.ownership, fixture.intent.Digest(), 0x73)
	store.mu.Lock()
	store.records[lookupKey] = foreign
	store.mu.Unlock()
	if _, found, err := store.Lookup(context.Background(), capture.key); found || errorCode(err) != ErrorCorruptRecord ||
		!errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("lookup with a forged index binding = (%t, %v)", found, err)
	}

	store.mu.Lock()
	delete(store.records, lookupKey)
	store.mu.Unlock()
	if _, found, err := store.Lookup(context.Background(), capture.key); err != nil || found {
		t.Fatalf("absent exact binding = (%t, %v)", found, err)
	}
}

func TestRuntimeClosureOwnedCreationReconcilesOnlyStableOutcomes(t *testing.T) {
	failure := errors.New("owned creation crash cut")
	for _, test := range []struct {
		name          string
		configure     func(*Repository, outputcap.Directory, outputcap.Directory, string, string, uint64)
		wantCondition fileexecution.OwnedCondition
		wantFile      bool
		want          error
	}{
		{
			name: "exact creation after anchor sync failure",
			configure: func(repository *Repository, _, _ outputcap.Directory, _, anchorShard string, _ uint64) {
				base := repository.anchors
				repository.anchors = &runtimeClosureOpenDirectory{
					Directory: base,
					open: func(name string, private bool) (outputcap.Directory, error) {
						child, err := base.OpenDirectory(name, private)
						if err != nil || name != anchorShard {
							return child, err
						}
						return &runtimeClosureSyncDirectory{Directory: child, sync: func() error { return failure }}, nil
					},
				}
			},
			wantCondition: fileexecution.OwnedReady,
			wantFile:      true,
			want:          failure,
		},
		{
			name: "stable concurrent collision",
			configure: func(repository *Repository, stage, anchor outputcap.Directory, stageShard, _ string, exactSize uint64) {
				base := repository.stages
				repository.stages = &runtimeClosureOpenDirectory{
					Directory: base,
					open: func(name string, private bool) (outputcap.Directory, error) {
						child, err := base.OpenDirectory(name, private)
						if err != nil || name != stageShard {
							return child, err
						}
						return &runtimeClosureCreateFileDirectory{
							Directory: child,
							create: func(stageName string, private bool, _ int64) (outputcap.File, error) {
								created, createErr := stage.CreateFile(stageName, private, int64(exactSize))
								if createErr != nil {
									return nil, createErr
								}
								_, anchorName := ownedObjectLocation(runtimeClosureObject(t, 0x91), ownedAnchorSuffix)
								linked, linkErr := anchor.LinkFileNoReplace(created, anchorName)
								if linkErr != nil {
									return nil, errors.Join(linkErr, closeFile(linked), closeFile(created))
								}
								return nil, errors.Join(outputcap.ErrNamespaceCollision, closeFile(linked), closeFile(created))
							},
						}, nil
					},
				}
			},
			wantCondition: fileexecution.OwnedObjectCollision,
			wantFile:      false,
			want:          outputcap.ErrNamespaceCollision,
		},
		{
			name: "ambiguous stage-only mutation",
			configure: func(repository *Repository, _, _ outputcap.Directory, stageShard, _ string, _ uint64) {
				base := repository.stages
				repository.stages = &runtimeClosureOpenDirectory{
					Directory: base,
					open: func(name string, private bool) (outputcap.Directory, error) {
						child, err := base.OpenDirectory(name, private)
						if err != nil || name != stageShard {
							return child, err
						}
						return &runtimeClosureSyncDirectory{Directory: child, sync: func() error { return failure }}, nil
					},
				}
			},
			wantCondition: fileexecution.OwnedAnchorMissing,
			wantFile:      false,
			want:          failure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x83)
			t.Cleanup(func() {
				_ = repository.Close()
				_ = lease.Close()
				_ = namespace.Close()
			})
			store, err := NewFileExecutionStore(&repository)
			if err != nil {
				t.Fatal(err)
			}
			object := runtimeClosureObject(t, 0x91)
			const exactSize = uint64(32)
			stageShard, _ := ownedObjectLocation(object, ownedStageSuffix)
			anchorShard, _ := ownedObjectLocation(object, ownedAnchorSuffix)
			stage, err := OpenShard(repository.stages, stageShard, true)
			if err != nil {
				t.Fatal(err)
			}
			anchor, err := OpenShard(repository.anchors, anchorShard, true)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = anchor.Close()
				_ = stage.Close()
			})
			test.configure(&repository, stage, anchor, stageShard, anchorShard, exactSize)

			file, observation, err := store.CreateOwnedFile(context.Background(), object, exactSize)
			if observation.Condition() != test.wantCondition || (file != nil) != test.wantFile || !errors.Is(err, test.want) {
				t.Fatalf("reconciled creation = (%T, %d, %v)", file, observation.Condition(), err)
			}
			if file != nil {
				if file.ObjectID() != object {
					t.Fatalf("reconciled object = %v, want %v", file.ObjectID(), object)
				}
				if closeErr := file.Close(); closeErr != nil {
					t.Fatal(closeErr)
				}
				if closeErr := file.Close(); closeErr != nil {
					t.Fatalf("idempotent owned close = %v", closeErr)
				}
				if err := file.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
					t.Fatalf("closed owned modified time = %v", err)
				}
			}
		})
	}
}

func TestRuntimeClosureStoreReconcilesFreshDurableObservations(t *testing.T) {
	t.Run("startup rejects unrecoverable and duplicate semantic state", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xd1)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0xd2)
		if err := repository.Create(initial); err != nil {
			t.Fatal(err)
		}
		if store, err := NewFileExecutionStore(&repository); store != nil || errorCode(err) != ErrorUnsafeInstall {
			t.Fatalf("unwitnessed startup = (%T, %v)", store, err)
		}

		base := repository.records
		repository.records = &runtimeClosureNamesDirectory{
			Directory: base,
			names:     func(int) ([]string, error) { return nil, fs.ErrPermission },
		}
		if store, err := NewFileExecutionStore(&repository); store != nil || errorCode(err) != ErrorStateIO {
			t.Fatalf("startup scan failure = (%T, %v)", store, err)
		}
		repository.records = base
	})

	t.Run("startup rejects two record IDs for one checkpoint key", func(t *testing.T) {
		fixture := runtimeClosureClaim(t, 64)
		root := newMemoryDirectory()
		namespace, err := Initialize(CertifiedConfig{Root: root, Ownership: fixture.ownership})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := namespace.AcquireIntent(fixture.intent.Digest())
		if err != nil {
			t.Fatal(err)
		}
		repository, err := lease.OpenOrCreateRepository()
		if err != nil {
			t.Fatal(err)
		}
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		for _, object := range []byte{0xd3, 0xd4} {
			candidate := runtimeClosureRecord(t, fixture, object)
			committed, err := checkpointmodel.PromoteInitialCandidate(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.Create(committed); err != nil {
				t.Fatal(err)
			}
		}
		if store, err := NewFileExecutionStore(&repository); store != nil || errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("duplicate semantic key = (%T, %v)", store, err)
		}
	})

	t.Run("replace and missing observations are authoritative", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xe1)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		store, err := NewFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		initial := checkpointRecordFixture(t, ownership, intent, 0xe2)
		if _, err := store.Store(context.Background(), nil, initial); err != nil {
			t.Fatal(err)
		}
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := store.Store(context.Background(), &initial, committed)
		observed, present := observation.Record()
		if err != nil || !present || observed.CommitState() != checkpointmodel.CommitVerified {
			t.Fatalf("replacement observation = (%v, %t, %v)", observed.CommitState(), present, err)
		}

		missing := checkpointRecordFixture(t, ownership, intent, 0xe4)
		missingCommitted, err := checkpointmodel.PromoteInitialCandidate(missing)
		if err != nil {
			t.Fatal(err)
		}
		observation, err = store.Store(context.Background(), &missing, missingCommitted)
		if _, present := observation.Record(); present || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing durable observation = (%t, %v)", present, err)
		}
	})

	t.Run("a fresh observation cannot overwrite an existing semantic key", func(t *testing.T) {
		fixture := runtimeClosureClaim(t, 64)
		root := newMemoryDirectory()
		namespace, err := Initialize(CertifiedConfig{Root: root, Ownership: fixture.ownership})
		if err != nil {
			t.Fatal(err)
		}
		lease, err := namespace.AcquireIntent(fixture.intent.Digest())
		if err != nil {
			t.Fatal(err)
		}
		repository, err := lease.OpenOrCreateRepository()
		if err != nil {
			t.Fatal(err)
		}
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		store, err := NewFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		next := runtimeClosureRecord(t, fixture, 0xe6)
		conflict := runtimeClosureRecord(t, fixture, 0xe7)
		store.records[checkpointRecordLookupKey(next)] = conflict
		if _, err := store.Store(context.Background(), nil, next); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("fresh index collision = %v", err)
		}

		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Store(canceled, nil, next); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled store = %v", err)
		}
		var nilStore *FileExecutionStore
		if _, err := nilStore.Store(context.Background(), nil, next); err == nil {
			t.Fatal("nil store accepted a mutation")
		}
	})
}

func TestRuntimeClosureOwnedObservationClassifiesDurableTopology(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, outputcap.Directory, outputcap.Directory, checkpointmodel.ObjectID, uint64)
		want      fileexecution.OwnedCondition
		wantError bool
	}{
		{
			name: "anchor wrong kind",
			configure: func(t *testing.T, _, anchors outputcap.Directory, object checkpointmodel.ObjectID, _ uint64) {
				runtimeClosureCreateOwnedDirectory(t, anchors, object, ownedAnchorSuffix)
			},
			want: fileexecution.OwnedAnchorUnsafe, wantError: true,
		},
		{
			name: "anchor missing",
			configure: func(t *testing.T, stages, _ outputcap.Directory, object checkpointmodel.ObjectID, size uint64) {
				runtimeClosureCreateOwnedFile(t, stages, object, ownedStageSuffix, size)
			},
			want: fileexecution.OwnedAnchorMissing,
		},
		{
			name: "stage wrong kind",
			configure: func(t *testing.T, stages, anchors outputcap.Directory, object checkpointmodel.ObjectID, size uint64) {
				runtimeClosureCreateOwnedDirectory(t, stages, object, ownedStageSuffix)
				runtimeClosureCreateOwnedFile(t, anchors, object, ownedAnchorSuffix, size)
			},
			want: fileexecution.OwnedStageUnsafe, wantError: true,
		},
		{
			name: "stage missing",
			configure: func(t *testing.T, _, anchors outputcap.Directory, object checkpointmodel.ObjectID, size uint64) {
				runtimeClosureCreateOwnedFile(t, anchors, object, ownedAnchorSuffix, size)
			},
			want: fileexecution.OwnedStageMissing,
		},
		{
			name: "different objects",
			configure: func(t *testing.T, stages, anchors outputcap.Directory, object checkpointmodel.ObjectID, size uint64) {
				runtimeClosureCreateOwnedFile(t, stages, object, ownedStageSuffix, size)
				runtimeClosureCreateOwnedFile(t, anchors, object, ownedAnchorSuffix, size)
			},
			want: fileexecution.OwnedStageMismatch,
		},
		{
			name: "same object wrong size",
			configure: func(t *testing.T, stages, anchors outputcap.Directory, object checkpointmodel.ObjectID, size uint64) {
				runtimeClosureCreateOwnedPair(t, stages, anchors, object, size+1)
			},
			want: fileexecution.OwnedStageMismatch,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0xf1)
			defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
			store, err := NewFileExecutionStore(&repository)
			if err != nil {
				t.Fatal(err)
			}
			object := runtimeClosureObject(t, 0xf2)
			const exactSize = uint64(16)
			test.configure(t, repository.stages, repository.anchors, object, exactSize)
			file, observation, err := store.OpenOwnedFile(context.Background(), object, exactSize, false)
			if file != nil || observation.Condition() != test.want || (err != nil) != test.wantError {
				t.Fatalf("owned topology = (%T, %d, %v)", file, observation.Condition(), err)
			}
		})
	}
}

func TestRuntimeClosureRetirementPreservesUnsafeOrUnprovenEntries(t *testing.T) {
	t.Run("absent shard makes remove idempotent and syncs the root", func(t *testing.T) {
		_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x11)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		store, err := NewFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		object := runtimeClosureObject(t, 0x12)
		if observation, err := store.ApplyRetirement(
			context.Background(), object, fileexecution.RetirementRemoveStage,
		); err != nil || observation.Condition() != fileexecution.OwnedAbsent {
			t.Fatalf("absent removal = (%d, %v)", observation.Condition(), err)
		}
		before := directorySyncCalls(repository.stages.(*memoryDirectory))
		if _, err := store.ApplyRetirement(
			context.Background(), object, fileexecution.RetirementSyncStageNamespace,
		); err != nil || directorySyncCalls(repository.stages.(*memoryDirectory)) != before+1 {
			t.Fatalf("absent namespace sync = %v", err)
		}
	})

	t.Run("wrong-kind stage is preserved", func(t *testing.T) {
		_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x13)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		store, err := NewFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		object := runtimeClosureObject(t, 0x14)
		runtimeClosureCreateOwnedDirectory(t, repository.stages, object, ownedStageSuffix)
		anchor := runtimeClosureCreateOwnedFile(t, repository.anchors, object, ownedAnchorSuffix, 0)
		if err := anchor.Close(); err != nil {
			t.Fatal(err)
		}
		observation, err := store.ApplyRetirement(context.Background(), object, fileexecution.RetirementRemoveStage)
		if observation.Condition() != fileexecution.OwnedStageUnsafe || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("wrong-kind retirement = (%d, %v)", observation.Condition(), err)
		}
	})

	t.Run("shard authority failure is not absence", func(t *testing.T) {
		_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x15)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		store, err := NewFileExecutionStore(&repository)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("stage shard authority failure")
		base := repository.stages
		repository.stages = &runtimeClosureClassifyDirectory{
			Directory: base,
			classify:  func(string) (outputcap.EntryKind, bool, error) { return 0, false, failure },
		}
		object := runtimeClosureObject(t, 0x16)
		if _, err := store.ApplyRetirement(
			context.Background(), object, fileexecution.RetirementRemoveStage,
		); !errors.Is(err, failure) {
			t.Fatalf("retirement authority cut = %v", err)
		}
	})
}

func TestRuntimeClosureFileStoreRejectsInvalidAndUnprovenPublicAuthority(t *testing.T) {
	_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0xa1)
	defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	object := runtimeClosureObject(t, 0xa2)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Lookup(canceled, fileexecution.CheckpointKey{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup = %v", err)
	}
	if _, _, err := store.CreateOwnedFile(context.Background(), checkpointmodel.ObjectID{}, 0); err == nil {
		t.Fatal("zero object creation was accepted")
	}
	if _, _, err := store.OpenOwnedFile(context.Background(), checkpointmodel.ObjectID{}, 0, false); err == nil {
		t.Fatal("zero object open was accepted")
	}
	if matched, err := store.FinalMatchesOwned(context.Background(), checkpointmodel.ObjectID{}, 0, nil); matched || err == nil {
		t.Fatalf("invalid final comparison = (%t, %v)", matched, err)
	}
	if linked, err := store.PublishOwnedNoReplace(
		context.Background(), checkpointmodel.ObjectID{}, 0, nil, "",
	); linked != nil || err == nil {
		t.Fatalf("invalid publication = (%T, %v)", linked, err)
	}
	if linked, err := store.PublishOwnedNoReplace(
		context.Background(), object, 1, newMemoryDirectory(), "final",
	); linked != nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("publication without an owned anchor = (%T, %v)", linked, err)
	}
}
