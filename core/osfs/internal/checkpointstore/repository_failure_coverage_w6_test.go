package checkpointstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/fileexecution"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestOperationRepositoryRejectsIncompleteAndSubstitutedAuthority(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0x11)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})

	var nilRepository *Repository
	if err := nilRepository.InstallOperation(fixture.operation, fixture.binding); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil install error = %v", err)
	}
	if _, err := nilRepository.ReadOperation(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil operation read error = %v", err)
	}
	if _, err := nilRepository.ReadMaterializationBinding(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reservation read error = %v", err)
	}
	if err := repository.InstallOperation(fixture.operation, append(bytes.Clone(fixture.binding), 0)); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("substituted reservation install error = %v", err)
	}
	foreign := compatibleOperationFixture(t, fixture, 0x21)
	if err := repository.InstallOperation(foreign.operation, foreign.binding); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("foreign operation install error = %v", err)
	}

	operationDirectory := repository.operation.(*memoryDirectory)
	operationFile := operationDirectory.files[OperationFile]
	reservationFile := operationDirectory.files[ReservationFile]
	operationFile.mu.Lock()
	originalOperation := bytes.Clone(operationFile.bytes)
	operationFile.bytes = []byte("corrupt operation")
	operationFile.mu.Unlock()
	if _, err := repository.ReadOperation(); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("corrupt operation read error = %v", err)
	}
	operationFile.mu.Lock()
	operationFile.bytes = originalOperation
	operationFile.mu.Unlock()

	reservationFile.mu.Lock()
	originalReservation := bytes.Clone(reservationFile.bytes)
	reservationFile.bytes[0] ^= 1
	reservationFile.mu.Unlock()
	if _, err := repository.ReadMaterializationBinding(); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("substituted reservation read error = %v", err)
	}
	reservationFile.mu.Lock()
	reservationFile.bytes = originalReservation
	reservationFile.mu.Unlock()

	var nilLease *OperationLease
	if err := nilLease.RegisterLookup(fixture.operation); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil lookup registration error = %v", err)
	}
	if err := lease.RegisterLookup(foreign.operation); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("foreign lookup registration error = %v", err)
	}
	if _, err := namespace.LookupCompatible(checkpointmodel.CompatibleOperationKey{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero compatible key error = %v", err)
	}
}

func TestCompatibleLookupTurnsStorageUncertaintyIntoAttention(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0x31)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	key := fixture.operation.ReopenKey().CompatibleKey()
	lookupRoot := namespace.lookup.(*memoryDirectory)
	keyDirectory := lookupRoot.dirsForTest(t, bytesToHex(key.Bytes()))
	operationName := operationNamespaceName(fixture.intent.OperationID())

	t.Run("short index image", func(t *testing.T) {
		index := keyDirectory.files[operationName]
		index.mu.Lock()
		original := bytes.Clone(index.bytes)
		index.bytes = []byte{1}
		index.mu.Unlock()
		t.Cleanup(func() {
			index.mu.Lock()
			index.bytes = original
			index.mu.Unlock()
		})
		lookup, err := namespace.LookupCompatible(key)
		if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
			t.Fatalf("short index lookup = (%d, %t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
		}
	})

	t.Run("index enumeration failure", func(t *testing.T) {
		failure := errors.New("list lookup failed")
		original := namespace.lookup
		namespace.lookup = &faultDirectory{
			Directory: original,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: keyDirectory, names: func(int) ([]string, error) { return nil, failure }}, nil
			},
		}
		defer func() { namespace.lookup = original }()
		if _, err := namespace.LookupCompatible(key); !errors.Is(err, failure) {
			t.Fatalf("lookup enumeration error = %v", err)
		}
	})

	t.Run("bounded index overflow", func(t *testing.T) {
		original := namespace.lookup
		namespace.lookup = &faultDirectory{
			Directory: original,
			openDirectory: func(string, bool) (outputcap.Directory, error) {
				return &faultDirectory{Directory: keyDirectory, names: func(int) ([]string, error) {
					return make([]string, checkpointmodel.MaxCheckpointRecordsPerOperation+1), nil
				}}, nil
			},
		}
		defer func() { namespace.lookup = original }()
		lookup, err := namespace.LookupCompatible(key)
		if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
			t.Fatalf("overflow lookup = (%d, %t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
		}
	})

	t.Run("operation image cannot authenticate index", func(t *testing.T) {
		operationDirectory := repository.operation.(*memoryDirectory)
		operationFile := operationDirectory.files[OperationFile]
		index := keyDirectory.files[operationName]
		operationFile.mu.Lock()
		originalOperation := bytes.Clone(operationFile.bytes)
		operationFile.bytes = []byte("corrupt")
		corruptOperation := bytes.Clone(operationFile.bytes)
		operationFile.mu.Unlock()
		index.mu.Lock()
		originalIndex := bytes.Clone(index.bytes)
		digest := sha256.Sum256(corruptOperation)
		index.bytes = digest[:]
		index.mu.Unlock()
		defer func() {
			operationFile.mu.Lock()
			operationFile.bytes = originalOperation
			operationFile.mu.Unlock()
			index.mu.Lock()
			index.bytes = originalIndex
			index.mu.Unlock()
		}()
		lookup, err := namespace.LookupCompatible(key)
		if err != nil || !lookup.OwnershipUncertain() || len(lookup.Operations()) != 0 {
			t.Fatalf("corrupt operation lookup = (%d, %t, %v)", len(lookup.Operations()), lookup.OwnershipUncertain(), err)
		}
	})
}

func TestAggregateRepositoryRejectsForeignAndUncommittedEvidence(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0x41)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})

	reference := installAggregateCheckpoint(t, &repository, ownership, fixture, 0x42)
	evidence, err := checkpointmodel.AggregateDigestFromBytes(bytes.Repeat([]byte{0x43}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	foreignFixture := compatibleOperationFixture(t, fixture, 0x51)
	foreignReceipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptTreeCompletion, OperationID: foreignFixture.intent.OperationID(),
		ReceiveIntent: foreignFixture.intent.Digest(), ReservationDigest: foreignFixture.intent.BindingDigest(),
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference}, EvidenceDigest: evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(foreignReceipt); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("foreign receipt error = %v", err)
	}
	foreignLifecycle, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: foreignFixture.intent.OperationID(), ReceiveIntent: foreignFixture.intent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateLifecycleState(foreignLifecycle); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("foreign lifecycle error = %v", err)
	}
	wrongInitial, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 2, Phase: checkpointmodel.LifecycleReceiving,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{reference}, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateLifecycleState(wrongInitial); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("non-initial lifecycle create error = %v", err)
	}

	candidate := checkpointRecordFixture(t, ownership, fixture, 0x61)
	if err := repository.Create(candidate); err != nil {
		t.Fatal(err)
	}
	candidateReference, err := checkpointmodel.FileCheckpointReferenceFromIdentity(
		candidate.RecordID(), candidate.CheckpointGeneration(),
	)
	if err != nil {
		t.Fatal(err)
	}
	uncommittedReceipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptTreeCompletion, OperationID: fixture.intent.OperationID(),
		ReceiveIntent: fixture.intent.Digest(), ReservationDigest: fixture.intent.BindingDigest(),
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{candidateReference}, EvidenceDigest: evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(uncommittedReceipt); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("uncommitted aggregate reference error = %v", err)
	}

	missingID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0x71}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	missingReference, err := checkpointmodel.FileCheckpointReferenceFromIdentity(missingID, 1)
	if err != nil {
		t.Fatal(err)
	}
	missingReceipt, err := checkpointmodel.NewDirectTreeReceipt(checkpointmodel.DirectTreeReceiptSpec{
		Kind: checkpointmodel.ReceiptTreeCompletion, OperationID: fixture.intent.OperationID(),
		ReceiveIntent: fixture.intent.Digest(), ReservationDigest: fixture.intent.BindingDigest(),
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{missingReference}, EvidenceDigest: evidence, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.InstallReceipt(missingReceipt); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("missing aggregate reference error = %v", err)
	}
}

type exactFaultDirectory struct {
	outputcap.Directory
	classify func(string) (outputcap.EntryKind, bool, error)
	remove   func(string, outputcap.File) error
}

func (directory *exactFaultDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	if directory.classify != nil {
		return directory.classify(name)
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *exactFaultDirectory) RemoveFile(name string, file outputcap.File) error {
	if directory.remove != nil {
		return directory.remove(name, file)
	}
	return directory.Directory.RemoveFile(name, file)
}

type ownedFaultFile struct {
	outputcap.File
	same func(outputcap.File) (bool, error)
	size func() (uint64, error)
}

type leaseFaultDirectory struct {
	outputcap.Directory
	acquireLock func(string, bool) (outputcap.Lock, bool, error)
	duplicate   func() (outputcap.Directory, error)
	syncErr     error
}

func (directory *leaseFaultDirectory) AcquireLock(name string, existing bool) (outputcap.Lock, bool, error) {
	if directory.acquireLock != nil {
		return directory.acquireLock(name, existing)
	}
	return directory.Directory.AcquireLock(name, existing)
}

func (directory *leaseFaultDirectory) Duplicate() (outputcap.Directory, error) {
	if directory.duplicate != nil {
		return directory.duplicate()
	}
	return directory.Directory.Duplicate()
}

func (directory *leaseFaultDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.Directory.Sync()
}

type coverageLock struct {
	outputcap.Lock
	file outputcap.File
}

func (lock *coverageLock) File() outputcap.File { return lock.file }
func (lock *coverageLock) Close() error         { return nil }

func (file *ownedFaultFile) SameFile(other outputcap.File) (bool, error) {
	if file.same != nil {
		return file.same(other)
	}
	return file.File.SameFile(other)
}

func (file *ownedFaultFile) Size() (uint64, error) {
	if file.size != nil {
		return file.size()
	}
	return file.File.Size()
}

func TestOwnedObservationClassifiesEveryRecoveryHazard(t *testing.T) {
	readyFile := &memoryFile{data: &memoryFileData{bytes: make([]byte, 4)}}
	otherFile := &memoryFile{data: &memoryFileData{bytes: make([]byte, 4)}}
	ready := privateFileObservation{file: readyFile, state: privateEntryReady}
	absent := privateFileObservation{state: privateEntryAbsent}
	unsafe := privateFileObservation{state: privateEntryUnsafe}

	cases := []struct {
		name     string
		stage    privateFileObservation
		anchor   privateFileObservation
		validate bool
		want     fileexecution.OwnedCondition
		wantErr  bool
	}{
		{"absent", absent, absent, true, fileexecution.OwnedAbsent, false},
		{"anchor unsafe", ready, unsafe, true, fileexecution.OwnedAnchorUnsafe, false},
		{"anchor missing", ready, absent, true, fileexecution.OwnedAnchorMissing, false},
		{"stage unsafe", unsafe, ready, true, fileexecution.OwnedStageUnsafe, false},
		{"stage missing", absent, ready, true, fileexecution.OwnedStageMissing, false},
		{"nil ready handles", privateFileObservation{state: privateEntryReady}, ready, true, fileexecution.OwnedStageUnsafe, true},
		{"different files", ready, privateFileObservation{file: otherFile, state: privateEntryReady}, true, fileexecution.OwnedStageMismatch, false},
		{"size mismatch", ready, ready, true, fileexecution.OwnedStageMismatch, false},
		{"identity only", ready, ready, false, fileexecution.OwnedReady, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			exactSize := uint64(4)
			if test.name == "size mismatch" {
				exactSize = 5
			}
			condition, err := classifyOwnedObservation(test.stage, test.anchor, exactSize, test.validate)
			if condition != test.want || (err != nil) != test.wantErr {
				t.Fatalf("owned classification = (%d, %v), want (%d, err=%t)", condition, err, test.want, test.wantErr)
			}
		})
	}

	identityFailure := errors.New("identity unavailable")
	fault := &ownedFaultFile{File: readyFile, same: func(outputcap.File) (bool, error) { return false, identityFailure }}
	condition, err := classifyOwnedObservation(
		privateFileObservation{file: fault, state: privateEntryReady}, ready, 4, true,
	)
	if condition != fileexecution.OwnedStageUnsafe || !errors.Is(err, identityFailure) {
		t.Fatalf("identity failure classification = (%d, %v)", condition, err)
	}
	if _, err := sameExactOwnedFiles(nil, readyFile, 4); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil exact-owned comparison error = %v", err)
	}
}

func TestOwnedCleanupRefusesInexactOrChangedEntries(t *testing.T) {
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x81}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	shardName, entryName := ownedObjectLocation(object, ownedStageSuffix)

	t.Run("missing shard is already clean", func(t *testing.T) {
		if err := removeOwnedEntry(newMemoryDirectory(), object, ownedStageSuffix); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("inexact classification is uncertain", func(t *testing.T) {
		root := newMemoryDirectory()
		shard := newMemoryDirectory()
		root.dirs[shardName] = shard
		rootFault := &faultDirectory{Directory: root, openDirectory: func(string, bool) (outputcap.Directory, error) {
			return &exactFaultDirectory{Directory: shard, classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryRegularFile, false, nil
			}}, nil
		}}
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("inexact cleanup error = %v", err)
		}
	})

	t.Run("wrong entry kind is retained", func(t *testing.T) {
		root := newMemoryDirectory()
		shard := newMemoryDirectory()
		root.dirs[shardName] = shard
		rootFault := &faultDirectory{Directory: root, openDirectory: func(string, bool) (outputcap.Directory, error) {
			return &exactFaultDirectory{Directory: shard, classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryDirectory, true, nil
			}}, nil
		}}
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("wrong-kind cleanup error = %v", err)
		}
	})

	t.Run("changed identity is retained", func(t *testing.T) {
		root := newMemoryDirectory()
		shard := newMemoryDirectory()
		root.dirs[shardName] = shard
		file, createErr := shard.CreateFile(entryName, true, 1)
		if createErr != nil {
			t.Fatal(createErr)
		}
		changed := errors.New("entry changed")
		rootFault := &faultDirectory{Directory: root, openDirectory: func(string, bool) (outputcap.Directory, error) {
			return &exactFaultDirectory{Directory: shard, remove: func(string, outputcap.File) error { return changed }}, nil
		}}
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix); !errors.Is(err, changed) {
			t.Fatalf("changed cleanup error = %v", err)
		}
		_ = file.Close()
	})

	t.Run("missing shard syncs root durability", func(t *testing.T) {
		root := newMemoryDirectory()
		if err := syncOwnedEntryNamespace(root, object); err != nil || directorySyncCalls(root) != 1 {
			t.Fatalf("missing shard sync = (%d, %v)", directorySyncCalls(root), err)
		}
	})
}

func TestOwnedCreateReconcilesPartialAllocationWithoutInventingReadyState(t *testing.T) {
	_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x91)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store := &FileExecutionStore{repository: &repository, records: make(map[[sha256.Size]byte]checkpointmodel.Record)}
	object, err := checkpointmodel.ObjectIDFromBytes(bytes.Repeat([]byte{0x92}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("stage namespace unavailable")
	originalStages := repository.stages
	repository.stages = &faultDirectory{
		Directory:       originalStages,
		createDirectory: func(string, bool) (outputcap.Directory, error) { return nil, failure },
	}
	file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
	if file != nil || observation.Condition() != fileexecution.OwnedAbsent || !errors.Is(err, failure) {
		t.Fatalf("partial allocation reconciliation = (%T, %d, %v)", file, observation.Condition(), err)
	}
	repository.stages = originalStages

	var nilFile *nativeOwnedFile
	if err := nilFile.SetModifiedTime(catalog.ModifiedTime{}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("nil native owned metadata error = %v", err)
	}
	if err := nilFile.Close(); err != nil {
		t.Fatalf("nil native owned close error = %v", err)
	}
}

func TestFileExecutionStoreRejectsAmbiguousIndexAndMissingInstall(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xa1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	store, err := NewFileExecutionStore(&repository)
	if err != nil {
		t.Fatal(err)
	}
	record := checkpointRecordFixture(t, ownership, fixture, 0xa2)
	if err := store.indexRecord(checkpointmodel.Record{}); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("invalid index error = %v", err)
	}
	if err := store.indexRecord(record); err != nil {
		t.Fatal(err)
	}
	conflictingSpec := checkpointmodel.RecordSpec{
		OperationID: record.OperationID(), ReceiveIntentDigest: record.ReceiveIntentDigest(),
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(), FileRevision: record.FileRevision(), CanonicalPath: record.CanonicalPath(),
		ExactSize: record.ExactSize(), MaterializerKind: record.MaterializerKind(),
		AuthorityRef: record.AuthorityRef().Bytes(), OwnedObjectID: bytes.Repeat([]byte{0xa3}, sha256.Size),
		StateGeneration: 1, Phase: checkpointmodel.PhaseActive, CommitState: checkpointmodel.CommitCandidate,
	}
	conflicting, err := checkpointmodel.NewRecord(conflictingSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.indexRecord(conflicting); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("ambiguous index error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Store(canceled, nil, record); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled checkpoint store error = %v", err)
	}
	var nilStore *FileExecutionStore
	if _, err := nilStore.Store(context.Background(), nil, record); err == nil {
		t.Fatal("nil checkpoint store accepted an install")
	}

	faultRepository := repository
	faultRepository.records = &faultDirectory{
		Directory:       repository.records,
		createDirectory: func(string, bool) (outputcap.Directory, error) { return nil, errors.New("record shard unavailable") },
	}
	missingStore := &FileExecutionStore{
		repository: &faultRepository,
		records:    make(map[[sha256.Size]byte]checkpointmodel.Record),
	}
	observation, err := missingStore.Store(context.Background(), nil, record)
	if _, present := observation.Record(); present || err == nil {
		t.Fatalf("failed checkpoint install = (present=%t, %v)", present, err)
	}
}

func TestRepositoryReconcilePreservesEveryUntrustedImageAsAttention(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xb1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	record := checkpointRecordFixture(t, ownership, fixture, 0xb2)
	shardName, recordName := recordLocation(record.RecordID())
	shard := newMemoryDirectory()
	result := Snapshot{}

	t.Run("wrong kind stable image", func(t *testing.T) {
		fault := &exactFaultDirectory{Directory: shard, classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryDirectory, true, nil
		}}
		_, attention, err := repository.loadRecordImage(fault, shardName, recordName, record.RecordID())
		if err != nil || attention == nil || attention.Code() != AttentionCorruptRecord {
			t.Fatalf("wrong-kind stable image = (%+v, %v)", attention, err)
		}
	})

	t.Run("decode failure stable image", func(t *testing.T) {
		if err := InstallCreate(shard, recordName, []byte("corrupt")); err != nil {
			t.Fatal(err)
		}
		_, attention, err := repository.loadRecordImage(shard, shardName, recordName, record.RecordID())
		if err != nil || attention == nil || attention.Code() != AttentionCorruptRecord {
			t.Fatalf("corrupt stable image = (%+v, %v)", attention, err)
		}
		delete(shard.files, recordName)
	})

	t.Run("foreign stable binding", func(t *testing.T) {
		foreign := checkpointRecordFixture(t, ownership, fixture, 0xb3)
		encoded, encodeErr := checkpointmodel.EncodeRecord(foreign)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		if err := InstallCreate(shard, recordName, encoded); err != nil {
			t.Fatal(err)
		}
		_, attention, err := repository.loadRecordImage(shard, shardName, recordName, record.RecordID())
		if err != nil || attention == nil || attention.Code() != AttentionInvalidBinding {
			t.Fatalf("foreign stable image = (%+v, %v)", attention, err)
		}
		delete(shard.files, recordName)
	})

	t.Run("invalid candidate image", func(t *testing.T) {
		candidateName := TemporaryName(recordName, []byte("corrupt"), 0)
		if err := InstallCreate(shard, candidateName, []byte("corrupt")); err != nil {
			t.Fatal(err)
		}
		if err := repository.reconcileCandidate(
			shard, shardName, candidateName, map[string]storedRecord{}, map[string]struct{}{}, &result,
		); err != nil {
			t.Fatal(err)
		}
		if len(result.Attention()) == 0 || result.Attention()[len(result.Attention())-1].Code() != AttentionInvalidCandidate {
			t.Fatalf("invalid candidate attention = %+v", result.Attention())
		}
	})

	loaded := storedRecord{record: record, encoded: record.CanonicalBytes()}
	witnessFailure := errors.New("durability unknown")
	if _, err := reconcileCandidateRecord(shard, recordName, loaded, func(checkpointmodel.Record) (bool, error) {
		return false, witnessFailure
	}); !errors.Is(err, witnessFailure) {
		t.Fatalf("candidate witness error = %v", err)
	}
	retained, err := reconcileCandidateRecord(shard, recordName, loaded, func(checkpointmodel.Record) (bool, error) {
		return false, nil
	})
	if err != nil || retained.record.RecordID() != record.RecordID() {
		t.Fatalf("unwitnessed candidate = (%x, %v)", retained.record.RecordID(), err)
	}
}

func TestRepositoryScanBudgetBoundsOpaqueRecoveryWork(t *testing.T) {
	budget := newRepositoryScanBudget(1, 1)
	if limit, err := budget.namesLimit(); err != nil || limit != 3 {
		t.Fatalf("initial scan limit = (%d, %v)", limit, err)
	}
	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0xc1}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	shard, name := recordLocation(recordID)
	if err := budget.observe(shard, name); err != nil {
		t.Fatal(err)
	}
	if err := budget.observe(shard, "opaque"); err != nil {
		t.Fatal(err)
	}
	if err := budget.observe(shard, "another-opaque"); !errors.Is(err, checkpointmodel.ErrRecordRecovery) {
		t.Fatalf("auxiliary overflow error = %v", err)
	}
	invalid := newRepositoryScanBudget(0, 1)
	if _, err := invalid.namesLimit(); !errors.Is(err, checkpointmodel.ErrRecordRecovery) {
		t.Fatalf("invalid scan budget error = %v", err)
	}
	if err := invalid.observe(shard, name); !errors.Is(err, checkpointmodel.ErrRecordRecovery) {
		t.Fatalf("invalid scan observation error = %v", err)
	}

	listFailure := errors.New("shard list unavailable")
	faultRepository := Repository{records: &faultDirectory{
		Directory: newMemoryDirectory(), names: func(int) ([]string, error) { return nil, listFailure },
	}}
	if _, err := faultRepository.Reconcile(func(checkpointmodel.Record) (bool, error) { return false, nil }); !errors.Is(err, listFailure) {
		t.Fatalf("repository list error = %v", err)
	}
	if _, err := (*Repository)(nil).Reconcile(func(checkpointmodel.Record) (bool, error) { return false, nil }); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reconcile error = %v", err)
	}
}

func TestRecordReadBoundsRejectShortAndOversizedImages(t *testing.T) {
	directory := newMemoryDirectory()
	if _, err := ReadFile(directory, "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing record read error = %v", err)
	}
	file := &faultFile{File: &memoryFile{data: &memoryFileData{bytes: []byte{1}}}, readAt: func([]byte, int64) (int, error) {
		return 0, io.ErrUnexpectedEOF
	}}
	fault := &faultDirectory{Directory: directory, openFile: func(string, bool, bool) (outputcap.File, error) {
		return file, nil
	}}
	if _, err := ReadFile(fault, "record"); err == nil {
		t.Fatal("short record read succeeded")
	}
}

func TestCertifiedNamespaceReopensWithoutRepairingDurableAuthority(t *testing.T) {
	root, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xd1)
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := namespace.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenNamespace(CertifiedConfig{Root: root, Ownership: ownership})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	reopenedLease, err := reopened.AcquireOperation(
		fixture.intent.OperationID(), fixture.intent.Digest(), fixture.intent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedLease.Close() })
	reopenedRepository, err := reopenedLease.OpenExistingRepository()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedRepository.Close() })
	operation, err := reopenedRepository.ReadOperation()
	if err != nil || operation.OperationID() != fixture.intent.OperationID() {
		t.Fatalf("reopened operation = (%x, %v)", operation.OperationID(), err)
	}
	lifecycle, err := reopenedRepository.ReadLifecycleState()
	if err != nil || lifecycle.Phase() != checkpointmodel.LifecycleIntentFrozen {
		t.Fatalf("reopened lifecycle = (%d, %v)", lifecycle.Phase(), err)
	}
}

func TestCreateCollisionSettlesOnlyAnExactAuthenticatedTarget(t *testing.T) {
	install := func(t *testing.T, directory *memoryDirectory, name string, value []byte) outputcap.File {
		t.Helper()
		file, err := directory.CreateFile(name, true, int64(len(value)))
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteFile(file, value); err != nil {
			t.Fatal(err)
		}
		return file
	}

	for name, target := range map[string][]byte{
		"exact target":   []byte("authenticated"),
		"foreign target": []byte("substituted!!"),
	} {
		t.Run(name, func(t *testing.T) {
			directory := newMemoryDirectory()
			expected := []byte("authenticated")
			targetName := "record"
			targetFile := install(t, directory, targetName, target)
			if err := targetFile.Close(); err != nil {
				t.Fatal(err)
			}
			temporaryName := TemporaryName(targetName, expected, 0)
			temporary := install(t, directory, temporaryName, expected)
			err := settleCreateCollision(directory, targetName, expected, temporaryName, temporary)
			if name == "exact target" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, checkpointmodel.ErrRecordBinding) {
				t.Fatalf("foreign target collision error = %v", err)
			}
			if _, err := directory.OpenFile(temporaryName, true, false); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("settled candidate survived: %v", err)
			}
		})
	}
}

func TestCertifiedLayoutValidationRejectsInexactAndUnknownEntries(t *testing.T) {
	if _, err := openExistingDirectory(nil, "entry"); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil existing parent error = %v", err)
	}
	if _, err := openExistingDirectory(newMemoryDirectory(), "entry"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing existing directory error = %v", err)
	}

	for name, classify := range map[string]func(string) (outputcap.EntryKind, bool, error){
		"inexact": func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryDirectory, false, nil
		},
		"wrong kind": func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
		"classification failure": func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, errors.New("classification failed")
		},
	} {
		t.Run("open "+name, func(t *testing.T) {
			_, err := openExistingDirectory(&exactFaultDirectory{Directory: newMemoryDirectory(), classify: classify}, "entry")
			if err == nil {
				t.Fatal("unsafe existing directory opened")
			}
		})
	}

	allowed := map[string]outputcap.EntryKind{"known": outputcap.EntryDirectory}
	if err := validateAllowedEntries(nil, allowed); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil layout validation error = %v", err)
	}
	listFailure := errors.New("layout enumeration failed")
	if err := validateAllowedEntries(&faultDirectory{
		Directory: newMemoryDirectory(), names: func(int) ([]string, error) { return nil, listFailure },
	}, allowed); !errors.Is(err, listFailure) {
		t.Fatalf("layout enumeration error = %v", err)
	}
	if err := validateAllowedEntries(&faultDirectory{
		Directory: newMemoryDirectory(), names: func(int) ([]string, error) { return []string{"known", "extra"}, nil },
	}, allowed); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("layout overflow error = %v", err)
	}
	if err := validateAllowedEntries(&faultDirectory{
		Directory: newMemoryDirectory(), names: func(int) ([]string, error) { return []string{"unknown"}, nil },
	}, allowed); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unknown layout entry error = %v", err)
	}
	classifyFailure := errors.New("entry classification failed")
	if err := validateAllowedEntries(&faultDirectory{
		Directory: &exactFaultDirectory{
			Directory: newMemoryDirectory(), classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, false, classifyFailure
			},
		},
		names: func(int) ([]string, error) { return []string{"known"}, nil },
	}, allowed); !errors.Is(err, classifyFailure) {
		t.Fatalf("entry classification error = %v", err)
	}
	if err := validateAllowedEntries(&faultDirectory{
		Directory: &exactFaultDirectory{
			Directory: newMemoryDirectory(), classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryRegularFile, true, nil
			},
		},
		names: func(int) ([]string, error) { return []string{"known"}, nil },
	}, allowed); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("wrong layout kind error = %v", err)
	}
}

func TestOperationLeaseRequiresExactLiveCapabilities(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0xe1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	operation := fixture.intent.OperationID()
	intent := fixture.intent.Digest()
	binding := fixture.intent.BindingDigest()
	lock := &coverageLock{file: &memoryFile{data: &memoryFileData{}}}

	for name, configure := range map[string]func(*Namespace) error{
		"lock failure": func(candidate *Namespace) error {
			failure := errors.New("lease unavailable")
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return nil, false, failure },
			}
			return failure
		},
		"nil lock": func(candidate *Namespace) error {
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return nil, false, nil },
			}
			return outputcap.ErrUnsafeNamespace
		},
		"new lock sync failure": func(candidate *Namespace) error {
			failure := errors.New("lease sync failed")
			candidate.leases = &leaseFaultDirectory{
				Directory: candidate.leases, syncErr: failure,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, true, nil },
			}
			return failure
		},
		"operation duplicate failure": func(candidate *Namespace) error {
			failure := errors.New("operation capability unavailable")
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, false, nil },
			}
			candidate.operations = &leaseFaultDirectory{
				Directory: candidate.operations,
				duplicate: func() (outputcap.Directory, error) { return nil, failure },
			}
			return failure
		},
		"nil operation duplicate": func(candidate *Namespace) error {
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, false, nil },
			}
			candidate.operations = &leaseFaultDirectory{
				Directory: candidate.operations,
				duplicate: func() (outputcap.Directory, error) { return nil, nil },
			}
			return outputcap.ErrUnsafeNamespace
		},
		"lookup duplicate failure": func(candidate *Namespace) error {
			failure := errors.New("lookup capability unavailable")
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, false, nil },
			}
			candidate.lookup = &leaseFaultDirectory{
				Directory: candidate.lookup,
				duplicate: func() (outputcap.Directory, error) { return nil, failure },
			}
			return failure
		},
		"nil lookup duplicate": func(candidate *Namespace) error {
			candidate.leases = &leaseFaultDirectory{
				Directory:   candidate.leases,
				acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, false, nil },
			}
			candidate.lookup = &leaseFaultDirectory{
				Directory: candidate.lookup,
				duplicate: func() (outputcap.Directory, error) { return nil, nil },
			}
			return outputcap.ErrUnsafeNamespace
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := namespace
			want := configure(&candidate)
			if _, err := candidate.AcquireOperation(operation, intent, binding); !errors.Is(err, want) {
				t.Fatalf("lease acquisition error = %v, want %v", err, want)
			}
		})
	}
}

func TestCandidateRecoverySeparatesOrphansConflictsAndInitialCrashCuts(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xf1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	initialCandidate := checkpointRecordFixture(t, ownership, fixture, 0xf2)
	initial, err := checkpointmodel.PromoteInitialCandidate(initialCandidate)
	if err != nil {
		t.Fatal(err)
	}
	nextCandidate, err := checkpointmodel.AdvanceGeneration(
		initial, []checkpointmodel.Range{{Offset: 0, End: initial.ExactSize()}},
		checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
	)
	if err != nil {
		t.Fatal(err)
	}

	run := func(
		t *testing.T,
		candidate checkpointmodel.Record,
		stable map[string]storedRecord,
		occupied map[string]struct{},
	) (Snapshot, map[string]storedRecord) {
		t.Helper()
		shard := newMemoryDirectory()
		encoded, err := checkpointmodel.EncodeRecord(candidate)
		if err != nil {
			t.Fatal(err)
		}
		shardName, targetName := recordLocation(candidate.RecordID())
		for name, loaded := range stable {
			if err := InstallCreate(shard, name, loaded.encoded); err != nil {
				t.Fatal(err)
			}
		}
		temporaryName := TemporaryName(targetName, encoded, 0)
		if err := InstallCreate(shard, temporaryName, encoded); err != nil {
			t.Fatal(err)
		}
		result := Snapshot{}
		if err := repository.reconcileCandidate(
			shard, shardName, temporaryName, stable, occupied, &result,
		); err != nil {
			t.Fatal(err)
		}
		return result, stable
	}

	t.Run("orphaned later generation", func(t *testing.T) {
		result, _ := run(t, nextCandidate, map[string]storedRecord{}, map[string]struct{}{})
		if len(result.Attention()) != 1 || result.Attention()[0].Code() != AttentionOrphanedCandidate {
			t.Fatalf("orphan attention = %+v", result.Attention())
		}
	})

	t.Run("occupied unauthenticated target", func(t *testing.T) {
		_, targetName := recordLocation(initialCandidate.RecordID())
		result, _ := run(
			t, initialCandidate, map[string]storedRecord{}, map[string]struct{}{targetName: {}},
		)
		if len(result.Attention()) != 1 || result.Attention()[0].Code() != AttentionConflictingCandidate {
			t.Fatalf("occupied target attention = %+v", result.Attention())
		}
	})

	t.Run("initial candidate becomes the stable crash cut", func(t *testing.T) {
		_, stable := run(t, initialCandidate, map[string]storedRecord{}, map[string]struct{}{})
		_, targetName := recordLocation(initialCandidate.RecordID())
		if loaded, found := stable[targetName]; !found || loaded.record.RecordID() != initialCandidate.RecordID() {
			t.Fatalf("recovered initial candidate = (%+v, %t)", loaded, found)
		}
	})

	t.Run("exact duplicate candidate is retired", func(t *testing.T) {
		encoded, encodeErr := checkpointmodel.EncodeRecord(initialCandidate)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		_, targetName := recordLocation(initialCandidate.RecordID())
		stable := map[string]storedRecord{
			targetName: {record: initialCandidate, encoded: encoded},
		}
		result, _ := run(t, initialCandidate, stable, map[string]struct{}{targetName: {}})
		if len(result.Attention()) != 0 {
			t.Fatalf("exact duplicate attention = %+v", result.Attention())
		}
	})
}

func admittedDirectoryForRepositoryCoverage(
	t *testing.T,
	fixture operationFixture,
	seed byte,
) checkpointmodel.AdmittedDirectory {
	t.Helper()
	scope, err := transfer.NewDirectoryAdmissionScope(fixture.intent)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := catalog.DirectoryGenerationFromBytes(bytes.Repeat([]byte{seed}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		bytes.Repeat([]byte{seed + 1}, sha256.Size), scope,
		transfer.MaterializationDirectory{
			DirectoryID: fixture.intent.SyntheticRoot(), Generation: generation,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	owned, err := transfer.OwnedObjectIDFromBytes(
		bytes.Repeat([]byte{seed + 2}, transfer.OwnedObjectIdentityBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	record, err := checkpointmodel.NewAdmittedDirectory(fixture.intent, admission, owned)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAggregateRepositoryFailsClosedOnSubstitutedDirectoryAndLifecycleAuthority(t *testing.T) {
	_, namespace, lease, repository, _, fixture := openRepositoryFixture(t, 0xe1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	record := admittedDirectoryForRepositoryCoverage(t, fixture, 0xe2)
	if err := repository.InstallAdmittedDirectory(record); err != nil {
		t.Fatal(err)
	}

	var nilRepository *Repository
	if err := nilRepository.InstallAdmittedDirectory(record); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("nil admitted-directory install error = %v", err)
	}
	if _, err := nilRepository.ReadAdmittedDirectory(catalog.DirectoryID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil admitted-directory read error = %v", err)
	}
	missingDirectory, err := catalog.DirectoryIDFromBytes(bytes.Repeat([]byte{0xee}, catalog.IdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReadAdmittedDirectory(missingDirectory); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing admitted-directory error = %v", err)
	}

	manifest := repository.manifests.(*memoryDirectory)
	name := admittedDirectoryPrefix + bytesToHex(record.DirectoryID().Bytes())
	file := manifest.files[name]
	file.mu.Lock()
	original := bytes.Clone(file.bytes)
	file.bytes = []byte("corrupt admitted directory")
	file.mu.Unlock()
	if _, err := repository.ReadAdmittedDirectory(record.DirectoryID()); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("corrupt admitted-directory error = %v", err)
	}
	file.mu.Lock()
	file.bytes = original
	file.mu.Unlock()

	frozen, err := repository.ReadLifecycleState()
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceLifecycleState(checkpointmodel.ReceiveLifecycleState{}, frozen); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("invalid previous lifecycle error = %v", err)
	}
	if err := repository.ReplaceLifecycleState(frozen, checkpointmodel.ReceiveLifecycleState{}); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("invalid next lifecycle error = %v", err)
	}
	generationGap, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 3, Phase: checkpointmodel.LifecycleReceiving,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceLifecycleState(frozen, generationGap); errorCode(err) != ErrorUnsafeInstall {
		t.Fatalf("generation-gap lifecycle error = %v", err)
	}

	missingRecordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0xef}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	missingReference, err := checkpointmodel.FileCheckpointReferenceFromIdentity(missingRecordID, 1)
	if err != nil {
		t.Fatal(err)
	}
	receiving, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: fixture.intent.OperationID(), ReceiveIntent: fixture.intent.Digest(),
		StateGeneration: 2, Phase: checkpointmodel.LifecycleReceiving,
		CheckpointRefs: []checkpointmodel.FileCheckpointReference{missingReference}, SuccessCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplaceLifecycleState(frozen, receiving); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("missing checkpoint authority error = %v", err)
	}

	receipts := repository.receipts.(*memoryDirectory)
	lifecycleFile := receipts.files[lifecycleStateFile]
	lifecycleFile.mu.Lock()
	lifecycleImage := bytes.Clone(lifecycleFile.bytes)
	lifecycleFile.bytes = []byte("corrupt lifecycle")
	lifecycleFile.mu.Unlock()
	if _, err := repository.ReadLifecycleState(); errorCode(err) != ErrorCorruptRecord {
		t.Fatalf("corrupt lifecycle read error = %v", err)
	}
	lifecycleFile.mu.Lock()
	lifecycleFile.bytes = lifecycleImage
	lifecycleFile.mu.Unlock()
}

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
			repository: &repository, records: make(map[[sha256.Size]byte]checkpointmodel.Record),
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
		if _, _, err := store.CreateOwnedFile(nil, object, 4); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("nil context error = %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := store.CreateOwnedFile(ctx, object, 4); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled create error = %v", err)
		}
	})

	t.Run("existing object is a collision", func(t *testing.T) {
		store, _, object := newStore(t, 0x21)
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
		if err != nil || file == nil || observation.Condition() != fileexecution.OwnedReady {
			t.Fatalf("initial owned create = (%T, %d, %v)", file, observation.Condition(), err)
		}
		_ = file.Close()
		file, observation, err = store.CreateOwnedFile(context.Background(), object, 4)
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
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
				return &faultDirectory{Directory: stageShard, createFile: func(string, bool, int64) (outputcap.File, error) {
					return nil, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
				return &faultDirectory{Directory: anchorShard, linkFile: func(outputcap.File, string) (outputcap.File, error) {
					return nil, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
				return &faultDirectory{Directory: anchorShard, linkFile: func(source outputcap.File, name string) (outputcap.File, error) {
					linked, err := anchorShard.LinkFileNoReplace(source, name)
					if err != nil {
						return nil, err
					}
					return &ownedFaultFile{File: linked}, nil
				}}, nil
			},
		}
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
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
				return &faultDirectory{Directory: stageShard, createFile: func(string, bool, int64) (outputcap.File, error) {
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
		file, observation, err := store.CreateOwnedFile(context.Background(), object, 4)
		if file != nil || observation.Condition() != fileexecution.OwnedObjectCollision || !errors.Is(err, collision) {
			t.Fatalf("concurrent allocation = (%T, %d, %v)", file, observation.Condition(), err)
		}
	})
}
