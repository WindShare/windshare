package checkpointstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestOperationCandidateOwnershipDoesNotDependOnImage(t *testing.T) {
	operation := receivecontract.OperationID{1}
	namespace := operationCandidates(operation)
	target, image := "record.checkpoint", []byte("complete-image")
	name := OperationTemporaryName(operation, target, image, 0)
	if !namespace.owns(name) || !namespace.matches(name, target, image) ||
		namespace.matches(name, target, []byte("another-image")) {
		t.Fatal("candidate ownership and image identity are not independent")
	}
	for _, foreign := range []string{
		TemporaryName(target, image, 0),
		OperationTemporaryName(receivecontract.OperationID{2}, target, image, 0),
		OperationTemporaryName(operation, target, image, -1),
		OperationTemporaryName(operation, target, image, installationAttempts),
		name + "0",
		strings.ToUpper(name),
		string(namespace) + "not-an-image-0",
		string(namespace) + strings.Repeat("a", candidateImageHashBytes*2) + "-+1",
	} {
		if namespace.owns(foreign) {
			t.Fatalf("unreserved candidate acquired ownership: %q", foreign)
		}
	}
	if sharedCandidates.owns(name) || operationCandidates(receivecontract.OperationID{}).owns(name) {
		t.Fatal("an unowned installer acquired deletion authority")
	}
}

func TestInterruptedCheckpointCleanupConvergesAcrossReopen(t *testing.T) {
	for _, initial := range []bool{true, false} {
		for _, shape := range []string{"empty", "truncated", "zero-padded"} {
			label := "replacement/" + shape
			if initial {
				label = "initial/" + shape
			}
			t.Run(label, func(t *testing.T) {
				_, registry, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x71)
				t.Cleanup(func() {
					_ = repository.Close()
					_ = lease.Close()
					_ = registry.Close()
				})
				record := checkpointRecordFixture(t, ownership, fixture, 0x72)
				var stable checkpointmodel.Record
				if !initial {
					if _, err := repository.Create(record); err != nil {
						t.Fatal(err)
					}
					var err error
					stable, err = checkpointmodel.PromoteInitialCandidate(record)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := repository.Replace(record, stable); err != nil {
						t.Fatal(err)
					}
					record, err = checkpointmodel.AdvanceDurableRanges(stable, []checkpointmodel.Range{{Offset: 0, End: 8}})
					if err != nil {
						t.Fatal(err)
					}
				}
				shardName, target := recordLocation(record.RecordID())
				shard, err := OpenShard(repository.records, shardName, true)
				if err != nil {
					t.Fatal(err)
				}
				defer shard.Close()
				encoded := record.CanonicalBytes()
				name := OperationTemporaryName(record.OperationID(), target, encoded, 0)
				var interrupted []byte
				switch shape {
				case "truncated":
					interrupted = encoded[:len(encoded)/2]
				case "zero-padded":
					interrupted = make([]byte, len(encoded))
					copy(interrupted, encoded[:len(encoded)/2])
				}
				writeInterruptedCandidate(t, shard, name, interrupted)

				repository.authority = newFileExecutionAuthority()
				store, err := NewFileExecutionStore(&repository)
				if err != nil {
					t.Fatal(err)
				}
				slots, attention, err := store.Inventory()
				if err != nil || len(attention) != 0 || store.ReclaimedCandidateCount() != 1 {
					t.Fatalf("interrupted inventory = attention:%+v reclaimed:%d err:%v", attention, store.ReclaimedCandidateCount(), err)
				}
				if initial && len(slots) != 0 || !initial && len(slots) != 1 {
					t.Fatal("incomplete candidate granted authority or changed its predecessor")
				}
				if !initial {
					selected, exact := slots[0].Record()
					if !exact || !bytes.Equal(selected.CanonicalBytes(), stable.CanonicalBytes()) {
						t.Fatal("incomplete candidate changed its predecessor")
					}
				}
				if err := store.CleanupOwned(context.Background()); err != nil {
					t.Fatal(err)
				}
				repository.authority = newFileExecutionAuthority()
				reopened, err := NewFileExecutionStore(&repository)
				if err != nil {
					t.Fatal(err)
				}
				if err := reopened.CleanupOwned(context.Background()); err != nil {
					t.Fatalf("cleanup failed after a second reopen: %v", err)
				}
				if names, err := shard.Names(1); err != nil || len(names) != 0 {
					t.Fatalf("private checkpoint state remains: %v, %v", names, err)
				}
			})
		}
	}
}

func writeInterruptedCandidate(t *testing.T, directory outputcap.Directory, name string, image []byte) {
	t.Helper()
	file, err := directory.CreateFile(name, true, int64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	if len(image) > 0 {
		if _, err := file.WriteAt(image, 0); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedCleanupPreservesUnownedAndSubstitutedCandidates(t *testing.T) {
	_, registry, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x81)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	record := checkpointRecordFixture(t, ownership, fixture, 0x82)
	shardName, target := recordLocation(record.RecordID())
	encoded := record.CanonicalBytes()
	ownedName := OperationTemporaryName(record.OperationID(), target, encoded, 0)
	for _, name := range []string{
		TemporaryName(target, encoded, 0),
		OperationTemporaryName(receivecontract.OperationID{3}, target, encoded, 0),
		ownedName + "-foreign",
	} {
		shard := newMemoryDirectory()
		writeInterruptedCandidate(t, shard, name, nil)
		result := Snapshot{}
		if err := repository.reconcileCandidate(shard, shardName, name, nil, nil, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Attention()) != 1 || result.ReclaimedCandidateCount() != 0 {
			t.Fatalf("unknown candidate was accepted: %+v", result)
		}
		if _, found := shard.files[name]; !found {
			t.Fatal("unknown candidate was removed")
		}
	}

	shard := newMemoryDirectory()
	writeInterruptedCandidate(t, shard, ownedName, nil)
	replacement := []byte("replacement after observation")
	directory := &faultDirectory{
		Directory: shard,
		openObserved: func(name string, private bool) (outputcap.ObservedFile, error) {
			observed, err := shard.OpenObservedFile(name, private)
			shard.files[name] = &memoryFileData{bytes: replacement}
			return observed, err
		},
	}
	result := Snapshot{}
	if err := repository.reconcileCandidate(directory, shardName, ownedName, nil, nil, &result); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("substituted candidate cleanup = %v", err)
	}
	if !bytes.Equal(shard.files[ownedName].bytes, replacement) || result.ReclaimedCandidateCount() != 0 {
		t.Fatal("cleanup deleted a replacement instead of its observed object")
	}
}

type candidateRemoveFailure struct {
	outputcap.Directory
	failure error
}

func (directory candidateRemoveFailure) RemoveFile(string, outputcap.FileIdentity) error {
	return directory.failure
}

func TestInterruptedCleanupSurfacesFailuresAndCanRetry(t *testing.T) {
	for _, step := range []string{"remove", "sync", "read"} {
		t.Run(step, func(t *testing.T) {
			_, registry, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x91)
			t.Cleanup(func() {
				_ = repository.Close()
				_ = lease.Close()
				_ = registry.Close()
			})
			record := checkpointRecordFixture(t, ownership, fixture, 0x92)
			shardName, target := recordLocation(record.RecordID())
			name := OperationTemporaryName(record.OperationID(), target, record.CanonicalBytes(), 0)
			shard := newMemoryDirectory()
			writeInterruptedCandidate(t, shard, name, []byte("unfinished"))
			failure := errors.New("injected candidate " + step + " failure")
			var directory outputcap.Directory
			switch step {
			case "remove":
				directory = candidateRemoveFailure{Directory: shard, failure: failure}
			case "sync":
				directory = &faultDirectory{Directory: shard, sync: func() error { return failure }}
			case "read":
				directory = &faultDirectory{Directory: shard, openObserved: func(name string, private bool) (outputcap.ObservedFile, error) {
					file, err := shard.OpenMutableFile(name, private)
					return &faultFile{MutableFile: file, readAt: func([]byte, int64) (int, error) { return 0, failure }}, err
				}}
			}
			result := Snapshot{}
			if err := repository.reconcileCandidate(directory, shardName, name, nil, nil, &result); !errors.Is(err, failure) {
				t.Fatalf("candidate %s failure hidden: %v", step, err)
			}
			if result.ReclaimedCandidateCount() != 0 {
				t.Fatal("failed cleanup was reported as durable")
			}
			if step != "sync" {
				if _, err := shard.OpenObservedFile(name, true); err != nil {
					t.Fatalf("failure removed the pending candidate: %v", err)
				}
			}
			if err := repository.reconcileCandidate(shard, shardName, name, nil, nil, &result); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
			if _, err := shard.OpenObservedFile(name, true); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("retry retained the interrupted candidate: %v", err)
			}
		})
	}
}

func TestRepositoryWriterLeavesReclaimableSlotsAtAbruptWriteCut(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "replace"}[replace], func(t *testing.T) {
			_, registry, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xa1)
			t.Cleanup(func() {
				_ = repository.Close()
				_ = lease.Close()
				_ = registry.Close()
			})
			initial := checkpointRecordFixture(t, ownership, fixture, 0xa2)
			if replace {
				if _, err := repository.Create(initial); err != nil {
					t.Fatal(err)
				}
			}
			next, err := checkpointmodel.PromoteInitialCandidate(initial)
			if err != nil {
				t.Fatal(err)
			}
			shardName, _ := recordLocation(initial.RecordID())
			shard, err := OpenShard(repository.records, shardName, true)
			if err != nil {
				t.Fatal(err)
			}
			defer shard.Close()
			original := repository.records
			crash := errors.New("abrupt checkpoint write cut")
			var candidateName string
			repository.records = wrapReconciliationShard(original, shardName, func(directory *faultDirectory) {
				directory.createFile = func(name string, private bool, size int64) (outputcap.MutableFile, error) {
					file, err := directory.Directory.CreateFile(name, private, size)
					if err != nil {
						return file, err
					}
					candidateName = name
					return &faultFile{MutableFile: file, writeAt: func(encoded []byte, offset int64) (int, error) {
						if _, err := file.WriteAt(encoded[:len(encoded)/2], offset); err != nil {
							t.Fatal(err)
						}
						// Returning an error would exercise rollback rather than a crash.
						panic(crash)
					}}, nil
				}
			})
			func() {
				defer func() {
					if observed := recover(); observed != crash {
						t.Fatalf("write cut = %v", observed)
					}
				}()
				if replace {
					_, _ = repository.Replace(initial, next)
				} else {
					_, _ = repository.Create(initial)
				}
			}()
			repository.records = original
			if !operationCandidates(initial.OperationID()).owns(candidateName) {
				t.Fatalf("writer created an unowned crash artifact: %s", candidateName)
			}
			result, err := repository.Reconcile(func(checkpointmodel.Record) (bool, error) { return false, nil })
			if err != nil || result.ReclaimedCandidateCount() != 1 || len(result.Attention()) != 0 {
				t.Fatalf("writer crash recovery = %+v, %v", result, err)
			}
			if replace && (len(result.Records()) != 1 || !bytes.Equal(result.Records()[0].CanonicalBytes(), initial.CanonicalBytes())) ||
				!replace && len(result.Records()) != 0 {
				t.Fatal("interrupted write changed fixed checkpoint authority")
			}
		})
	}
}

func TestInterruptedCleanupCannotOutliveOperationLease(t *testing.T) {
	_, registry, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xb1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	record := checkpointRecordFixture(t, ownership, fixture, 0xb2)
	shardName, target := recordLocation(record.RecordID())
	shard, err := OpenShard(repository.records, shardName, true)
	if err != nil {
		t.Fatal(err)
	}
	defer shard.Close()
	name := OperationTemporaryName(record.OperationID(), target, record.CanonicalBytes(), 0)
	writeInterruptedCandidate(t, shard, name, nil)
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Reconcile(func(checkpointmodel.Record) (bool, error) { return false, nil }); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("released lease still granted cleanup authority: %v", err)
	}
	file, err := shard.OpenObservedFile(name, true)
	if err != nil {
		t.Fatalf("released lease removed candidate: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
