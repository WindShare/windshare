package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"slices"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOpenOwnsAnIntentNamespaceAndSerializesRuntimeClaims(t *testing.T) {
	root := newMemoryDirectory()
	backend, err := transfer.NewOutputBackendID("checkpointstore-test")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x31}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	config := Config{
		Root:         root,
		BackendID:    backend,
		RootIdentity: bytes.Repeat([]byte{0x41}, sha256.Size),
		Intent:       intent,
	}
	if err := BootstrapOwnership(NamespaceConfig{
		Root: root, BackendID: backend, RootIdentity: config.RootIdentity,
	}); err != nil {
		t.Fatal(err)
	}

	claim, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Intent == nil || claim.Records == nil || claim.Anchors == nil || claim.Stages == nil || claim.Lock == nil {
		t.Fatal("Open() returned a partial checkpoint authority")
	}
	if _, err := Open(config); !errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		t.Fatalf("concurrent Open() error = %v, want %v", err, outputcap.ErrNamespaceLockBusy)
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatalf("idempotent Close() = %v", err)
	}
	if err := (*Claim)(nil).Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open() after releasing runtime claim = %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	mismatch := config
	mismatch.RootIdentity = bytes.Repeat([]byte{0x42}, sha256.Size)
	if _, err := Open(mismatch); !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
		t.Fatalf("ownership mismatch error = %v", err)
	}
}

func TestOpenRejectsIncompleteBindingsAndNeverBootstrapsOwnership(t *testing.T) {
	root := newMemoryDirectory()
	backend, err := transfer.NewOutputBackendID("checkpointstore-test")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x51}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	valid := Config{Root: root, BackendID: backend, RootIdentity: bytes.Repeat([]byte{0x52}, sha256.Size), Intent: intent}
	for _, config := range []Config{
		{},
		{Root: root, BackendID: backend, RootIdentity: valid.RootIdentity},
		{Root: root, BackendID: backend, RootIdentity: []byte{1}, Intent: intent},
		{Root: root, BackendID: "", RootIdentity: valid.RootIdentity, Intent: intent},
	} {
		if _, err := Open(config); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("Open(%+v) error = %v", config, err)
		}
	}

	if _, err := Open(valid); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open() without cleaner-established ownership = %v", err)
	}
	status, err := InspectOwnership(NamespaceConfig{
		Root: root, BackendID: backend, RootIdentity: valid.RootIdentity,
	})
	if err != nil || status != OwnershipAbsent {
		t.Fatalf("Open() manufactured ownership: status=%d err=%v", status, err)
	}
}

func TestOpenFailsClosedAtEveryNamespaceBoundary(t *testing.T) {
	backend, err := transfer.NewOutputBackendID("checkpointstore-test")
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x61}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := resumestate.NewFileCheckpointOwnership(string(backend), bytes.Repeat([]byte{0x62}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	encodedOwnership, err := resumestate.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		id   string
		name string
	}{
		{id: "control", name: resumestate.ControlDirectoryName},
		{id: "checkpoint-root", name: resumestate.CheckpointsDirectoryName},
		{id: "intents", name: IntentsDirectory},
		{id: "intent", name: resumestate.IntentNamespaceName(intent)},
		{id: "records", name: RecordsDirectory},
		{id: "anchors", name: resumestate.AnchorsDirectoryName},
		{id: "stages", name: resumestate.StagesDirectoryName},
	}
	for blockedIndex, blocked := range steps {
		t.Run(blocked.id, func(t *testing.T) {
			root := newMemoryDirectory()
			parent := root
			for index, step := range steps {
				if index == blockedIndex {
					writeMemoryFile(t, parent, step.name, []byte("regular file blocks directory authority"))
					break
				}
				created, createErr := parent.CreateDirectory(step.name, true)
				if createErr != nil {
					t.Fatalf("create fixture directory %q: %v", step.name, createErr)
				}
				createdDirectory := created.(*memoryDirectory)
				if step.id == "checkpoint-root" {
					writeMemoryFile(t, createdDirectory, OwnershipFile, encodedOwnership)
				}
				// The first four directories form the ownership spine. Records,
				// anchors, and stages are siblings owned by the intent.
				if index < 4 {
					parent = createdDirectory
				}
			}
			claim, openErr := Open(Config{
				Root: root, BackendID: backend, RootIdentity: bytes.Repeat([]byte{0x62}, sha256.Size), Intent: intent,
			})
			if openErr == nil {
				t.Fatalf("Open succeeded with %s namespace occupied by a regular file", blocked.id)
			}
			if claim.Intent != nil || claim.Records != nil || claim.Anchors != nil || claim.Stages != nil || claim.Lock != nil {
				t.Fatalf("Open leaked a partial claim after %s failure: %+v", blocked.id, claim)
			}
		})
	}
}

func TestRecordStoreCreatesReadsAndReplacesAuthenticatedImages(t *testing.T) {
	root := newMemoryDirectory()
	records, err := OpenOrCreateRecordsDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened, err := OpenOrCreateRecordsDirectory(root); err != nil {
		t.Fatal(err)
	} else if same, _ := records.SameDirectory(reopened); !same {
		t.Fatal("records namespace reopened a different directory")
	}
	if _, err := OpenOrCreateRecordsDirectory(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil records parent error = %v", err)
	}

	if ValidShard("0g") || ValidShard("a") || !ValidShard("0f") {
		t.Fatal("checkpoint shard validation changed")
	}
	if _, err := OpenShard(records, "0f", false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing shard error = %v", err)
	}
	shard, err := OpenShard(records, "0f", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShard(records, "0f", false); err != nil {
		t.Fatalf("reopen shard = %v", err)
	}
	if _, err := OpenShard(nil, "0f", true); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil shard parent error = %v", err)
	}
	if _, err := OpenShard(records, "xx", true); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("invalid shard error = %v", err)
	}

	first := []byte("checkpoint-one")
	second := []byte("checkpoint-two")
	if err := InstallCreate(shard, "record.state", first); err != nil {
		t.Fatal(err)
	}
	if err := InstallCreate(shard, "record.state", first); err != nil {
		t.Fatalf("idempotent create = %v", err)
	}
	if err := InstallCreate(shard, "record.state", second); !errors.Is(err, resumestate.ErrFileCheckpointBinding) {
		t.Fatalf("conflicting create error = %v", err)
	}
	if encoded, err := ReadFile(shard, "record.state"); err != nil || !bytes.Equal(encoded, first) {
		t.Fatalf("ReadFile() = %q, %v", encoded, err)
	}
	if err := InstallReplace(shard, "record.state", []byte("stale"), second); !errors.Is(err, resumestate.ErrFileCheckpointBinding) {
		t.Fatalf("stale replace error = %v", err)
	}
	if err := InstallReplace(shard, "record.state", first, second); err != nil {
		t.Fatal(err)
	}
	if encoded, err := ReadFile(shard, "record.state"); err != nil || !bytes.Equal(encoded, second) {
		t.Fatalf("replaced ReadFile() = %q, %v", encoded, err)
	}
	if operationErr, cleanupErr := RemoveExact(shard, "record.state", first); !errors.Is(operationErr, resumestate.ErrFileCheckpointBinding) || cleanupErr != nil {
		t.Fatalf("stale RemoveExact() = %v, %v", operationErr, cleanupErr)
	}
	if encoded, err := ReadFile(shard, "record.state"); err != nil || !bytes.Equal(encoded, second) {
		t.Fatalf("stale removal changed checkpoint = %q, %v", encoded, err)
	}
	if operationErr, cleanupErr := RemoveExact(shard, "record.state", second); operationErr != nil || cleanupErr != nil {
		t.Fatalf("RemoveExact() = %v, %v", operationErr, cleanupErr)
	}
	if _, err := ReadFile(shard, "record.state"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed checkpoint remains readable: %v", err)
	}
	if operationErr, cleanupErr := RemoveExact(nil, "record.state", second); !errors.Is(operationErr, resumestate.ErrInvalidFileCheckpoint) || cleanupErr != nil {
		t.Fatalf("invalid RemoveExact() = %v, %v", operationErr, cleanupErr)
	}

	temporaryName := TemporaryName("record.state", first, 2)
	if temporaryName != TemporaryName("record.state", first, 2) ||
		temporaryName == TemporaryName("record.state", second, 2) {
		t.Fatal("temporary names are not deterministic and content-bound")
	}
	for _, err := range []error{
		InstallCreate(nil, "record.state", first),
		InstallCreate(shard, "", first),
		InstallCreate(shard, "record.state", nil),
		InstallReplace(nil, "record.state", first, second),
		InstallReplace(shard, "", first, second),
		InstallReplace(shard, "record.state", first, nil),
		WriteFile(nil, first),
	} {
		if !errors.Is(err, resumestate.ErrInvalidFileCheckpoint) {
			t.Fatalf("invalid record operation error = %v", err)
		}
	}
	if _, err := ReadFile(shard, "missing.state"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing ReadFile() error = %v", err)
	}
}

func TestRecordStoreResumesOnlyExactDeterministicCandidates(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		directory := newMemoryDirectory()
		encoded := []byte("create-image")
		candidate := TemporaryName("record.state", encoded, 0)
		if !IsTemporaryName(candidate) || IsTemporaryName("record.state") ||
			!MatchesTemporaryName(candidate, "record.state", encoded) ||
			MatchesTemporaryName(candidate, "record.state", []byte("different-image")) {
			t.Fatal("deterministic candidate classification accepted an inexact binding")
		}
		writeMemoryFile(t, directory, candidate, encoded)

		if err := InstallCreate(directory, "record.state", encoded); err != nil {
			t.Fatal(err)
		}
		if actual, err := ReadFile(directory, "record.state"); err != nil || !bytes.Equal(actual, encoded) {
			t.Fatalf("resumed create image = %q, %v", actual, err)
		}
		if _, err := ReadFile(directory, candidate); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("resumed create retained candidate: %v", err)
		}
	})

	t.Run("exact-removal", func(t *testing.T) {
		directory := newMemoryDirectory()
		encoded := []byte("removable-image")
		candidate := TemporaryName("record.state", encoded, 0)
		writeMemoryFile(t, directory, candidate, encoded)

		if err := RemoveExactTemporary(directory, candidate, encoded); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFile(directory, candidate); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("exact candidate remains after removal: %v", err)
		}
	})

	t.Run("replace", func(t *testing.T) {
		directory := newMemoryDirectory()
		previous := []byte("previous-image")
		next := []byte("replacement-image")
		writeMemoryFile(t, directory, "record.state", previous)
		candidate := TemporaryName("record.state", next, 0)
		writeMemoryFile(t, directory, candidate, next)

		if err := InstallReplace(directory, "record.state", previous, next); err != nil {
			t.Fatal(err)
		}
		if actual, err := ReadFile(directory, "record.state"); err != nil || !bytes.Equal(actual, next) {
			t.Fatalf("resumed replacement image = %q, %v", actual, err)
		}
		if _, err := ReadFile(directory, candidate); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("resumed replacement retained candidate: %v", err)
		}
	})

	t.Run("mismatched-image", func(t *testing.T) {
		directory := newMemoryDirectory()
		expected := []byte("expected-image")
		candidate := TemporaryName("record.state", expected, 0)
		writeMemoryFile(t, directory, candidate, []byte("foreign-image"))

		if err := InstallCreate(directory, "record.state", expected); !errors.Is(err, resumestate.ErrFileCheckpointBinding) {
			t.Fatalf("mismatched candidate error = %v", err)
		}
		if _, err := ReadFile(directory, "record.state"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("mismatched candidate installed target: %v", err)
		}
	})
}

func TestOwnershipBootstrapResumesExactDeterministicCandidate(t *testing.T) {
	root := newMemoryDirectory()
	backend, err := transfer.NewOutputBackendID("checkpointstore-test")
	if err != nil {
		t.Fatal(err)
	}
	config := NamespaceConfig{
		Root: root, BackendID: backend, RootIdentity: bytes.Repeat([]byte{0x91}, sha256.Size),
	}
	control, err := openOrCreateDirectory(root, resumestate.ControlDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRoot, err := openOrCreateDirectory(control, resumestate.CheckpointsDirectoryName)
	if err != nil {
		t.Fatal(err)
	}
	ownership, err := namespaceOwnership(config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	candidate := TemporaryName(OwnershipFile, encoded, 0)
	writeMemoryFile(t, checkpointRoot, candidate, encoded)

	status, err := InspectOwnership(config)
	if err != nil || status != OwnershipRecoverable {
		t.Fatalf("candidate ownership status = %d, %v", status, err)
	}
	if err := BootstrapOwnership(config); err != nil {
		t.Fatal(err)
	}
	status, err = InspectOwnership(config)
	if err != nil || status != OwnershipMatched {
		t.Fatalf("resumed ownership status = %d, %v", status, err)
	}
	if _, err := ReadFile(checkpointRoot, candidate); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resumed ownership retained candidate: %v", err)
	}
}

func TestRecordStoreMapsRecordIDsAndCleansTemporaryFiles(t *testing.T) {
	records := newMemoryDirectory()
	recordID, err := resumestate.FileCheckpointRecordIDFromBytes(bytes.Repeat([]byte{0x71}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	shard, name, err := FileFor(records, recordID, true)
	if err != nil || shard == nil || name == "" {
		t.Fatalf("FileFor() = %v, %q, %v", shard, name, err)
	}
	if _, reopenedName, err := FileFor(records, recordID, false); err != nil || reopenedName != name {
		t.Fatalf("reopened FileFor() name = %q, %v", reopenedName, err)
	}

	temporary, err := shard.CreateFile("temporary", true, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(temporary, []byte("tmp")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemporary(shard, "temporary", temporary); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTemporary(nil, "temporary", temporary); err != nil {
		t.Fatalf("nil RemoveTemporary() = %v", err)
	}
	if err := RemoveTemporary(shard, "temporary", nil); err != nil {
		t.Fatalf("nil-file RemoveTemporary() = %v", err)
	}
	if err := WriteFile(shortWriteFile{}, []byte("short")); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("short WriteFile() error = %v", err)
	}
}

func TestDirectoryCreationPreservesCollisionAndFailureBoundaries(t *testing.T) {
	if _, err := openOrCreateDirectory(nil, "child"); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil parent error = %v", err)
	}
	if _, err := openOrCreateDirectory(newMemoryDirectory(), ""); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("empty name error = %v", err)
	}

	failure := errors.New("directory operation failed")
	openedOnError := newMemoryDirectory()
	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return openedOnError, failure
		},
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("open failure = %v", err)
	}

	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return nil, fs.ErrNotExist
		},
		createDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), nil
		},
		sync: func() error { return failure },
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("parent sync failure = %v", err)
	}

	collisionChild := newMemoryDirectory()
	openCalls := 0
	reopened, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			openCalls++
			if openCalls == 1 {
				return nil, fs.ErrNotExist
			}
			return collisionChild, nil
		},
		createDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), outputcap.ErrNamespaceCollision
		},
	}, "child")
	if err != nil || reopened != collisionChild {
		t.Fatalf("collision reopen = %v, %v", reopened, err)
	}

	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return nil, fs.ErrNotExist
		},
		createDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), failure
		},
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("create failure = %v", err)
	}
}

func TestOwnershipMarkerRejectsAmbiguousInstallCuts(t *testing.T) {
	failure := errors.New("marker operation failed")
	ownership, err := resumestate.NewFileCheckpointOwnership("checkpointstore-test", bytes.Repeat([]byte{0x81}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeFileCheckpointOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnership(newMemoryDirectory(), resumestate.FileCheckpointOwnership{}, true); err == nil {
		t.Fatal("zero ownership marker was accepted")
	}
	if err := ensureOwnership(&faultDirectory{
		Directory: newMemoryDirectory(),
		observeEntry: func(string) (outputcap.EntryKind, error) {
			return outputcap.EntryAbsent, failure
		},
	}, ownership, true); !errors.Is(err, failure) {
		t.Fatalf("marker observation failure = %v", err)
	}
	wrongKind := newMemoryDirectory()
	if _, err := wrongKind.CreateDirectory(OwnershipFile, true); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnership(wrongKind, ownership, true); !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
		t.Fatalf("non-file marker error = %v", err)
	}
	malformed := newMemoryDirectory()
	writeMemoryFile(t, malformed, OwnershipFile, []byte("malformed"))
	if err := ensureOwnership(malformed, ownership, true); !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
		t.Fatalf("malformed marker error = %v", err)
	}
	if err := ensureOwnership(&faultDirectory{
		Directory: newMemoryDirectory(),
		observeEntry: func(string) (outputcap.EntryKind, error) {
			return outputcap.EntryRegularFile, nil
		},
	}, ownership, true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unreadable marker error = %v", err)
	}

	for _, test := range []struct {
		name      string
		directory outputcap.Directory
		want      error
	}{
		{
			name: "allocation exhausted",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return nil, outputcap.ErrNamespaceCollision
			}},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "create failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return nil, failure
			}},
			want: failure,
		},
		{
			name: "create contention",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return nil, outputcap.ErrFixedLinkSourceChanged
			}},
			want: outputcap.ErrNamespaceLockBusy,
		},
		{
			name: "write failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return &faultFile{writeAt: func([]byte, int64) (int, error) { return 0, failure }}, nil
			}},
			want: failure,
		},
		{
			name: "short write",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return &faultFile{writeAt: func(encoded []byte, _ int64) (int, error) { return len(encoded) - 1, nil }}, nil
			}},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "link failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), linkFile: func(outputcap.File, string) (outputcap.File, error) {
				return nil, failure
			}},
			want: failure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := installOwnedFile(test.directory, OwnershipFile, encoded); !errors.Is(err, test.want) {
				t.Fatalf("installOwnedFile() error = %v, want %v", err, test.want)
			}
		})
	}

	idempotent := newMemoryDirectory()
	writeMemoryFile(t, idempotent, OwnershipFile, encoded)
	if err := installOwnedFile(idempotent, OwnershipFile, encoded); err != nil {
		t.Fatalf("idempotent collision = %v", err)
	}
	conflict := newMemoryDirectory()
	writeMemoryFile(t, conflict, OwnershipFile, []byte("different"))
	if err := installOwnedFile(conflict, OwnershipFile, encoded); !errors.Is(err, resumestate.ErrFileCheckpointOwnership) {
		t.Fatalf("conflicting collision = %v", err)
	}
	if err := RemoveTemporary(nil, "candidate", &memoryFile{data: &memoryFileData{}}); err != nil {
		t.Fatalf("nil private cleanup = %v", err)
	}
	if err := RemoveTemporary(newMemoryDirectory(), "absent-candidate", &memoryFile{data: &memoryFileData{}}); err != nil {
		t.Fatalf("idempotent absent cleanup = %v", err)
	}
	if err := closeFile(nil); err != nil {
		t.Fatalf("nil file close = %v", err)
	}
}

func TestRecordInstallRejectsUnverifiedNamespaceOutcomes(t *testing.T) {
	failure := errors.New("record operation failed")
	encoded := []byte("record-image")
	for _, test := range []struct {
		name      string
		directory outputcap.Directory
		want      error
	}{
		{
			name: "allocation exhausted",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return nil, outputcap.ErrNamespaceCollision
			}},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "create failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return nil, failure
			}},
			want: failure,
		},
		{
			name: "write failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), createFile: func(string, bool, int64) (outputcap.File, error) {
				return &faultFile{writeAt: func([]byte, int64) (int, error) { return 0, failure }}, nil
			}},
			want: failure,
		},
		{
			name: "link failure",
			directory: &faultDirectory{Directory: newMemoryDirectory(), linkFile: func(outputcap.File, string) (outputcap.File, error) {
				return nil, failure
			}},
			want: failure,
		},
		{
			name: "link contention",
			directory: &faultDirectory{Directory: newMemoryDirectory(), linkFile: func(outputcap.File, string) (outputcap.File, error) {
				return nil, outputcap.ErrFixedLinkSourceChanged
			}},
			want: outputcap.ErrNamespaceLockBusy,
		},
		{
			name: "unverified link",
			directory: &faultDirectory{Directory: newMemoryDirectory(), linkFile: func(source outputcap.File, _ string) (outputcap.File, error) {
				return source, nil
			}},
			want: resumestate.ErrFileCheckpointCrashBoundary,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := InstallCreate(test.directory, "record.state", encoded); !errors.Is(err, test.want) {
				t.Fatalf("InstallCreate() error = %v, want %v", err, test.want)
			}
		})
	}

	base := newMemoryDirectory()
	writeMemoryFile(t, base, "record.state", encoded)
	failedReplace := &faultDirectory{
		Directory: base,
		replaceFile: func(outputcap.File, string) error {
			return failure
		},
	}
	if err := InstallReplace(failedReplace, "record.state", encoded, []byte("next-image")); !errors.Is(err, failure) || !errors.Is(err, resumestate.ErrFileCheckpointCrashBoundary) {
		t.Fatalf("failed replacement = %v", err)
	}

	adopted := newMemoryDirectory()
	writeMemoryFile(t, adopted, "record.state", encoded)
	adoptedWithError := &faultDirectory{
		Directory: adopted,
		replaceFile: func(source outputcap.File, name string) error {
			if err := adopted.ReplacePrivateFile(source, name); err != nil {
				return err
			}
			return failure
		},
	}
	if err := InstallReplace(adoptedWithError, "record.state", encoded, []byte("next-image")); !errors.Is(err, failure) {
		t.Fatalf("adopted replacement error = %v", err)
	}
	if _, _, err := FileFor(nil, mustRecordID(t, 0x91), true); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil FileFor() error = %v", err)
	}
}

func TestRecordReplacementPreservesEveryPreInstallFailure(t *testing.T) {
	failure := errors.New("replacement precondition failed")
	previous := []byte("previous-image")
	next := []byte("next-image")
	if _, err := OpenShard(&faultDirectory{
		Directory: newMemoryDirectory(),
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), failure
		},
	}, "0a", false); !errors.Is(err, failure) {
		t.Fatalf("shard open failure = %v", err)
	}
	if err := RemoveTemporary(newMemoryDirectory(), "missing", &memoryFile{data: &memoryFileData{}}); err != nil {
		t.Fatalf("already removed temporary = %v", err)
	}
	if err := InstallReplace(newMemoryDirectory(), "missing.state", previous, next); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing replacement target = %v", err)
	}

	for _, test := range []struct {
		name      string
		directory func(*memoryDirectory) outputcap.Directory
		want      error
	}{
		{
			name: "allocation exhausted",
			directory: func(base *memoryDirectory) outputcap.Directory {
				return &faultDirectory{Directory: base, createFile: func(string, bool, int64) (outputcap.File, error) {
					return nil, outputcap.ErrNamespaceCollision
				}}
			},
			want: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "create failure",
			directory: func(base *memoryDirectory) outputcap.Directory {
				return &faultDirectory{Directory: base, createFile: func(string, bool, int64) (outputcap.File, error) {
					return nil, failure
				}}
			},
			want: failure,
		},
		{
			name: "write failure",
			directory: func(base *memoryDirectory) outputcap.Directory {
				return &faultDirectory{Directory: base, createFile: func(string, bool, int64) (outputcap.File, error) {
					return &faultFile{writeAt: func([]byte, int64) (int, error) { return 0, failure }}, nil
				}}
			},
			want: failure,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newMemoryDirectory()
			writeMemoryFile(t, base, "record.state", previous)
			if err := InstallReplace(test.directory(base), "record.state", previous, next); !errors.Is(err, test.want) {
				t.Fatalf("InstallReplace() error = %v, want %v", err, test.want)
			}
		})
	}
}

func writeMemoryFile(t *testing.T, directory outputcap.Directory, name string, encoded []byte) {
	t.Helper()
	file, err := directory.CreateFile(name, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(file, encoded); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustRecordID(t *testing.T, fill byte) resumestate.FileCheckpointRecordID {
	t.Helper()
	recordID, err := resumestate.FileCheckpointRecordIDFromBytes(bytes.Repeat([]byte{fill}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return recordID
}

type faultDirectory struct {
	outputcap.Directory
	names           func(int) ([]string, error)
	openDirectory   func(string, bool) (outputcap.Directory, error)
	createDirectory func(string, bool) (outputcap.Directory, error)
	createFile      func(string, bool, int64) (outputcap.File, error)
	openFile        func(string, bool, bool) (outputcap.File, error)
	linkFile        func(outputcap.File, string) (outputcap.File, error)
	replaceFile     func(outputcap.File, string) error
	observeEntry    func(string) (outputcap.EntryKind, error)
	sync            func() error
}

func (directory *faultDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

func (directory *faultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.openDirectory != nil {
		return directory.openDirectory(name, private)
	}
	return directory.Directory.OpenDirectory(name, private)
}

func (directory *faultDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.createDirectory != nil {
		return directory.createDirectory(name, private)
	}
	return directory.Directory.CreateDirectory(name, private)
}

func (directory *faultDirectory) CreateFile(name string, private bool, size int64) (outputcap.File, error) {
	if directory.createFile != nil {
		return directory.createFile(name, private, size)
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *faultDirectory) OpenFile(name string, private, writable bool) (outputcap.File, error) {
	if directory.openFile != nil {
		return directory.openFile(name, private, writable)
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *faultDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	if directory.linkFile != nil {
		return directory.linkFile(source, name)
	}
	return directory.Directory.LinkFileNoReplace(source, name)
}

func (directory *faultDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	if directory.replaceFile != nil {
		return directory.replaceFile(source, name)
	}
	return directory.Directory.ReplacePrivateFile(source, name)
}

func (directory *faultDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory.observeEntry != nil {
		return directory.observeEntry(name)
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *faultDirectory) Sync() error {
	if directory.sync != nil {
		return directory.sync()
	}
	return directory.Directory.Sync()
}

type faultFile struct {
	outputcap.File
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)
}

func (file *faultFile) ReadAt(encoded []byte, offset int64) (int, error) {
	if file.readAt != nil {
		return file.readAt(encoded, offset)
	}
	return file.File.ReadAt(encoded, offset)
}

func (file *faultFile) WriteAt(encoded []byte, offset int64) (int, error) {
	return file.writeAt(encoded, offset)
}

func (file *faultFile) Close() error { return nil }

type memoryDirectory struct {
	outputcap.Directory
	mu    sync.Mutex
	dirs  map[string]*memoryDirectory
	files map[string]*memoryFileData
	locks map[string]*memoryLock
}

func newMemoryDirectory() *memoryDirectory {
	return &memoryDirectory{
		dirs:  make(map[string]*memoryDirectory),
		files: make(map[string]*memoryFileData),
		locks: make(map[string]*memoryLock),
	}
}

func (directory *memoryDirectory) Close() error { return nil }

func (directory *memoryDirectory) Duplicate() (outputcap.Directory, error) { return directory, nil }

func (directory *memoryDirectory) Sync() error { return nil }

func (directory *memoryDirectory) Names(limit int) ([]string, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if limit <= 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	names := make([]string, 0, len(directory.dirs)+len(directory.files))
	for name := range directory.dirs {
		names = append(names, name)
	}
	for name := range directory.files {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return names, nil
}

func (directory *memoryDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}

func (directory *memoryDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.dirs[name]; found {
		return outputcap.EntryDirectory, true, nil
	}
	if _, found := directory.files[name]; found {
		return outputcap.EntryRegularFile, true, nil
	}
	return outputcap.EntryAbsent, true, nil
}

func (directory *memoryDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	peer, ok := other.(*memoryDirectory)
	return ok && directory == peer, nil
}

func (directory *memoryDirectory) OpenDirectory(name string, _ bool) (outputcap.Directory, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	child, found := directory.dirs[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return child, nil
}

func (directory *memoryDirectory) CreateDirectory(name string, _ bool) (outputcap.Directory, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	child := newMemoryDirectory()
	directory.dirs[name] = child
	return child, nil
}

func (directory *memoryDirectory) CreateFile(name string, _ bool, size int64) (outputcap.File, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	data := &memoryFileData{bytes: make([]byte, int(size))}
	directory.files[name] = data
	return &memoryFile{data: data}, nil
}

func (directory *memoryDirectory) OpenFile(name string, _, _ bool) (outputcap.File, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return nil, fs.ErrNotExist
	}
	return &memoryFile{data: data}, nil
}

func (directory *memoryDirectory) LinkFileNoReplace(source outputcap.File, name string) (outputcap.File, error) {
	file, ok := source.(*memoryFile)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if _, found := directory.files[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	if _, found := directory.dirs[name]; found {
		return nil, outputcap.ErrNamespaceCollision
	}
	directory.files[name] = file.data
	return &memoryFile{data: file.data}, nil
}

func (directory *memoryDirectory) ReplacePrivateFile(source outputcap.File, name string) error {
	file, ok := source.(*memoryFile)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	for candidate, data := range directory.files {
		if candidate != name && data == file.data {
			delete(directory.files, candidate)
			break
		}
	}
	directory.files[name] = file.data
	return nil
}

func (directory *memoryDirectory) RemoveFile(name string, expected outputcap.File) error {
	file, ok := expected.(*memoryFile)
	if !ok {
		return outputcap.ErrUnsafeNamespace
	}
	directory.mu.Lock()
	defer directory.mu.Unlock()
	data, found := directory.files[name]
	if !found {
		return fs.ErrNotExist
	}
	if data != file.data {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.files, name)
	return nil
}

func (directory *memoryDirectory) AcquireLock(name string, _ bool) (outputcap.Lock, bool, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	if lock, found := directory.locks[name]; found && !lock.closed {
		return nil, false, outputcap.ErrNamespaceLockBusy
	}
	data, found := directory.files[name]
	if !found {
		data = &memoryFileData{}
		directory.files[name] = data
	}
	lock := &memoryLock{directory: directory, name: name, file: &memoryFile{data: data}}
	directory.locks[name] = lock
	return lock, !found, nil
}

type memoryFileData struct {
	mu    sync.Mutex
	bytes []byte
}

type memoryFile struct {
	outputcap.File
	data *memoryFileData
}

func (file *memoryFile) Close() error { return nil }

func (file *memoryFile) Sync() error { return nil }

func (file *memoryFile) Size() (uint64, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	return uint64(len(file.data.bytes)), nil
}

func (file *memoryFile) ReadAt(target []byte, offset int64) (int, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	if offset < 0 || offset >= int64(len(file.data.bytes)) {
		return 0, io.EOF
	}
	read := copy(target, file.data.bytes[offset:])
	if read != len(target) {
		return read, io.EOF
	}
	return read, nil
}

func (file *memoryFile) WriteAt(source []byte, offset int64) (int, error) {
	file.data.mu.Lock()
	defer file.data.mu.Unlock()
	if offset < 0 || offset+int64(len(source)) > int64(len(file.data.bytes)) {
		return 0, io.ErrShortWrite
	}
	return copy(file.data.bytes[offset:], source), nil
}

type memoryLock struct {
	outputcap.Lock
	directory *memoryDirectory
	name      string
	file      *memoryFile
	closed    bool
}

func (lock *memoryLock) File() outputcap.File { return lock.file }

func (lock *memoryLock) Close() error {
	lock.directory.mu.Lock()
	defer lock.directory.mu.Unlock()
	if lock.closed {
		return nil
	}
	lock.closed = true
	delete(lock.directory.locks, lock.name)
	return nil
}

type shortWriteFile struct{ outputcap.File }

func (shortWriteFile) WriteAt(source []byte, _ int64) (int, error) { return len(source) - 1, nil }

func (shortWriteFile) Sync() error { return nil }
