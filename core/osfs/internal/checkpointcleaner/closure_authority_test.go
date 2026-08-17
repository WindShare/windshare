package checkpointcleaner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

func TestC5ClosureAggregatesCloseFailuresInReverseLockOrder(t *testing.T) {
	order := []string{}
	failures := map[string]error{}
	newFailure := func(name string) error {
		failure := errors.New(name + " close failed")
		failures[name] = failure
		return failure
	}
	run := &cleanupRun{
		sessionLocks: []cleanupLockRef{
			{lock: &c5ClosureCloseLock{name: "session-1-lock", order: &order, err: newFailure("session-1-lock")}, parent: &c5ClosureCloseDirectory{name: "session-1-parent", order: &order, err: newFailure("session-1-parent")}},
			{lock: &c5ClosureCloseLock{name: "session-2-lock", order: &order, err: newFailure("session-2-lock")}, parent: &c5ClosureCloseDirectory{name: "session-2-parent", order: &order, err: newFailure("session-2-parent")}},
		},
		coordinator: &c5ClosureCloseLock{name: "coordinator", order: &order, err: newFailure("coordinator")},
		cleanupLock: &c5ClosureCloseLock{name: "cleanup", order: &order, err: newFailure("cleanup")},
		namespace:   &c5ClosureCloseDirectory{name: "namespace", order: &order, err: newFailure("namespace")},
		control:     &c5ClosureCloseDirectory{name: "control", order: &order, err: newFailure("control")},
		guard:       &c5ClosureCloseGuard{name: "guard", order: &order, err: newFailure("guard")},
	}
	err := run.Close()
	wantOrder := []string{
		"session-2-lock", "session-2-parent", "session-1-lock", "session-1-parent",
		"coordinator", "cleanup", "namespace", "control", "guard",
	}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("close order = %v, want %v", order, wantOrder)
	}
	for name, failure := range failures {
		if !errors.Is(err, failure) {
			t.Fatalf("missing %s failure in %v", name, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
	if err := (*cleanupRun)(nil).Close(); err != nil {
		t.Fatalf("nil close = %v", err)
	}
}

func TestC5ClosureCoordinatorSyncCutRetainsPublishedStateAndResumes(t *testing.T) {
	platform, rootPath := newCleanerPlatform(t)
	legacy := installLegacyNamespace(t, platform)
	control := openLegacyControl(t, platform)
	controlTemporary := legacyresume.ControlRecord + ".tmp-" + strings.Repeat("d", 64)
	writeCapabilityFile(t, control, controlTemporary, []byte("retired control candidate"))
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	publishedPath := filepath.Join(rootPath, "published-after-cleanup.txt")
	if err := os.WriteFile(publishedPath, []byte("published"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncFailure := errors.New("coordinator directory sync failed")
	tracker := &c5ClosureTracker{coordinatorSyncFailure: syncFailure}
	trackedPlatform := &c5ClosureTrackedPlatform{Platform: platform, tracker: tracker}
	steps := []CheckpointCleanupStep{}
	config := cleanerConfig(trackedPlatform)
	config.Fault = func(step CheckpointCleanupStep) error {
		steps = append(steps, step)
		return nil
	}
	report, err := cleanOwnedNamespace(context.Background(), config)
	if !errors.Is(err, syncFailure) || report.Complete || report.Removed == 0 {
		t.Fatalf("sync-cut cleanup = %+v, %v", report, err)
	}
	if !c5ClosureHasEntry(report, "published-after-cleanup.txt", cleanupDetailPublished) {
		t.Fatalf("published output missing from report: %+v", report.Entries)
	}
	if encoded, err := os.ReadFile(publishedPath); err != nil || string(encoded) != "published" {
		t.Fatalf("published output changed: %q, %v", encoded, err)
	}
	for _, relative := range []string{
		legacy.session,
		filepath.Join(legacyresume.ControlDirectory, controlTemporary),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
		filepath.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
	} {
		if _, err := os.Stat(filepath.Join(rootPath, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed crash-cut path %q = %v", relative, err)
		}
	}

	wantLocks := []string{
		path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
		path.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, FileCheckpointCleanupLock),
		path.Join(
			legacyresume.ControlDirectory, legacyresume.SessionsDirectory,
			strings.Repeat("a", 64), strings.Repeat("b", 32), legacyresume.SessionLock,
		),
	}
	if !slices.Equal(tracker.locks, wantLocks) {
		t.Fatalf("lock order = %v, want %v", tracker.locks, wantLocks)
	}
	temporaryIndex := c5ClosureStepIndex(steps, path.Join(legacyresume.ControlDirectory, controlTemporary))
	recordIndex := c5ClosureStepIndex(steps, path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord))
	coordinatorIndex := c5ClosureStepIndex(steps, path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock))
	if temporaryIndex <= 0 || recordIndex != temporaryIndex+1 || coordinatorIndex != recordIndex+1 || coordinatorIndex != len(steps)-1 {
		t.Fatalf("removal order = %+v", steps)
	}
	sessionPrefix := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory) + "/"
	for _, step := range steps[:temporaryIndex] {
		if step.RelativePath != strings.TrimSuffix(sessionPrefix, "/") &&
			!strings.HasPrefix(step.RelativePath, sessionPrefix) {
			t.Fatalf("non-session path preceded control temporary: %+v", steps)
		}
	}

	resumed, err := cleanOwnedNamespace(context.Background(), cleanerConfig(trackedPlatform))
	if err != nil || !resumed.Complete || !resumed.Resumed || resumed.Removed != 0 {
		t.Fatalf("resumed sync cut = %+v, %v", resumed, err)
	}
	if _, err := os.Stat(publishedPath); err != nil {
		t.Fatalf("resume removed published output: %v", err)
	}
}

func TestC5ClosureBoundedRecordFailsClosedAtEveryAuthorityCut(t *testing.T) {
	if _, err := readBoundedRecord(nil, "record", 1); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil directory = %v", err)
	}
	if _, err := readBoundedRecord(&c5ClosureFaultDirectory{}, "", 1); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("empty name = %v", err)
	}

	classificationFailure := errors.New("classification failed")
	directory := &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryAbsent, false, classificationFailure
	}}
	if _, err := readBoundedRecord(directory, "record", 8); !errors.Is(err, classificationFailure) {
		t.Fatalf("classification failure = %v", err)
	}
	for name, entry := range map[string]c5ClosureEntry{
		"absent":       {kind: outputcap.EntryAbsent, exact: true},
		"aliased":      {kind: outputcap.EntryRegularFile, exact: false},
		"wrong object": {kind: outputcap.EntryDirectory, exact: true},
	} {
		t.Run(name, func(t *testing.T) {
			directory := &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
				return entry.kind, entry.exact, nil
			}}
			_, err := readBoundedRecord(directory, "record", 8)
			if entry.kind == outputcap.EntryAbsent {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("absent record = %v", err)
				}
			} else if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("unsafe record = %v", err)
			}
		})
	}

	openFailure := errors.New("open failed")
	closeFailure := errors.New("open result close failed")
	directory = c5ClosureRecordDirectory(&c5ClosureFaultFile{closeErr: closeFailure})
	directory.openFile = func(string, bool, bool) (outputcap.MutableFile, error) {
		return &c5ClosureFaultFile{closeErr: closeFailure}, openFailure
	}
	if _, err := readBoundedRecord(directory, "record", 8); !errors.Is(err, openFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("open/close failures = %v", err)
	}

	sizeFailure := errors.New("size failed")
	for name, file := range map[string]*c5ClosureFaultFile{
		"size error": {sizeErr: sizeFailure},
		"empty":      {size: 0},
		"oversized":  {size: 9},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readBoundedRecord(c5ClosureRecordDirectory(file), "record", 8)
			if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("size rejection = %v", err)
			}
			if file.sizeErr != nil && !errors.Is(err, sizeFailure) {
				t.Fatalf("missing size failure = %v", err)
			}
		})
	}

	readFailure := errors.New("read failed")
	file := &c5ClosureFaultFile{size: 4, read: func([]byte, int64) (int, error) { return 0, readFailure }}
	if _, err := readBoundedRecord(c5ClosureRecordDirectory(file), "record", 8); !errors.Is(err, readFailure) {
		t.Fatalf("read failure = %v", err)
	}
	file = &c5ClosureFaultFile{size: 4, read: func(buffer []byte, _ int64) (int, error) {
		copy(buffer, "ab")
		return 2, io.EOF
	}}
	if _, err := readBoundedRecord(c5ClosureRecordDirectory(file), "record", 8); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short record = %v", err)
	}
	closeFailure = errors.New("record close failed")
	file = &c5ClosureFaultFile{data: []byte("proof"), size: 5, closeErr: closeFailure}
	encoded, err := readBoundedRecord(c5ClosureRecordDirectory(file), "record", 8)
	if string(encoded) != "proof" || !errors.Is(err, closeFailure) {
		t.Fatalf("record/close result = %q, %v", encoded, err)
	}
	if err := closeFile(nil); err != nil {
		t.Fatalf("nil file close = %v", err)
	}
}

func TestC5ClosureCheckpointNamespaceCreationRevalidatesCollisionsAndSyncs(t *testing.T) {
	t.Run("already retained", func(t *testing.T) {
		namespace := &c5ClosureFaultDirectory{}
		run := &cleanupRun{namespace: namespace}
		if err := run.ensureCheckpointNamespace(); err != nil || run.namespace != namespace {
			t.Fatalf("retained namespace = %p, %v", run.namespace, err)
		}
	})

	t.Run("existing conflicts", func(t *testing.T) {
		for name, fact := range map[string]c5ClosureEntry{
			"aliased":    {kind: outputcap.EntryDirectory, exact: false},
			"wrong kind": {kind: outputcap.EntryRegularFile, exact: true},
		} {
			t.Run(name, func(t *testing.T) {
				run := &cleanupRun{control: &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
					return fact.kind, fact.exact, nil
				}}}
				if err := run.ensureCheckpointNamespace(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
					t.Fatalf("conflicting namespace = %v", err)
				}
			})
		}
	})

	t.Run("existing exact namespace", func(t *testing.T) {
		namespace := &c5ClosureFaultDirectory{}
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryDirectory, true, nil
			},
			openDirectory: func(string, bool) (outputcap.Directory, error) { return namespace, nil },
		}
		run := &cleanupRun{control: control}
		if err := run.ensureCheckpointNamespace(); err != nil || run.namespace != namespace {
			t.Fatalf("existing namespace = %p, %v", run.namespace, err)
		}
	})

	t.Run("collision is rebound by exact identity", func(t *testing.T) {
		namespace := &c5ClosureFaultDirectory{}
		classifications := 0
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				classifications++
				if classifications == 1 {
					return outputcap.EntryAbsent, true, nil
				}
				return outputcap.EntryDirectory, true, nil
			},
			createDirectory: func(string, bool) (outputcap.Directory, error) {
				return nil, outputcap.ErrNamespaceCollision
			},
			openDirectory: func(string, bool) (outputcap.Directory, error) { return namespace, nil },
		}
		run := &cleanupRun{control: control}
		if err := run.ensureCheckpointNamespace(); err != nil || run.namespace != namespace {
			t.Fatalf("reconciled collision = %p, %v", run.namespace, err)
		}
	})

	t.Run("collision changed kind", func(t *testing.T) {
		classifications := 0
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				classifications++
				if classifications == 1 {
					return outputcap.EntryAbsent, true, nil
				}
				return outputcap.EntryRegularFile, true, nil
			},
			createDirectory: func(string, bool) (outputcap.Directory, error) {
				return nil, outputcap.ErrNamespaceCollision
			},
		}
		if err := (&cleanupRun{control: control}).ensureCheckpointNamespace(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("changed collision = %v", err)
		}
	})

	t.Run("create error closes returned authority", func(t *testing.T) {
		createFailure := errors.New("create namespace failed")
		closeFailure := errors.New("created authority close failed")
		child := &c5ClosureFaultDirectory{closeErr: closeFailure}
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, true, nil
			},
			createDirectory: func(string, bool) (outputcap.Directory, error) { return child, createFailure },
		}
		if err := (&cleanupRun{control: control}).ensureCheckpointNamespace(); !errors.Is(err, createFailure) || !errors.Is(err, closeFailure) {
			t.Fatalf("create/close failure = %v", err)
		}
	})

	t.Run("nil created authority", func(t *testing.T) {
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, true, nil
			},
			createDirectory: func(string, bool) (outputcap.Directory, error) { return nil, nil },
		}
		if err := (&cleanupRun{control: control}).ensureCheckpointNamespace(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("nil namespace = %v", err)
		}
	})

	t.Run("both durability barriers are attempted", func(t *testing.T) {
		namespaceSync := errors.New("namespace sync failed")
		controlSync := errors.New("control sync failed")
		closeFailure := errors.New("namespace close failed")
		namespace := &c5ClosureFaultDirectory{syncErr: namespaceSync, closeErr: closeFailure}
		control := &c5ClosureFaultDirectory{
			classify: func(string) (outputcap.EntryKind, bool, error) {
				return outputcap.EntryAbsent, true, nil
			},
			createDirectory: func(string, bool) (outputcap.Directory, error) { return namespace, nil },
			syncErr:         controlSync,
		}
		err := (&cleanupRun{control: control}).ensureCheckpointNamespace()
		for _, want := range []error{namespaceSync, controlSync, closeFailure} {
			if !errors.Is(err, want) {
				t.Fatalf("missing %v from %v", want, err)
			}
		}
	})
}

func TestC5ClosureLockAcquisitionRejectsBusyReplacedAndMalformedLocks(t *testing.T) {
	t.Run("cleanup lock", func(t *testing.T) {
		if err := (&cleanupRun{}).acquireCleanupLock(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("missing namespace = %v", err)
		}
		busy := &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return nil, false, outputcap.ErrNamespaceLockBusy
		}}
		if err := (&cleanupRun{namespace: busy}).acquireCleanupLock(); !errors.Is(err, ErrCheckpointCleanerBusy) {
			t.Fatalf("busy cleanup lock = %v", err)
		}
		syncFailure := errors.New("new cleanup lock sync failed")
		closed := false
		lock := &c5ClosureFaultLock{file: &c5ClosureFaultFile{}, close: func() error { closed = true; return nil }}
		namespace := &c5ClosureFaultDirectory{
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, true, nil },
			syncErr:     syncFailure,
		}
		if err := (&cleanupRun{namespace: namespace}).acquireCleanupLock(); !errors.Is(err, syncFailure) || !closed {
			t.Fatalf("new lock sync cut: closed=%t err=%v", closed, err)
		}
		lock = &c5ClosureFaultLock{file: &c5ClosureFaultFile{size: 1}}
		namespace = &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return lock, false, nil
		}}
		if err := (&cleanupRun{namespace: namespace}).acquireCleanupLock(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("non-empty cleanup lock = %v", err)
		}
	})

	t.Run("coordinator lock", func(t *testing.T) {
		busy := &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return nil, false, outputcap.ErrNamespaceLockBusy
		}}
		if err := (&cleanupRun{control: busy}).acquireCoordinator(); !errors.Is(err, ErrCheckpointCleanerBusy) {
			t.Fatalf("busy coordinator = %v", err)
		}
		created := &c5ClosureFaultLock{file: &c5ClosureFaultFile{}}
		control := &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return created, true, nil
		}}
		if err := (&cleanupRun{control: control}).acquireCoordinator(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("recreated coordinator = %v", err)
		}
		malformed := &c5ClosureFaultLock{file: &c5ClosureFaultFile{size: 1}}
		control = &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return malformed, false, nil
		}}
		if err := (&cleanupRun{control: control}).acquireCoordinator(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("non-empty coordinator = %v", err)
		}
	})

	t.Run("session lock", func(t *testing.T) {
		busy := &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
			return nil, false, outputcap.ErrNamespaceLockBusy
		}}
		if err := (&cleanupRun{}).acquireExistingSessionLock(busy, legacyresume.SessionLock, "session"); !errors.Is(err, ErrCheckpointCleanerBusy) {
			t.Fatalf("busy session = %v", err)
		}
		duplicateFailure := errors.New("duplicate session failed")
		lockClosed := false
		lock := &c5ClosureFaultLock{file: &c5ClosureFaultFile{}, close: func() error { lockClosed = true; return nil }}
		directory := &c5ClosureFaultDirectory{
			acquireLock: func(string, bool) (outputcap.Lock, bool, error) { return lock, false, nil },
			duplicate:   func() (outputcap.Directory, error) { return nil, duplicateFailure },
		}
		if err := (&cleanupRun{}).acquireExistingSessionLock(directory, legacyresume.SessionLock, "session"); !errors.Is(err, duplicateFailure) || !lockClosed {
			t.Fatalf("duplicate session cut: closed=%t err=%v", lockClosed, err)
		}
	})
}

func TestC5ClosureRevalidationRejectsReboundRootsDirectoriesAndLocks(t *testing.T) {
	binding := c5ClosureRootBinding(t, "root")
	root := &c5ClosureFaultDirectory{sameDirectory: func(outputcap.Directory) (bool, error) { return true, nil }}
	platform := &c5ClosurePlatform{
		root: root, binding: binding,
		certification: outputcap.CertificationWindowsNTFSProcessRestart,
		durability:    transfer.DurabilityProcessRestart,
	}
	run := &cleanupRun{
		cleaner: &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{Platform: platform}},
		root:    root, rootBinding: binding.Bytes(), certification: string(platform.certification),
		durability: transfer.DurabilityProcessRestart,
	}
	platform.bindingErr = errors.New("root binding failed")
	if err := run.revalidateCertifiedRoot(); !errors.Is(err, ErrCheckpointCleanerOwnership) || !errors.Is(err, platform.bindingErr) {
		t.Fatalf("binding failure = %v", err)
	}
	platform.bindingErr = nil
	platform.binding = c5ClosureRootBinding(t, "replacement")
	if err := run.revalidateCertifiedRoot(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("replaced root binding = %v", err)
	}
	platform.binding = binding
	root.sameDirectory = func(outputcap.Directory) (bool, error) { return false, nil }
	if err := run.revalidateCertifiedRoot(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("replaced root directory = %v", err)
	}

	openFailure := errors.New("control reopen failed")
	run.root = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return nil, openFailure
	}}
	run.control = &c5ClosureFaultDirectory{}
	if err := run.revalidateControl(); !errors.Is(err, openFailure) || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("control reopen = %v", err)
	}
	closeFailure := errors.New("reopened control close failed")
	currentControl := &c5ClosureFaultDirectory{
		sameDirectory: func(outputcap.Directory) (bool, error) { return false, nil },
		closeErr:      closeFailure,
	}
	run.root = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return currentControl, nil
	}}
	if err := run.revalidateControl(); !errors.Is(err, ErrCheckpointCleanerOwnership) || !errors.Is(err, closeFailure) {
		t.Fatalf("control replacement = %v", err)
	}

	run.control = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return nil, openFailure
	}}
	run.namespace = &c5ClosureFaultDirectory{}
	if err := run.revalidateCheckpointNamespace(); !errors.Is(err, openFailure) || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("namespace reopen = %v", err)
	}
	currentNamespace := &c5ClosureFaultDirectory{
		sameDirectory: func(outputcap.Directory) (bool, error) { return false, nil },
		closeErr:      closeFailure,
	}
	run.control = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return currentNamespace, nil
	}}
	if err := run.revalidateCheckpointNamespace(); !errors.Is(err, ErrCheckpointCleanerOwnership) || !errors.Is(err, closeFailure) {
		t.Fatalf("namespace replacement = %v", err)
	}

	if err := run.revalidateLock(nil, "lock", nil); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil lock authority = %v", err)
	}
	lockOpenFailure := errors.New("lock reopen failed")
	parent := &c5ClosureFaultDirectory{openFile: func(string, bool, bool) (outputcap.MutableFile, error) {
		return nil, lockOpenFailure
	}}
	expected := &c5ClosureFaultLock{file: &c5ClosureFaultFile{}}
	if err := run.revalidateLock(parent, "lock", expected); !errors.Is(err, lockOpenFailure) || !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("lock reopen = %v", err)
	}
	currentFile := &c5ClosureFaultFile{same: func(outputcap.FileIdentity) (bool, error) { return false, nil }, closeErr: closeFailure}
	parent.openFile = func(string, bool, bool) (outputcap.MutableFile, error) { return currentFile, nil }
	if err := run.revalidateLock(parent, "lock", expected); !errors.Is(err, ErrCheckpointCleanerOwnership) || !errors.Is(err, closeFailure) {
		t.Fatalf("lock replacement = %v", err)
	}
}

func TestC5ClosureMutationAuthorizationRevalidatesEveryRetainedAuthority(t *testing.T) {
	fixture := c5ClosureAuthorizedMutation(t)
	if err := fixture.run.authorizeMutation(fixture.state); err != nil {
		t.Fatalf("canonical mutation authority = %v", err)
	}

	t.Run("required handles", func(t *testing.T) {
		run := &cleanupRun{}
		if err := run.authorizeMutation(nil); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("missing mutation authority = %v", err)
		}
	})

	t.Run("root binding", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		failure := errors.New("root binding revalidation failed")
		fixture.platform.bindingErr = failure
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, failure) {
			t.Fatalf("root binding cut = %v", err)
		}
	})

	t.Run("control replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		failure := errors.New("control replacement")
		fixture.root.openDirectory = func(string, bool) (outputcap.Directory, error) { return nil, failure }
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, failure) {
			t.Fatalf("control cut = %v", err)
		}
	})

	t.Run("checkpoint replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		failure := errors.New("checkpoint replacement")
		fixture.control.openDirectory = func(string, bool) (outputcap.Directory, error) { return nil, failure }
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, failure) {
			t.Fatalf("checkpoint cut = %v", err)
		}
	})

	t.Run("stale ownership proof", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		fixture.run.ownershipProof = []byte("proof-without-record")
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("stale ownership proof = %v", err)
		}
	})

	t.Run("state replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		fixture.stateFile.data = []byte("replacement")
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("state replacement = %v", err)
		}
	})

	t.Run("cleanup lock replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		fixture.cleanupCurrent.same = func(outputcap.FileIdentity) (bool, error) { return false, nil }
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("cleanup lock replacement = %v", err)
		}
	})

	t.Run("coordinator replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		fixture.coordinatorCurrent.same = func(outputcap.FileIdentity) (bool, error) { return false, nil }
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("coordinator replacement = %v", err)
		}
	})

	t.Run("remaining session replacement", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		expected := &c5ClosureFaultFile{}
		current := &c5ClosureFaultFile{same: func(outputcap.FileIdentity) (bool, error) { return false, nil }}
		parent := &c5ClosureFaultDirectory{openFile: func(string, bool, bool) (outputcap.MutableFile, error) {
			return current, nil
		}}
		fixture.run.sessionLocks = []cleanupLockRef{{
			parent: parent, name: legacyresume.SessionLock,
			lock: &c5ClosureFaultLock{file: expected},
		}}
		if err := fixture.run.authorizeMutation(fixture.state); !errors.Is(err, ErrCheckpointCleanerOwnership) {
			t.Fatalf("session lock replacement = %v", err)
		}
	})

	t.Run("mutation counter overflow", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		relative := "approved"
		fixture.run.approved = map[string]outputcap.EntryKind{relative: outputcap.EntryRegularFile}
		state := cleanerState{Mutations: ^uint64(0)}
		called := false
		err := fixture.run.applyRemoval(
			context.Background(), relative, &state, &fixture.state, &CheckpointCleanupReport{},
			func() error { called = true; return nil },
		)
		if !errors.Is(err, ErrCheckpointCleanerState) || called {
			t.Fatalf("mutation overflow: called=%t err=%v", called, err)
		}
	})

	t.Run("canceled before authority use", func(t *testing.T) {
		fixture := c5ClosureAuthorizedMutation(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fixture.run.approved = map[string]outputcap.EntryKind{"approved": outputcap.EntryRegularFile}
		err := fixture.run.applyRemoval(
			ctx, "approved", &cleanerState{}, &fixture.state, &CheckpointCleanupReport{}, func() error { return nil },
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled mutation = %v", err)
		}
	})
}

func TestC5ClosureTreeInspectionPropagatesCapabilityFaultsWithoutMutation(t *testing.T) {
	intentName := strings.Repeat("a", 64)
	openFailure := errors.New("open failed")
	run := &cleanupRun{control: &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return nil, openFailure
	}}, approved: make(map[string]outputcap.EntryKind)}
	if err := run.inspectAndLockLegacySessions(context.Background(), &CheckpointCleanupReport{}); !errors.Is(err, openFailure) {
		t.Fatalf("sessions open = %v", err)
	}

	namesFailure := errors.New("names failed")
	run.control = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return &c5ClosureFaultDirectory{names: func(int) ([]string, error) { return nil, namesFailure }}, nil
	}}
	if err := run.inspectAndLockLegacySessions(context.Background(), &CheckpointCleanupReport{}); !errors.Is(err, namesFailure) {
		t.Fatalf("sessions names = %v", err)
	}

	classificationFailure := errors.New("entry classification failed")
	sessions := &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) { return []string{intentName}, nil },
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, classificationFailure
		},
	}
	run.control = &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return sessions, nil
	}}
	if err := run.inspectAndLockLegacySessions(context.Background(), &CheckpointCleanupReport{}); !errors.Is(err, classificationFailure) {
		t.Fatalf("intent classification = %v", err)
	}

	sessions.classify = func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryRegularFile, true, nil
	}
	report := CheckpointCleanupReport{}
	if err := run.inspectAndLockLegacySessions(context.Background(), &report); err != nil ||
		!c5ClosureHasEntry(
			report, path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, intentName),
			cleanupDetailConflict,
		) {
		t.Fatalf("intent kind report = %+v, %v", report, err)
	}

	sessions.classify = func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryDirectory, true, nil
	}
	sessions.openDirectory = func(string, bool) (outputcap.Directory, error) { return nil, openFailure }
	if err := run.inspectAndLockLegacySessions(context.Background(), &CheckpointCleanupReport{}); !errors.Is(err, openFailure) {
		t.Fatalf("intent open = %v", err)
	}
	intentCloseFailure := errors.New("intent close failed")
	sessions.openDirectory = func(string, bool) (outputcap.Directory, error) {
		return &c5ClosureFaultDirectory{closeErr: intentCloseFailure}, nil
	}
	if err := run.inspectAndLockLegacySessions(context.Background(), &CheckpointCleanupReport{}); !errors.Is(err, intentCloseFailure) {
		t.Fatalf("intent close = %v", err)
	}

	sessionName := strings.Repeat("b", 32)
	intent := &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) { return []string{sessionName}, nil },
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryDirectory, true, nil
		},
		openDirectory: func(string, bool) (outputcap.Directory, error) { return nil, openFailure },
	}
	if err := run.inspectLegacyIntent(context.Background(), intent, "intent", &CheckpointCleanupReport{}); !errors.Is(err, openFailure) {
		t.Fatalf("session open = %v", err)
	}
	sessionCloseFailure := errors.New("session close failed")
	intent.openDirectory = func(string, bool) (outputcap.Directory, error) {
		return &c5ClosureFaultDirectory{closeErr: sessionCloseFailure}, nil
	}
	if err := run.inspectLegacyIntent(context.Background(), intent, "intent", &CheckpointCleanupReport{}); !errors.Is(err, sessionCloseFailure) {
		t.Fatalf("session close = %v", err)
	}

	shardFailure := errors.New("shard classification failed")
	shardCloseFailure := errors.New("shard close failed")
	shard := &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) { return []string{"record"}, nil },
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, shardFailure
		},
		closeErr: shardCloseFailure,
	}
	data := &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) { return []string{"!!", "aa", "ab"}, nil },
		classify: func(name string) (outputcap.EntryKind, bool, error) {
			switch name {
			case "aa":
				return outputcap.EntryRegularFile, true, nil
			case "ab":
				return outputcap.EntryDirectory, true, nil
			default:
				return outputcap.EntryRegularFile, true, nil
			}
		},
		openDirectory: func(string, bool) (outputcap.Directory, error) { return shard, nil },
	}
	parent := &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return data, nil
	}}
	report = CheckpointCleanupReport{}
	err := run.inspectLegacyDataDirectory(
		context.Background(), parent, legacyresume.FilesDirectory, "session/files",
		outputcap.EntryDirectory, legacyFiles, &report,
	)
	if !errors.Is(err, shardFailure) || !errors.Is(err, shardCloseFailure) ||
		!c5ClosureHasEntry(report, "session/files/!!", cleanupDetailUnknown) ||
		!c5ClosureHasEntry(report, "session/files/aa", cleanupDetailConflict) {
		t.Fatalf("shard fault report = %+v, %v", report, err)
	}

	created := &c5ClosureFaultLock{file: &c5ClosureFaultFile{}}
	lockDirectory := &c5ClosureFaultDirectory{acquireLock: func(string, bool) (outputcap.Lock, bool, error) {
		return created, true, nil
	}}
	if err := run.acquireExistingSessionLock(lockDirectory, legacyresume.SessionLock, "session"); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("recreated session lock = %v", err)
	}
	malformed := &c5ClosureFaultLock{file: &c5ClosureFaultFile{size: 1}}
	lockDirectory.acquireLock = func(string, bool) (outputcap.Lock, bool, error) { return malformed, false, nil }
	if err := run.acquireExistingSessionLock(lockDirectory, legacyresume.SessionLock, "session"); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("non-empty session lock = %v", err)
	}
}

func TestC5ClosureSessionInspectionRejectsMalformedLockAndDataAuthorities(t *testing.T) {
	session := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{
		legacyresume.SessionLock:    {kind: outputcap.EntryDirectory, exact: true},
		legacyresume.HeaderRecord:   {kind: outputcap.EntryDirectory, exact: true},
		legacyresume.FilesDirectory: {kind: outputcap.EntryRegularFile, exact: true},
		"aliased":                   {kind: outputcap.EntryRegularFile, exact: false},
	}}
	run := &cleanupRun{approved: make(map[string]outputcap.EntryKind)}
	report := CheckpointCleanupReport{}
	if err := run.inspectLegacySession(context.Background(), session, "session", false, &report); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		path.Join("session", legacyresume.SessionLock),
		path.Join("session", legacyresume.HeaderRecord),
		path.Join("session", legacyresume.FilesDirectory),
		path.Join("session", "aliased"),
	} {
		if !c5ClosureHasEntry(report, relative, cleanupDetailConflict) {
			t.Fatalf("missing conflict for %q: %+v", relative, report.Entries)
		}
	}
	if len(run.approved) != 0 || len(run.sessionLocks) != 0 {
		t.Fatalf("malformed session gained authority: approved=%v locks=%v", run.approved, run.sessionLocks)
	}

	openFailure := errors.New("data directory open failed")
	faultSession := &c5ClosureFaultDirectory{openDirectory: func(string, bool) (outputcap.Directory, error) {
		return nil, openFailure
	}}
	if err := run.inspectLegacyDataDirectory(
		context.Background(), faultSession, legacyresume.FilesDirectory, "session/files",
		outputcap.EntryDirectory, legacyFiles, &report,
	); !errors.Is(err, openFailure) {
		t.Fatalf("data directory reopen = %v", err)
	}
	namesFailure := errors.New("data directory enumeration failed")
	faultSession.openDirectory = func(string, bool) (outputcap.Directory, error) {
		return &c5ClosureFaultDirectory{names: func(int) ([]string, error) { return nil, namesFailure }}, nil
	}
	if err := run.inspectLegacyDataDirectory(
		context.Background(), faultSession, legacyresume.FilesDirectory, "session/files",
		outputcap.EntryDirectory, legacyFiles, &report,
	); !errors.Is(err, namesFailure) {
		t.Fatalf("data directory enumeration = %v", err)
	}
}

func TestC5ClosureStateLoaderRejectsMalformedAndOverflowedRecords(t *testing.T) {
	run, canonical := c5ClosureStateRun(t)
	if _, _, found, err := (&cleanupRun{}).loadState(); err != nil || found {
		t.Fatalf("nil namespace state: found=%t err=%v", found, err)
	}
	classificationFailure := errors.New("state classification failed")
	run.namespace = &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryAbsent, false, classificationFailure
	}}
	if _, _, _, err := run.loadState(); !errors.Is(err, classificationFailure) {
		t.Fatalf("state classification = %v", err)
	}
	run.namespace = &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryDirectory, true, nil
	}}
	if _, _, _, err := run.loadState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("state kind = %v", err)
	}
	run.namespace = c5ClosureStateDirectory(append([]byte(nil), canonical...))
	state, _, found, err := run.loadState()
	if err != nil || !found || !state.Complete {
		t.Fatalf("canonical state: found=%t state=%+v err=%v", found, state, err)
	}
	run.namespace = c5ClosureStateDirectory([]byte("{"))
	if _, _, _, err := run.loadState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("malformed JSON = %v", err)
	}
	overflow := bytes.Repeat([]byte{'x'}, maxCleanerStateBytes+1)
	run.namespace = c5ClosureStateDirectory(overflow)
	if _, _, _, err := run.loadState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("oversized state = %v", err)
	}

	run, _ = c5ClosureStateRun(t)
	state = c5ClosureCanonicalState(run)
	state.RunGeneration = ^uint64(0)
	state.Checksum = stateChecksum(state)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	run.namespace = c5ClosureStateDirectory(encoded)
	if _, _, _, err := run.beginState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("generation overflow = %v", err)
	}
	if err := run.persistState(nil, new([]byte)); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("nil persisted state = %v", err)
	}
}
