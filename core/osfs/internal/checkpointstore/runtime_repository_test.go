package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestRuntimeClosureRepositoryCandidateDecisionsPreserveAmbiguity(t *testing.T) {
	t.Run("stable duplicate is removed", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x21)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x22)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Create(committed); err != nil {
			t.Fatal(err)
		}
		candidateName := runtimeClosureWriteCandidate(t, repository.records, committed, 1)
		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Records()) != 1 || len(snapshot.Attention()) != 0 {
			t.Fatalf("stable duplicate reconciliation = (%d, %v, %v)", len(snapshot.Records()), snapshot.Attention(), err)
		}
		runtimeClosureRequireMissingCandidate(t, repository.records, committed.RecordID(), candidateName)
	})

	t.Run("superseded candidate is removed", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x31)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x32)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Create(committed); err != nil {
			t.Fatal(err)
		}
		candidateName := runtimeClosureWriteCandidate(t, repository.records, initial, 2)
		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Records()) != 1 || len(snapshot.Attention()) != 0 {
			t.Fatalf("superseded candidate reconciliation = (%d, %v, %v)", len(snapshot.Records()), snapshot.Attention(), err)
		}
		runtimeClosureRequireMissingCandidate(t, repository.records, initial.RecordID(), candidateName)
	})

	t.Run("uncommitted successor remains attention state", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x41)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x42)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Create(committed); err != nil {
			t.Fatal(err)
		}
		pending, err := checkpointmodel.AdvanceGeneration(
			committed, []checkpointmodel.Range{{Offset: 0, End: 8}},
			checkpointmodel.PhaseActive, checkpointmodel.CommitCandidate,
		)
		if err != nil {
			t.Fatal(err)
		}
		candidateName := runtimeClosureWriteCandidate(t, repository.records, pending, 3)
		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Records()) != 1 || len(snapshot.Attention()) != 1 ||
			snapshot.Attention()[0].Code() != AttentionConflictingCandidate {
			t.Fatalf("ambiguous candidate reconciliation = (%d, %v, %v)", len(snapshot.Records()), snapshot.Attention(), err)
		}
		runtimeClosureRequirePresentCandidate(t, repository.records, pending.RecordID(), candidateName)
	})

	t.Run("orphaned committed candidate remains attention state", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x51)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x52)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		candidateName := runtimeClosureWriteCandidate(t, repository.records, committed, 4)
		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Records()) != 0 || len(snapshot.Attention()) != 1 ||
			snapshot.Attention()[0].Code() != AttentionOrphanedCandidate {
			t.Fatalf("orphaned candidate reconciliation = (%d, %v, %v)", len(snapshot.Records()), snapshot.Attention(), err)
		}
		runtimeClosureRequirePresentCandidate(t, repository.records, committed.RecordID(), candidateName)
	})
}

func TestRuntimeClosureCandidateInstallationReportsEveryDurabilityCut(t *testing.T) {
	target := "record.state"
	encoded := []byte("durable-record-image")

	t.Run("exact collision converges and closes returned capability", func(t *testing.T) {
		directory := newMemoryDirectory()
		closes := 0
		wrapped := &runtimeClosureLinkDirectory{
			Directory: directory,
			link: func(source outputcap.File, name string) (outputcap.File, error) {
				linked, err := directory.LinkFileNoReplace(source, name)
				if err != nil {
					return linked, err
				}
				return &runtimeClosureCloseFile{File: linked, close: func() error { closes++; return linked.Close() }},
					outputcap.ErrNamespaceCollision
			},
		}
		if err := InstallCreate(wrapped, target, encoded); err != nil || closes != 1 {
			t.Fatalf("exact collision = closes:%d error:%v", closes, err)
		}
		runtimeClosureRequireImage(t, directory, target, encoded)
	})

	t.Run("foreign collision is preserved", func(t *testing.T) {
		directory := newMemoryDirectory()
		foreign := []byte("foreign-image")
		wrapped := &runtimeClosureLinkDirectory{
			Directory: directory,
			link: func(_ outputcap.File, name string) (outputcap.File, error) {
				writeMemoryFile(t, directory, name, foreign)
				return nil, outputcap.ErrNamespaceCollision
			},
		}
		if err := InstallCreate(wrapped, target, encoded); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("foreign collision = %v", err)
		}
		runtimeClosureRequireImage(t, directory, target, foreign)
	})

	t.Run("linked target close failure is observable", func(t *testing.T) {
		directory := newMemoryDirectory()
		failure := errors.New("linked target close failure")
		wrapped := &runtimeClosureLinkDirectory{
			Directory: directory,
			link: func(source outputcap.File, name string) (outputcap.File, error) {
				linked, err := directory.LinkFileNoReplace(source, name)
				return &runtimeClosureCloseFile{File: linked, close: func() error { return failure }}, err
			},
		}
		if err := InstallCreate(wrapped, target, encoded); !errors.Is(err, failure) {
			t.Fatalf("target close cut = %v", err)
		}
		runtimeClosureRequireImage(t, directory, target, encoded)
	})

	t.Run("parent sync failure retains the exact target", func(t *testing.T) {
		directory := newMemoryDirectory()
		failure := errors.New("checkpoint parent sync failure")
		wrapped := &runtimeClosureSyncDirectory{Directory: directory, sync: func() error { return failure }}
		if err := InstallCreate(wrapped, target, encoded); !errors.Is(err, failure) {
			t.Fatalf("parent sync cut = %v", err)
		}
		runtimeClosureRequireImage(t, directory, target, encoded)
	})

	t.Run("candidate cleanup contention keeps restart evidence", func(t *testing.T) {
		directory := newMemoryDirectory()
		candidate := TemporaryName(target, encoded, 0)
		wrapped := &runtimeClosureRemoveDirectory{
			Directory: directory,
			remove: func(name string, expected outputcap.File) error {
				if name == candidate {
					return outputcap.ErrNamespaceLockBusy
				}
				return directory.RemoveFile(name, expected)
			},
		}
		if err := InstallCreate(wrapped, target, encoded); !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
			t.Fatalf("candidate cleanup cut = %v", err)
		}
		runtimeClosureRequireImage(t, directory, target, encoded)
		if _, err := ReadFile(directory, candidate); err != nil {
			t.Fatalf("candidate evidence was lost: %v", err)
		}
	})
}

func TestRuntimeClosureRepositoryMapsReopenReplaceAndRemoveOutcomes(t *testing.T) {
	t.Run("reopen rejects corrupt and foreign images", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x61)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)

		corrupt := checkpointRecordFixture(t, ownership, intent, 0x62)
		shardName, recordName := recordLocation(corrupt.RecordID())
		shard, err := OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, shard, recordName, []byte("not a checkpoint record"))
		_ = shard.Close()
		if _, err := repository.Reopen(corrupt.RecordID()); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("corrupt reopen = %v", err)
		}

		expected := checkpointRecordFixture(t, ownership, intent, 0x64)
		foreign := checkpointRecordFixture(t, ownership, intent, 0x66)
		encoded, err := checkpointmodel.EncodeRecord(foreign)
		if err != nil {
			t.Fatal(err)
		}
		shardName, recordName = recordLocation(expected.RecordID())
		shard, err = OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, shard, recordName, encoded)
		_ = shard.Close()
		if _, err := repository.Reopen(expected.RecordID()); errorCode(err) != ErrorCorruptRecord ||
			!errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("foreign reopen = %v", err)
		}
		if _, err := repository.Reopen(mustRecordID(t, 0xf1)); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing reopen = %v", err)
		}
	})

	t.Run("replace reports a close cut after installing exact bytes", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x71)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x72)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Create(initial); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("replacement shard close failure")
		base := repository.records
		repository.records = runtimeClosureOpenWithCloseFailure(base, failure)
		err = repository.Replace(initial, committed)
		repository.records = base
		if errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
			t.Fatalf("replace close cut = %v", err)
		}
		reopened, err := repository.Reopen(initial.RecordID())
		if err != nil || reopened.CommitState() != checkpointmodel.CommitVerified {
			t.Fatalf("installed replacement after close cut = (%v, %v)", reopened.CommitState(), err)
		}
	})

	t.Run("remove reports a close cut after deleting exact bytes", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x81)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		record := checkpointRecordFixture(t, ownership, intent, 0x82)
		if err := repository.Create(record); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("removal shard close failure")
		base := repository.records
		repository.records = runtimeClosureOpenWithCloseFailure(base, failure)
		err := repository.Remove(record)
		repository.records = base
		if errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
			t.Fatalf("remove close cut = %v", err)
		}
		if _, err := repository.Reopen(record.RecordID()); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("removed checkpoint reopened = %v", err)
		}
	})

	t.Run("record contracts fail before namespace mutation", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x91)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		first := checkpointRecordFixture(t, ownership, intent, 0x92)
		other := checkpointRecordFixture(t, ownership, intent, 0x94)
		if err := repository.Replace(first, other); errorCode(err) != ErrorUnsafeInstall {
			t.Fatalf("cross-record replacement = %v", err)
		}
		if err := repository.Replace(first, first); errorCode(err) != ErrorUnsafeInstall {
			t.Fatalf("non-transition replacement = %v", err)
		}
		foreignIntent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0xe1}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		foreign := checkpointRecordFixture(t, ownership, foreignIntent, 0x96)
		if err := repository.Create(foreign); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("foreign create = %v", err)
		}
		if err := repository.Remove(foreign); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("foreign remove = %v", err)
		}
		var nilRepository *Repository
		if _, err := nilRepository.Reopen(first.RecordID()); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("nil repository reopen = %v", err)
		}
	})
}

func TestRuntimeClosureRepositoryReconcileClassifiesImagesAndBoundaryFaults(t *testing.T) {
	t.Run("record images grant authority only when canonical and bound", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x21)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)

		wrongKind := checkpointRecordFixture(t, ownership, intent, 0x22)
		shardName, recordName := recordLocation(wrongKind.RecordID())
		shard, err := OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := shard.CreateDirectory(recordName, true); err != nil {
			t.Fatal(err)
		}
		_ = shard.Close()

		malformed := checkpointRecordFixture(t, ownership, intent, 0x24)
		shardName, recordName = recordLocation(malformed.RecordID())
		shard, err = OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, shard, recordName, []byte("malformed checkpoint"))
		_ = shard.Close()

		expected := checkpointRecordFixture(t, ownership, intent, 0x26)
		foreign := checkpointRecordFixture(t, ownership, intent, 0x28)
		encoded, err := checkpointmodel.EncodeRecord(foreign)
		if err != nil {
			t.Fatal(err)
		}
		shardName, recordName = recordLocation(expected.RecordID())
		shard, err = OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, shard, recordName, encoded)
		_ = shard.Close()

		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Records()) != 0 {
			t.Fatalf("record image reconciliation = (%d, %v)", len(snapshot.Records()), err)
		}
		codes := make(map[AttentionCode]int)
		for _, attention := range snapshot.Attention() {
			codes[attention.Code()]++
		}
		if codes[AttentionCorruptRecord] != 2 || codes[AttentionInvalidBinding] != 1 {
			t.Fatalf("record image attention = %v", snapshot.Attention())
		}
	})

	t.Run("scan, witness, and close failures retain their native cause", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x31)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		if _, err := repository.Reconcile(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("nil witness = %v", err)
		}

		failure := errors.New("repository scan failure")
		base := repository.records
		repository.records = &runtimeClosureNamesDirectory{
			Directory: base,
			names:     func(int) ([]string, error) { return nil, failure },
		}
		if _, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t)); errorCode(err) != ErrorStateIO ||
			!errors.Is(err, failure) {
			t.Fatalf("record root scan failure = %v", err)
		}
		repository.records = base

		initial := checkpointRecordFixture(t, ownership, intent, 0x32)
		if err := repository.Create(initial); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.Reconcile(func(checkpointmodel.Record) (bool, error) { return false, failure }); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
			t.Fatalf("initial witness failure = %v", err)
		}

		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Replace(initial, committed); err != nil {
			t.Fatal(err)
		}
		repository.records = runtimeClosureOpenWithCloseFailure(base, failure)
		if _, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t)); !errors.Is(err, failure) {
			t.Fatalf("shard close failure = %v", err)
		}
		repository.records = base
	})
}

func TestRuntimeClosureRecordIORejectsUnstableCapabilities(t *testing.T) {
	failure := errors.New("record capability failure")
	if _, err := ReadFile(nil, "record"); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("nil record directory = %v", err)
	}
	if _, err := ReadFile(&runtimeClosureClassifyDirectory{
		Directory: newMemoryDirectory(),
		classify:  func(string) (outputcap.EntryKind, bool, error) { return 0, false, failure },
	}, "record"); !errors.Is(err, failure) {
		t.Fatalf("record classification failure = %v", err)
	}
	wrongKind := newMemoryDirectory()
	if _, err := wrongKind.CreateDirectory("record", true); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(wrongKind, "record"); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("wrong-kind record = %v", err)
	}

	if _, err := readBoundedFile(nil); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("nil record file = %v", err)
	}
	baseFile := &memoryFile{data: &memoryFileData{bytes: []byte("record")}}
	if _, err := readBoundedFile(&runtimeClosureSizeFile{
		File: baseFile, size: func() (uint64, error) { return 0, failure },
	}); !errors.Is(err, failure) {
		t.Fatalf("record size failure = %v", err)
	}
	if _, err := readBoundedFile(&memoryFile{data: &memoryFileData{}}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("empty record file = %v", err)
	}
	if _, err := readBoundedFile(&runtimeClosureReadFile{
		File: baseFile,
		read: func(target []byte, _ int64) (int, error) {
			copy(target, []byte("rec"))
			return 3, io.EOF
		},
	}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short record read = %v", err)
	}

	openFailure := &runtimeClosureOpenFileDirectory{
		Directory: newMemoryDirectory(),
		open:      func(string, bool, bool) (outputcap.File, error) { return nil, failure },
	}
	if operationErr, cleanupErr := RemoveExact(openFailure, "record", []byte("image")); !errors.Is(operationErr, failure) || cleanupErr != nil {
		t.Fatalf("remove open failure = (%v, %v)", operationErr, cleanupErr)
	}
	if err := RemoveExactTemporary(newMemoryDirectory(), "missing", []byte("image")); err != nil {
		t.Fatalf("already-absent candidate = %v", err)
	}

	if err := reconcileExactCandidates(nil, "record", []byte("image")); !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
		t.Fatalf("invalid candidate reconciliation = %v", err)
	}
	if err := reconcileExactCandidates(&runtimeClosureClassifyDirectory{
		Directory: newMemoryDirectory(),
		classify:  func(string) (outputcap.EntryKind, bool, error) { return 0, false, failure },
	}, "record", []byte("image")); !errors.Is(err, failure) {
		t.Fatalf("candidate classification failure = %v", err)
	}
	candidateWrongKind := newMemoryDirectory()
	candidateName := TemporaryName("record", []byte("image"), 0)
	if _, err := candidateWrongKind.CreateDirectory(candidateName, true); err != nil {
		t.Fatal(err)
	}
	if err := reconcileExactCandidates(candidateWrongKind, "record", []byte("image")); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("wrong-kind candidate = %v", err)
	}
	if !candidateWriteInFlight([]byte{'a', 0, 0}, []byte("abc")) ||
		candidateWriteInFlight([]byte{'a', 0, 'x'}, []byte("abc")) {
		t.Fatal("candidate write-in-flight classification accepted non-zero divergence")
	}
}

func TestRuntimeClosureRepositoryFaultsPreserveOperationSemantics(t *testing.T) {
	failure := errors.New("repository capability failure")

	t.Run("CRUD rejects foreign bindings and missing durable locations", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x51)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		if err := (*Repository)(nil).Close(); err != nil {
			t.Fatalf("nil repository close = %v", err)
		}
		initial := checkpointRecordFixture(t, ownership, intent, 0x52)
		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		foreignIntent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0xa7}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		foreign := checkpointRecordFixture(t, ownership, foreignIntent, 0x54)
		if err := repository.Replace(foreign, committed); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("foreign predecessor = %v", err)
		}
		if err := repository.Replace(initial, foreign); errorCode(err) != ErrorCorruptRecord {
			t.Fatalf("foreign successor = %v", err)
		}
		if err := repository.Replace(initial, committed); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing replacement location = %v", err)
		}
		if err := repository.Remove(initial); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing removal location = %v", err)
		}

		base := repository.records
		repository.records = &runtimeClosureClassifyDirectory{
			Directory: base,
			classify:  func(string) (outputcap.EntryKind, bool, error) { return 0, false, failure },
		}
		if err := repository.Create(initial); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
			t.Fatalf("create shard authority failure = %v", err)
		}
		repository.records = base
	})

	t.Run("shard classification, open, and listing faults remain distinct", func(t *testing.T) {
		for _, test := range []struct {
			name string
			wrap func(outputcap.Directory, string) outputcap.Directory
		}{
			{
				name: "classify",
				wrap: func(base outputcap.Directory, shard string) outputcap.Directory {
					return &runtimeClosureClassifyDirectory{
						Directory: base,
						classify: func(name string) (outputcap.EntryKind, bool, error) {
							if name == shard {
								return 0, false, failure
							}
							return base.ClassifyExactEntry(name)
						},
					}
				},
			},
			{
				name: "open",
				wrap: func(base outputcap.Directory, shard string) outputcap.Directory {
					return &runtimeClosureOpenDirectory{
						Directory: base,
						open: func(name string, private bool) (outputcap.Directory, error) {
							if name == shard {
								return nil, failure
							}
							return base.OpenDirectory(name, private)
						},
					}
				},
			},
			{
				name: "list",
				wrap: func(base outputcap.Directory, shard string) outputcap.Directory {
					return &runtimeClosureOpenDirectory{
						Directory: base,
						open: func(name string, private bool) (outputcap.Directory, error) {
							child, err := base.OpenDirectory(name, private)
							if err != nil || name != shard {
								return child, err
							}
							return &runtimeClosureNamesDirectory{
								Directory: child,
								names:     func(int) ([]string, error) { return nil, failure },
							}, nil
						},
					}
				},
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x61)
				defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
				shard := "0a"
				created, err := repository.records.CreateDirectory(shard, true)
				if err != nil {
					t.Fatal(err)
				}
				_ = created.Close()
				repository.records = test.wrap(repository.records, shard)
				if _, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t)); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
					t.Fatalf("shard %s failure = %v", test.name, err)
				}
			})
		}
	})

	t.Run("candidate install and replacement faults preserve restart evidence", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x71)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		initial := checkpointRecordFixture(t, ownership, intent, 0x72)
		initialCandidate := runtimeClosureWriteCandidate(t, repository.records, initial, 5)
		shardName, _ := recordLocation(initial.RecordID())
		base := repository.records
		repository.records = runtimeClosureOpenWithLinkFailure(base, shardName, failure)
		if _, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t)); !errors.Is(err, failure) {
			t.Fatalf("initial candidate install failure = %v", err)
		}
		repository.records = base
		runtimeClosureRequirePresentCandidate(t, repository.records, initial.RecordID(), initialCandidate)

		committed, err := checkpointmodel.PromoteInitialCandidate(initial)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Create(committed); err != nil {
			t.Fatal(err)
		}
		next, err := checkpointmodel.AdvanceGeneration(
			committed, []checkpointmodel.Range{{Offset: 0, End: 4}},
			checkpointmodel.PhaseActive, checkpointmodel.CommitVerified,
		)
		if err != nil {
			t.Fatal(err)
		}
		nextCandidate := runtimeClosureWriteCandidate(t, repository.records, next, 6)
		repository.records = runtimeClosureOpenWithReplaceFailure(base, shardName, failure)
		if _, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t)); !errors.Is(err, failure) {
			t.Fatalf("replacement candidate install failure = %v", err)
		}
		repository.records = base
		runtimeClosureRequirePresentCandidate(t, repository.records, next.RecordID(), nextCandidate)
	})

	t.Run("wrong-kind and foreign candidates never become authority", func(t *testing.T) {
		_, namespace, lease, repository, ownership, intent := openRepositoryFixture(t, 0x81)
		defer runtimeClosureCloseRepository(t, &namespace, &lease, &repository)
		record := checkpointRecordFixture(t, ownership, intent, 0x82)
		encoded, err := checkpointmodel.EncodeRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		shardName, recordName := recordLocation(record.RecordID())
		shard, err := OpenShard(repository.records, shardName, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := shard.CreateDirectory(TemporaryName(recordName, encoded, 7), true); err != nil {
			t.Fatal(err)
		}
		_ = shard.Close()

		foreignIntent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0xb3}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		foreign := checkpointRecordFixture(t, ownership, foreignIntent, 0x84)
		runtimeClosureWriteCandidate(t, repository.records, foreign, 8)
		snapshot, err := repository.Reconcile(runtimeClosureUnexpectedWitness(t))
		if err != nil || len(snapshot.Attention()) != 2 {
			t.Fatalf("foreign candidate reconciliation = (%v, %v)", snapshot.Attention(), err)
		}
		for _, attention := range snapshot.Attention() {
			if attention.Code() != AttentionInvalidCandidate {
				t.Fatalf("candidate attention = %v", snapshot.Attention())
			}
		}
	})
}
