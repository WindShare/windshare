package checkpointstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestOwnedCreateReconcilesFailureCutsWithoutInventingAuthority(t *testing.T) {
	newStore := func(t *testing.T, seed byte) (*FileExecutionStore, *Repository, checkpointmodel.ObjectID) {
		t.Helper()
		_, namespace, lease, repository, _, _ := openRepositoryFixture(t, seed)
		t.Cleanup(func() {
			_ = repository.Close()
			_ = lease.Close()
			_ = namespace.Close()
		})
		object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{seed + 1}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		return &FileExecutionStore{
			repository: &repository, authority: repository.authority,
		}, &repository, object
	}
	prepareShard := func(
		t *testing.T,
		root outputcap.Directory,
		object checkpointmodel.ObjectID,
		suffix string,
	) *memoryDirectory {
		t.Helper()
		shardName, _ := ownedObjectLocation(object, suffix)
		shard, err := OpenShard(root, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		memory, ok := shard.(*memoryDirectory)
		if !ok {
			t.Fatalf("owned shard = %T", shard)
		}
		return memory
	}

	t.Run("invalid and canceled requests", func(t *testing.T) {
		store, _, object := newStore(t, 0x11)
		if _, _, err := store.CreateOwnedFile(
			context.TODO(), nil, checkpointmodel.ObjectID{}, 4,
		); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("zero object error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := store.CreateOwnedFile(ctx, nil, object, 4); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled create error = %v", err)
		}
	})

	t.Run("existing object is a collision", func(t *testing.T) {
		store, _, object := newStore(t, 0x21)
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if err != nil || file == nil || observation.Condition() != fileexecution.OwnedReady {
			t.Fatalf("initial owned create = (%T, %d, %v)", file, observation.Condition(), err)
		}
		_ = file.Close()
		file, observation, err = store.CreateOwnedFile(context.Background(), nil, object, 4)
		if err != nil || file != nil || observation.Condition() != fileexecution.OwnedObjectCollision {
			t.Fatalf("duplicate owned create = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("anchor namespace failure leaves no ready object", func(t *testing.T) {
		store, repository, object := newStore(t, 0x31)
		failure := errors.New("anchor namespace unavailable")
		repository.anchors = &faultDirectory{
			Directory:       repository.anchors,
			createDirectory: func(string, bool) (outputcap.Directory, error) { return nil, failure },
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedAbsent || !errors.Is(err, failure) {
			t.Fatalf("anchor namespace cut = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("stage classification uncertainty stops before allocation", func(t *testing.T) {
		store, repository, object := newStore(t, 0x41)
		stageShard := prepareShard(t, repository.stages, object, ownedStageSuffix)
		failure := errors.New("stage classification unavailable")
		repository.stages = &faultDirectory{
			Directory: repository.stages,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &exactFaultDirectory{Directory: stageShard, classify: func(string) (outputcap.EntryKind, bool, error) {
					return outputcap.EntryAbsent, false, failure
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedAnchorMissing || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("uncertain stage observation = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("nil stage handle is reconciled as absent", func(t *testing.T) {
		store, repository, object := newStore(t, 0x51)
		stageShard := prepareShard(t, repository.stages, object, ownedStageSuffix)
		prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		repository.stages = &faultDirectory{
			Directory: repository.stages,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: stageShard, createFile: func(string, bool, int64) (outputcap.MutableFile, error) {
					return nil, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedAbsent || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil stage cut = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("stage durability failure exposes partial evidence", func(t *testing.T) {
		store, repository, object := newStore(t, 0x61)
		stageShard := prepareShard(t, repository.stages, object, ownedStageSuffix)
		prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		failure := errors.New("stage namespace sync failed")
		repository.stages = &faultDirectory{
			Directory: repository.stages,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: stageShard, sync: func() error { return failure }}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedAnchorMissing || !errors.Is(err, failure) {
			t.Fatalf("stage durability cut = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("nil link handle leaves an authenticated stage only", func(t *testing.T) {
		store, repository, object := newStore(t, 0x71)
		prepareShard(t, repository.stages, object, ownedStageSuffix)
		anchorShard := prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		repository.anchors = &faultDirectory{
			Directory: repository.anchors,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: anchorShard, linkFile: func(outputcap.FileIdentity, string) (outputcap.ObservedFile, error) {
					return nil, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedAnchorMissing || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("nil anchor cut = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})

	t.Run("anchor durability failure reopens the exact pair", func(t *testing.T) {
		store, repository, object := newStore(t, 0x81)
		prepareShard(t, repository.stages, object, ownedStageSuffix)
		anchorShard := prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		failure := errors.New("anchor namespace sync failed")
		repository.anchors = &faultDirectory{
			Directory: repository.anchors,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: anchorShard, sync: func() error { return failure }}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file == nil || observation.Condition() != fileexecution.OwnedReady || !errors.Is(err, failure) {
			t.Fatalf("anchor durability cut = (%T, %d, %v)", file, observation.Condition(), err)
		}
		_ = file.Close()
	})

	t.Run("handle mismatch is reconciled from durable identity", func(t *testing.T) {
		store, repository, object := newStore(t, 0x91)
		prepareShard(t, repository.stages, object, ownedStageSuffix)
		anchorShard := prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		repository.anchors = &faultDirectory{
			Directory: repository.anchors,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: anchorShard, linkFile: func(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
					linked, err := anchorShard.LinkFileNoReplace(source, name)
					if err != nil {
						return nil, err
					}
					return &ownedFaultFile{ObservedFile: linked}, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if err != nil || file == nil || observation.Condition() != fileexecution.OwnedReady {
			t.Fatalf("durable identity reconciliation = (%T, %d, %v)", file, observation.Condition(), err)
		}
		_ = file.Close()
	})

	t.Run("concurrent exact allocation becomes a collision", func(t *testing.T) {
		store, repository, object := newStore(t, 0xa1)
		stageShard := prepareShard(t, repository.stages, object, ownedStageSuffix)
		anchorShard := prepareShard(t, repository.anchors, object, ownedAnchorSuffix)
		_, stageName := ownedObjectLocation(object, ownedStageSuffix)
		_, anchorName := ownedObjectLocation(object, ownedAnchorSuffix)
		collision := errors.New("concurrent allocation won")
		repository.stages = &faultDirectory{
			Directory: repository.stages,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: stageShard, createFile: func(string, bool, int64) (outputcap.MutableFile, error) {
					stage, err := stageShard.CreateFile(stageName, true, 4)
					if err != nil {
						return nil, err
					}
					anchor, err := anchorShard.LinkFileNoReplace(stage, anchorName)
					_ = stage.Close()
					if err != nil {
						return nil, err
					}
					_ = anchor.Close()
					return nil, collision
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedObjectCollision || !errors.Is(err, collision) {
			t.Fatalf("concurrent allocation = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})
}
