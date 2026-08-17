package checkpointstore

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type reconciliationNativeFailure struct {
	class outputcap.NativeErrorClass
}

func (failure reconciliationNativeFailure) Error() string { return "provider failure" }
func (failure reconciliationNativeFailure) NativeErrorClass() outputcap.NativeErrorClass {
	return failure.class
}

type recoveryFaultFile struct {
	outputcap.RecoveryDurabilityFile
	syncErr  error
	closeErr error
}

func (file *recoveryFaultFile) Sync() error  { return file.syncErr }
func (file *recoveryFaultFile) Close() error { return file.closeErr }

func candidateReconciliationFixture(
	t *testing.T,
) (*FileExecutionStore, *Repository, checkpointmodel.Record) {
	t.Helper()
	_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0xc1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecordFixture(t, ownership, intent, 0xc2)
	owned, observation, err := store.CreateOwnedFile(
		context.Background(), nil, record.OwnedObjectID(), record.ExactSize(),
	)
	if err != nil || owned == nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("create candidate object = (%T, %d, %v)", owned, observation.Condition(), err)
	}
	if err := owned.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Store(context.Background(), nil, record); err != nil {
		t.Fatal(err)
	}
	return store, &repository, record
}

func wrapReconciliationShard(
	root outputcap.Directory,
	shardName string,
	configure func(*faultDirectory),
) outputcap.Directory {
	return &faultDirectory{
		Directory: root,
		openDirectory: func(name string, private bool) (outputcap.Directory, error) {
			opened, err := root.OpenDirectory(name, private)
			if err != nil || name != shardName {
				return opened, err
			}
			wrapped := &faultDirectory{Directory: opened}
			configure(wrapped)
			return wrapped, nil
		},
	}
}

func TestCandidateRecoveryUsesDurabilityAndObservationCapabilities(t *testing.T) {
	store, repository, record := candidateReconciliationFixture(t)
	stageShard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedStageSuffix)
	anchorShard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedAnchorSuffix)

	stageRoot := repository.stages
	anchorRoot := repository.anchors
	var recoveryOpens, observedOpens, mutableOpens int
	var recoveryHandle outputcap.RecoveryDurabilityFile
	var observedHandle outputcap.ObservedFile
	repository.stages = wrapReconciliationShard(stageRoot, stageShard, func(shard *faultDirectory) {
		shard.openRecovery = func(name string, private bool) (outputcap.RecoveryDurabilityFile, error) {
			recoveryOpens++
			opened, err := shard.Directory.OpenRecoveryDurabilityFile(name, private)
			recoveryHandle = opened
			return opened, err
		}
		shard.openFile = func(name string, private, writable bool) (outputcap.MutableFile, error) {
			mutableOpens++
			return shard.Directory.OpenMutableFile(name, private)
		}
	})
	repository.anchors = wrapReconciliationShard(anchorRoot, anchorShard, func(shard *faultDirectory) {
		shard.openObserved = func(name string, private bool) (outputcap.ObservedFile, error) {
			observedOpens++
			opened, err := shard.Directory.OpenObservedFile(name, private)
			observedHandle = opened
			return opened, err
		}
		shard.openFile = func(name string, private, writable bool) (outputcap.MutableFile, error) {
			mutableOpens++
			return shard.Directory.OpenMutableFile(name, private)
		}
	})

	recoverable, err := store.candidateDurable(record)
	if err != nil || !recoverable {
		t.Fatalf("candidate durability = (%t, %v)", recoverable, err)
	}
	if recoveryOpens != 1 || observedOpens != 1 || mutableOpens != 0 {
		t.Fatalf("candidate opens = recovery:%d observed:%d mutable:%d", recoveryOpens, observedOpens, mutableOpens)
	}
	if recoveryHandle == nil {
		t.Fatal("candidate recovery did not retain its durability witness")
	}
	if _, writable := any(recoveryHandle).(io.WriterAt); writable {
		t.Fatal("candidate recovery handle exposed content mutation")
	}
	if _, readable := any(recoveryHandle).(io.ReaderAt); readable {
		t.Fatal("candidate recovery handle exposed content observation")
	}
	if observedHandle == nil {
		t.Fatal("candidate recovery did not retain its observed anchor")
	}
	if _, durable := any(observedHandle).(interface{ Sync() error }); durable {
		t.Fatal("candidate anchor observation exposed durability authority")
	}
}

func TestObservedOwnedFilePreservesObservationWithoutMutationAuthority(t *testing.T) {
	store, _, record := candidateReconciliationFixture(t)
	observed, observation, err := store.OpenOwnedFile(
		context.Background(), record.OwnedObjectID(), record.ExactSize(), false,
	)
	if err != nil || observed == nil || observation.Condition() != fileexecution.OwnedReady {
		t.Fatalf("open observed owned file = (%T, %d, %v)", observed, observation.Condition(), err)
	}
	if observed.ObjectID() != record.OwnedObjectID() {
		t.Fatalf("observed object = %x", observed.ObjectID())
	}
	if matches, matchErr := observed.MetadataMatches(record.ExactSize(), catalog.ModifiedTime{}); matchErr != nil || !matches {
		t.Fatalf("observed metadata = (%t, %v)", matches, matchErr)
	}
	if _, writeErr := observed.WriteAt([]byte("mutation canary"), 0); !errors.Is(writeErr, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("observed write = %v", writeErr)
	}
	if syncErr := observed.Sync(); !errors.Is(syncErr, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("observed sync = %v", syncErr)
	}
	if metadataErr := observed.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(metadataErr, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("observed metadata mutation = %v", metadataErr)
	}
	if err := observed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := observed.Close(); err != nil {
		t.Fatalf("second observed close = %v", err)
	}
	if _, matchErr := observed.MetadataMatches(record.ExactSize(), catalog.ModifiedTime{}); !errors.Is(matchErr, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed observed metadata = %v", matchErr)
	}
}

func TestCandidateReconciliationFreezesFirstFailureAndRetainsCandidate(t *testing.T) {
	for _, test := range []struct {
		name      string
		step      ReconciliationStep
		configure func(*Repository, checkpointmodel.Record, error)
		cleanup   bool
	}{
		{
			name: "candidate observation", step: ReconciliationCandidateObservation,
			configure: func(repository *Repository, record checkpointmodel.Record, failure error) {
				shard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedStageSuffix)
				root := repository.stages
				repository.stages = wrapReconciliationShard(root, shard, func(directory *faultDirectory) {
					directory.openRecovery = func(string, bool) (outputcap.RecoveryDurabilityFile, error) {
						return nil, failure
					}
				})
			},
		},
		{
			name: "stage durability", step: ReconciliationStageDurability, cleanup: true,
			configure: func(repository *Repository, record checkpointmodel.Record, failure error) {
				shard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedStageSuffix)
				root := repository.stages
				repository.stages = wrapReconciliationShard(root, shard, func(directory *faultDirectory) {
					directory.openRecovery = func(name string, private bool) (outputcap.RecoveryDurabilityFile, error) {
						opened, err := directory.Directory.OpenRecoveryDurabilityFile(name, private)
						return &recoveryFaultFile{
							RecoveryDurabilityFile: opened, syncErr: failure,
							closeErr: errors.New("cleanup canary"),
						}, err
					}
				})
			},
		},
		{
			name: "stage namespace durability", step: ReconciliationNamespaceDurability,
			configure: func(repository *Repository, record checkpointmodel.Record, failure error) {
				shard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedStageSuffix)
				root := repository.stages
				repository.stages = wrapReconciliationShard(root, shard, func(directory *faultDirectory) {
					directory.sync = func() error { return failure }
				})
			},
		},
		{
			name: "anchor namespace durability", step: ReconciliationNamespaceDurability,
			configure: func(repository *Repository, record checkpointmodel.Record, failure error) {
				shard, _ := ownedObjectLocation(record.OwnedObjectID(), ownedAnchorSuffix)
				root := repository.anchors
				repository.anchors = wrapReconciliationShard(root, shard, func(directory *faultDirectory) {
					directory.sync = func() error { return failure }
				})
			},
		},
		{
			name: "record promotion", step: ReconciliationRecordPromotion,
			configure: func(repository *Repository, record checkpointmodel.Record, failure error) {
				shard, _ := recordLocation(record.RecordID())
				root := repository.records
				repository.records = wrapReconciliationShard(root, shard, func(directory *faultDirectory) {
					directory.replaceFile = func(outputcap.FileIdentity, string) error { return failure }
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, repository, candidate := candidateReconciliationFixture(t)
			failure := reconciliationNativeFailure{class: outputcap.NativeErrorAccessDenied}
			test.configure(repository, candidate, failure)

			_, err := NewFileExecutionStore(repository)
			if err == nil {
				t.Fatal("candidate reconciliation unexpectedly succeeded")
			}
			var reconciliation *ReconciliationError
			if !errors.As(err, &reconciliation) || reconciliation.Step() != test.step ||
				reconciliation.Fault().Valid() == false {
				t.Fatalf("reconciliation diagnosis = (%T, %v)", reconciliation, err)
			}
			if native, ok := reconciliation.NativeClass(); !ok || native != outputcap.NativeErrorAccessDenied {
				t.Fatalf("native class = (%s, %t)", native, ok)
			}
			if test.cleanup && !errors.Is(err, failure) {
				t.Fatalf("primary failure was lost after cleanup join: %v", err)
			}

			retained, reopenErr := repository.Reopen(candidate.RecordID())
			if reopenErr != nil || retained.CommitState() != checkpointmodel.CommitCandidate ||
				retained.CheckpointGeneration() != candidate.CheckpointGeneration() {
				t.Fatalf("retained candidate = (%+v, %v)", retained, reopenErr)
			}
		})
	}
}
