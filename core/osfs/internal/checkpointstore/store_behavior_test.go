package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOpenFailsClosedAtEveryNamespaceBoundary(t *testing.T) {
	for _, blocked := range []string{
		ControlDirectory, CheckpointDirectory, LeasesDirectory, LookupDirectory, OperationsDirectory,
	} {
		t.Run(blocked, func(t *testing.T) {
			root := newMemoryDirectory()
			config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x62)
			if blocked == ControlDirectory {
				writeMemoryFile(t, root, blocked, []byte("regular file blocks directory authority"))
			} else {
				control, err := root.CreateDirectory(ControlDirectory, true)
				if err != nil {
					t.Fatal(err)
				}
				checkpointParent := control
				if blocked != CheckpointDirectory {
					checkpointRoot, err := control.CreateDirectory(CheckpointDirectory, true)
					if err != nil {
						t.Fatal(err)
					}
					encoded, err := checkpointmodel.EncodeOwnership(config.Ownership)
					if err != nil {
						t.Fatal(err)
					}
					writeMemoryFile(t, checkpointRoot, OwnershipFile, encoded)
					checkpointParent = checkpointRoot
				}
				writeMemoryFile(t, checkpointParent, blocked, []byte("regular file blocks directory authority"))
			}
			if _, err := Initialize(config); err == nil {
				t.Fatalf("Initialize succeeded with %s occupied by a regular file", blocked)
			}
		})
	}

	for _, blocked := range []string{RecordsDirectory, AnchorsDirectory, StagesDirectory} {
		t.Run(blocked, func(t *testing.T) {
			root := newMemoryDirectory()
			config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x72)
			namespace, err := Initialize(config)
			if err != nil {
				t.Fatal(err)
			}
			defer namespace.Close()
			lease, err := namespace.AcquireOperation(
				intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			operationDirectory, err := lease.operations.CreateDirectory(
				operationNamespaceName(intent.intent.OperationID()), true,
			)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints, err := operationDirectory.CreateDirectory(CheckpointsDirectory, true)
			if err != nil {
				t.Fatal(err)
			}
			writeMemoryFile(t, checkpoints, blocked, []byte("regular file blocks directory authority"))
			if err := errors.Join(checkpoints.Close(), operationDirectory.Close()); err != nil {
				t.Fatal(err)
			}
			if _, err := lease.OpenOrCreateRepository(); err == nil {
				t.Fatalf("repository opened with %s occupied by a regular file", blocked)
			}
		})
	}

	t.Run("unknown operation child", func(t *testing.T) {
		root := newMemoryDirectory()
		config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x73)
		namespace, err := Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		defer namespace.Close()
		lease, err := namespace.AcquireOperation(
			intent.intent.OperationID(), intent.intent.Digest(), intent.intent.BindingDigest(),
		)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		operationDirectory, err := lease.operations.CreateDirectory(
			operationNamespaceName(intent.intent.OperationID()), true,
		)
		if err != nil {
			t.Fatal(err)
		}
		writeMemoryFile(t, operationDirectory, "foreign-entry", []byte("preserve"))
		if err := operationDirectory.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := lease.OpenOrCreateRepository(); errorCode(err) != ErrorUnsafeInstall {
			t.Fatalf("repository accepted unknown operation child: %v", err)
		}
		operationDirectory, err = lease.operations.OpenDirectory(
			operationNamespaceName(intent.intent.OperationID()), true,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer operationDirectory.Close()
		if encoded, err := ReadFile(operationDirectory, "foreign-entry"); err != nil || string(encoded) != "preserve" {
			t.Fatalf("unknown operation child was mutated: %q, %v", encoded, err)
		}
	})
}

func TestRecordStoreCreatesReadsAndReplacesAuthenticatedImages(t *testing.T) {
	records := newMemoryDirectory()

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
	if err := InstallCreate(shard, "record.state", second); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("conflicting create error = %v", err)
	}
	if encoded, err := ReadFile(shard, "record.state"); err != nil || !bytes.Equal(encoded, first) {
		t.Fatalf("ReadFile() = %q, %v", encoded, err)
	}
	if err := InstallReplace(shard, "record.state", []byte("stale"), second); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
		t.Fatalf("stale replace error = %v", err)
	}
	if err := InstallReplace(shard, "record.state", first, second); err != nil {
		t.Fatal(err)
	}
	if encoded, err := ReadFile(shard, "record.state"); err != nil || !bytes.Equal(encoded, second) {
		t.Fatalf("replaced ReadFile() = %q, %v", encoded, err)
	}
	if operationErr, cleanupErr := RemoveExact(shard, "record.state", first); !errors.Is(operationErr, checkpointmodel.ErrRecordBinding) || cleanupErr != nil {
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
	if operationErr, cleanupErr := RemoveExact(nil, "record.state", second); !errors.Is(operationErr, checkpointmodel.ErrInvalidRecord) || cleanupErr != nil {
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
		if !errors.Is(err, checkpointmodel.ErrInvalidRecord) {
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

		if err := InstallCreate(directory, "record.state", expected); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("mismatched candidate error = %v", err)
		}
		if _, err := ReadFile(directory, "record.state"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("mismatched candidate installed target: %v", err)
		}
	})
}

func TestOwnershipBootstrapResumesExactDeterministicCandidate(t *testing.T) {
	root := newMemoryDirectory()
	config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x91)
	control, err := openOrCreateDirectory(root, ControlDirectory)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRoot, err := openOrCreateDirectory(control, CheckpointDirectory)
	if err != nil {
		t.Fatal(err)
	}
	ownershipDirectory, err := openOrCreateDirectory(checkpointRoot, OwnershipDirectory)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := checkpointmodel.EncodeOwnership(config.Ownership)
	if err != nil {
		t.Fatal(err)
	}
	candidate := TemporaryName(OwnershipFile, encoded, 0)
	writeMemoryFile(t, ownershipDirectory, candidate, encoded)
	duplicateCandidate := TemporaryName(OwnershipFile, encoded, 1)
	writeMemoryFile(t, ownershipDirectory, duplicateCandidate, encoded)

	status, err := inspectOwnership(ownershipDirectory, config.Ownership)
	if err != nil || status != OwnershipRecoverable {
		t.Fatalf("candidate ownership status = %d, %v", status, err)
	}
	namespace, err := Initialize(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := namespace.Close(); err != nil {
		t.Fatal(err)
	}
	status, err = inspectOwnership(ownershipDirectory, config.Ownership)
	if err != nil || status != OwnershipMatched {
		t.Fatalf("resumed ownership status = %d, %v", status, err)
	}
	if _, err := ReadFile(ownershipDirectory, candidate); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resumed ownership retained candidate: %v", err)
	}
	if _, err := ReadFile(ownershipDirectory, duplicateCandidate); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resumed ownership retained duplicate candidate: %v", err)
	}
}

func TestRecordStoreMapsRecordIDsAndCleansTemporaryFiles(t *testing.T) {
	records := newMemoryDirectory()
	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{0x71}, sha256.Size))
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
	existingParent := newMemoryDirectory()
	existingChild, err := existingParent.CreateDirectory("child", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := existingChild.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: existingParent,
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return openedOnError, failure
		},
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("open failure = %v", err)
	}

	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		createDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), nil
		},
		sync: func() error { return failure },
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("parent sync failure = %v", err)
	}

	collisionChild := newMemoryDirectory()
	collisionParent := newMemoryDirectory()
	reopened, err := openOrCreateDirectory(&faultDirectory{
		Directory: collisionParent,
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return collisionChild, nil
		},
		createDirectory: func(name string, private bool) (outputcap.Directory, error) {
			created, err := collisionParent.CreateDirectory(name, private)
			if err != nil {
				return nil, err
			}
			return created, outputcap.ErrNamespaceCollision
		},
	}, "child")
	if err != nil || reopened != collisionChild {
		t.Fatalf("collision reopen = %v, %v", reopened, err)
	}

	if _, err := openOrCreateDirectory(&faultDirectory{
		Directory: newMemoryDirectory(),
		createDirectory: func(string, bool) (outputcap.Directory, error) {
			return newMemoryDirectory(), failure
		},
	}, "child"); !errors.Is(err, failure) {
		t.Fatalf("create failure = %v", err)
	}
}

func TestOwnershipMarkerRejectsAmbiguousInstallCuts(t *testing.T) {
	failure := errors.New("marker operation failed")
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        bytes.Repeat([]byte{0x81}, receivecontract.AuthorityRefBytes),
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := checkpointmodel.EncodeOwnership(ownership)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnership(newMemoryDirectory(), checkpointmodel.Ownership{}, true); err == nil {
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
	if err := ensureOwnership(wrongKind, ownership, true); !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
		t.Fatalf("non-file marker error = %v", err)
	}
	malformed := newMemoryDirectory()
	writeMemoryFile(t, malformed, OwnershipFile, []byte("malformed"))
	if err := ensureOwnership(malformed, ownership, true); !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
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
	if err := installOwnedFile(conflict, OwnershipFile, encoded); !errors.Is(err, checkpointmodel.ErrInvalidOwnership) {
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
			want: checkpointmodel.ErrRecordCrashBoundary,
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
	if err := InstallReplace(failedReplace, "record.state", encoded, []byte("next-image")); !errors.Is(err, failure) || !errors.Is(err, checkpointmodel.ErrRecordCrashBoundary) {
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

func TestRecordInstallerClosesCapabilitiesReturnedWithErrors(t *testing.T) {
	failure := errors.New("operation returned a capability with an error")
	createCloses := 0
	createHandle := &closeTrackingFile{
		File:   &memoryFile{data: &memoryFileData{bytes: make([]byte, len("record-image"))}},
		closes: &createCloses,
	}
	createDirectory := &faultDirectory{
		Directory: newMemoryDirectory(),
		createFile: func(string, bool, int64) (outputcap.File, error) {
			return createHandle, failure
		},
	}
	if err := InstallCreate(createDirectory, "record.state", []byte("record-image")); !errors.Is(err, failure) || createCloses != 1 {
		t.Fatalf("create capability cleanup = closes:%d error:%v", createCloses, err)
	}

	linkCloses := 0
	linkHandle := &closeTrackingFile{
		File:   &memoryFile{data: &memoryFileData{bytes: []byte("target")}},
		closes: &linkCloses,
	}
	linkDirectory := &faultDirectory{
		Directory: newMemoryDirectory(),
		linkFile: func(outputcap.File, string) (outputcap.File, error) {
			return linkHandle, failure
		},
	}
	if err := InstallCreate(linkDirectory, "record.state", []byte("record-image")); !errors.Is(err, failure) || linkCloses != 1 {
		t.Fatalf("link capability cleanup = closes:%d error:%v", linkCloses, err)
	}
}

func TestRecordReplacementPreservesEveryPreInstallFailure(t *testing.T) {
	failure := errors.New("replacement precondition failed")
	previous := []byte("previous-image")
	next := []byte("next-image")
	openFailureBase := newMemoryDirectory()
	openFailureShard, err := openFailureBase.CreateDirectory("0a", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := openFailureShard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenShard(&faultDirectory{
		Directory: openFailureBase,
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

func mustRecordID(t *testing.T, fill byte) checkpointmodel.RecordID {
	t.Helper()
	recordID, err := checkpointmodel.RecordIDFromBytes(bytes.Repeat([]byte{fill}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	return recordID
}
