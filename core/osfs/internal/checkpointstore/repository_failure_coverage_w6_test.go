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
		if err := removeOwnedEntry(newMemoryDirectory(), object, ownedStageSuffix, true); err != nil {
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
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix, true); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
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
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix, true); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
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
		if err := removeOwnedEntry(rootFault, object, ownedStageSuffix, true); !errors.Is(err, changed) {
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
	file, observation, err := store.CreateOwnedFile(context.Background(), nil, object, 4)
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

	t.Run("unlinked next candidate is retired behind its committed predecessor", func(t *testing.T) {
		encoded, encodeErr := checkpointmodel.EncodeRecord(initial)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		_, targetName := recordLocation(initial.RecordID())
		stable := map[string]storedRecord{
			targetName: {record: initial, encoded: encoded},
		}
		result, recovered := run(t, nextCandidate, stable, map[string]struct{}{targetName: {}})
		if len(result.Attention()) != 0 ||
			recovered[targetName].record.CheckpointGeneration() != initial.CheckpointGeneration() {
			t.Fatalf("superseded candidate recovery = (%+v, %+v)", result.Attention(), recovered[targetName])
		}
	})

	nextVerified, err := checkpointmodel.Promote(
		nextCandidate,
		checkpointmodel.PhaseActive,
		checkpointmodel.CommitVerified,
	)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := checkpointmodel.AdvanceState(
		initial,
		initial.StateGeneration()+1,
		checkpointmodel.PhasePaused,
		checkpointmodel.CommitVerified,
		0,
		0,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("committed lifecycle state replaces its predecessor", func(t *testing.T) {
		encoded, encodeErr := checkpointmodel.EncodeRecord(initial)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		_, targetName := recordLocation(initial.RecordID())
		stable := map[string]storedRecord{
			targetName: {record: initial, encoded: encoded},
		}
		result, recovered := run(t, paused, stable, map[string]struct{}{targetName: {}})
		if len(result.Attention()) != 0 ||
			recovered[targetName].record.Phase() != checkpointmodel.PhasePaused {
			t.Fatalf("committed candidate recovery = (%+v, %+v)", result.Attention(), recovered[targetName])
		}
	})

	t.Run("invalid committed generation is preserved as conflict", func(t *testing.T) {
		encoded, encodeErr := checkpointmodel.EncodeRecord(initial)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		_, targetName := recordLocation(initial.RecordID())
		stable := map[string]storedRecord{
			targetName: {record: initial, encoded: encoded},
		}
		result, _ := run(t, nextVerified, stable, map[string]struct{}{targetName: {}})
		if len(result.Attention()) != 1 ||
			result.Attention()[0].Code() != AttentionConflictingCandidate {
			t.Fatalf("divergent candidate attention = %+v", result.Attention())
		}
	})
}

func TestCandidateRecoveryClassifiesExactNamespaceAndReadHazards(t *testing.T) {
	_, namespace, lease, repository, ownership, fixture := openRepositoryFixture(t, 0xf8)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = namespace.Close()
	})
	record := checkpointRecordFixture(t, ownership, fixture, 0xf9)
	encoded, err := checkpointmodel.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	shardName, targetName := recordLocation(record.RecordID())
	candidateName := TemporaryName(targetName, encoded, 0)

	run := func(
		t *testing.T,
		directory outputcap.Directory,
		wantAttention bool,
		wantErr error,
	) {
		t.Helper()
		result := Snapshot{}
		err := repository.reconcileCandidate(
			directory,
			shardName,
			candidateName,
			map[string]storedRecord{},
			map[string]struct{}{},
			&result,
		)
		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Fatalf("candidate recovery error = %v, want %v", err, wantErr)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if got := len(result.Attention()) != 0; got != wantAttention {
			t.Fatalf("attention = %+v, want present %t", result.Attention(), wantAttention)
		}
	}

	t.Run("candidate disappeared after bounded enumeration", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: newMemoryDirectory(),
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, true, nil
			},
		}, false, nil)
	})
	t.Run("candidate entry is inexact", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: newMemoryDirectory(),
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryRegularFile, false, nil
			},
		}, true, nil)
	})
	t.Run("candidate entry has the wrong kind", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: newMemoryDirectory(),
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryDirectory, true, nil
			},
		}, true, nil)
	})
	classifyFailure := errors.New("candidate classification failed")
	t.Run("candidate classification fails", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: newMemoryDirectory(),
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, false, classifyFailure
			},
		}, false, classifyFailure)
	})
	t.Run("unsafe candidate read becomes attention", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: &faultDirectory{
				Directory: newMemoryDirectory(),
				openFile: func(string, bool, bool) (outputcap.File, error) {
					return nil, outputcap.ErrUnsafeNamespace
				},
			},
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryRegularFile, true, nil
			},
		}, true, nil)
	})
	readFailure := errors.New("candidate read failed")
	t.Run("candidate state IO is surfaced", func(t *testing.T) {
		run(t, &exactFaultDirectory{
			Directory: &faultDirectory{
				Directory: newMemoryDirectory(),
				openFile: func(string, bool, bool) (outputcap.File, error) {
					return nil, readFailure
				},
			},
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryRegularFile, true, nil
			},
		}, false, readFailure)
	})

	foreignIntent, err := transfer.ReceiveIntentDigestFromBytes(bytes.Repeat(
		[]byte{0xfa},
		transfer.ReceiveIntentDigestBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  record.OperationID(),
		ReceiveIntentDigest:          foreignIntent,
		MaterializationBindingDigest: record.MaterializationBindingDigest(),
		FileID:                       record.FileID(),
		FileRevision:                 record.FileRevision(),
		CanonicalPath:                record.CanonicalPath(),
		ExactSize:                    record.ExactSize(),
		MaterializerKind:             record.MaterializerKind(),
		AuthorityRef:                 record.AuthorityRef().Bytes(),
		OwnedObjectID:                record.OwnedObjectID().Bytes(),
		StateGeneration:              record.StateGeneration(),
		CheckpointGeneration:         record.CheckpointGeneration(),
		VerifiedRanges:               record.VerifiedRanges(),
		Phase:                        record.Phase(),
		CommitState:                  record.CommitState(),
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignEncoded, err := checkpointmodel.EncodeRecord(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignShard, foreignTarget := recordLocation(foreign.RecordID())
	foreignName := TemporaryName(foreignTarget, foreignEncoded, 0)
	directory := newMemoryDirectory()
	if err := InstallCreate(directory, foreignName, foreignEncoded); err != nil {
		t.Fatal(err)
	}
	result := Snapshot{}
	if err := repository.reconcileCandidate(
		directory,
		foreignShard,
		foreignName,
		map[string]storedRecord{},
		map[string]struct{}{},
		&result,
	); err != nil {
		t.Fatal(err)
	}
	if len(result.Attention()) != 1 ||
		result.Attention()[0].Code() != AttentionInvalidCandidate {
		t.Fatalf("foreign candidate attention = %+v", result.Attention())
	}
}
