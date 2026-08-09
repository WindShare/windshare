package checkpointcleaner

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"path"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type cleanupCoverageEntry struct {
	kind     outputcap.EntryKind
	closeErr error
}

func (entry *cleanupCoverageEntry) Kind() outputcap.EntryKind { return entry.kind }
func (entry *cleanupCoverageEntry) Close() error              { return entry.closeErr }

func TestCleanupAdmissionTurnsUnknownLegacyOwnershipIntoAttention(t *testing.T) {
	if admission := admitCleanupState(cleanerState{}, false, legacyObservation{}); admission != cleanupStateIdle {
		t.Fatalf("empty cleanup admission = %d", admission)
	}
	if admission := admitCleanupState(cleanerState{Complete: false}, true, legacyObservation{}); admission != cleanupStateResume {
		t.Fatalf("resumed cleanup admission = %d", admission)
	}
	if admission := admitCleanupState(cleanerState{}, false, legacyObservation{controlRecord: true}); admission != cleanupStateFresh {
		t.Fatalf("fresh cleanup admission = %d", admission)
	}

	for name, test := range map[string]struct {
		admission cleanupStateAdmission
		legacy    legacyObservation
		seed      func(*cleanupRun)
		wantPath  string
	}{
		"resumed without coordinator": {
			admission: cleanupStateResume, legacy: legacyObservation{controlRecord: true},
			wantPath: path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
		},
		"resumed tree without ownership": {
			admission: cleanupStateResume,
			legacy:    legacyObservation{coordinatorLock: true, sessionsDirectory: true},
			seed: func(run *cleanupRun) {
				run.coordinator = &c5ClosureFaultLock{file: &c5ClosureFaultFile{}}
			},
			wantPath: path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
		},
		"fresh without ownership": {
			admission: cleanupStateFresh, legacy: legacyObservation{coordinatorLock: true},
			wantPath: path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
		},
		"fresh without coordinator": {
			admission: cleanupStateFresh, legacy: legacyObservation{controlRecord: true},
			wantPath: path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := &cleanupRun{legacy: test.legacy, control: &c5ClosureFaultDirectory{}}
			if test.seed != nil {
				test.seed(run)
			}
			report := CheckpointCleanupReport{}
			authorized, err := run.acquireLegacyMaintenanceAuthority(test.admission, &report)
			if err != nil || authorized || !report.NeedsAttention() ||
				len(report.Entries) == 0 || report.Entries[0].RelativePath != test.wantPath {
				t.Fatalf("cleanup admission = (authorized=%t, report=%+v, %v)", authorized, report, err)
			}
		})
	}
	if authorized, err := (&cleanupRun{}).acquireLegacyMaintenanceAuthority(cleanupStateIdle, nil); err != nil || authorized {
		t.Fatalf("idle maintenance authority = (%t, %v)", authorized, err)
	}
	addRetained(nil, "ignored", cleanupDetailUnknown)
}

func TestCleanupTreeMutationRejectsEveryChangedObservationBeforeRemoval(t *testing.T) {
	state := cleanerState{}
	previous := []byte("state")
	report := CheckpointCleanupReport{}
	run := &cleanupRun{approved: map[string]outputcap.EntryKind{}}
	relative := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run.removeTreeEntry(canceled, &c5ClosureFaultDirectory{}, relative, "entry", &state, &previous, &report); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled tree cleanup error = %v", err)
	}
	classifyFailure := errors.New("classification failed")
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, classifyFailure
		},
	}, relative, "entry", &state, &previous, &report); !errors.Is(err, classifyFailure) {
		t.Fatalf("tree classification error = %v", err)
	}
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, true, nil
		},
	}, relative, "entry", &state, &previous, &report); err != nil {
		t.Fatalf("already absent tree entry = %v", err)
	}
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, false, nil
		},
	}, relative, "entry", &state, &previous, &report); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("inexact tree entry error = %v", err)
	}
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
	}, relative, "unapproved", &state, &previous, &report); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("unapproved tree entry error = %v", err)
	}

	lockPath := path.Join(relative, legacyresume.SessionLock)
	run.approved[lockPath] = outputcap.EntryRegularFile
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
	}, relative, legacyresume.SessionLock, &state, &previous, &report); err != nil {
		t.Fatalf("held session lock was removed: %v", err)
	}

	otherPath := path.Join(relative, "other")
	run.approved[otherPath] = outputcap.EntryOther
	if err := run.removeTreeEntry(context.Background(), &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryOther, true, nil
		},
	}, relative, "other", &state, &previous, &report); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("non-file tree entry error = %v", err)
	}

	listFailure := errors.New("tree enumeration failed")
	if err := run.removeTreeContents(context.Background(), &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) { return nil, listFailure },
	}, relative, &state, &previous, &report); !errors.Is(err, listFailure) {
		t.Fatalf("tree enumeration error = %v", err)
	}
}

func TestCleanupDirectoryRemovalRetainsMissingChangedAndNonemptyTrees(t *testing.T) {
	run := &cleanupRun{approved: map[string]outputcap.EntryKind{}}
	state := cleanerState{}
	previous := []byte("state")
	report := CheckpointCleanupReport{}
	relative := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory)

	if err := run.removeDirectoryEntry(
		context.Background(), &c5ClosureFaultDirectory{}, "sessions", relative, &state, &previous, &report,
	); err != nil {
		t.Fatalf("already absent directory error = %v", err)
	}
	openFailure := errors.New("entry open failed")
	if err := run.removeDirectoryEntry(context.Background(), &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return nil, openFailure },
	}, "sessions", relative, &state, &previous, &report); !errors.Is(err, openFailure) {
		t.Fatalf("directory open error = %v", err)
	}
	if err := run.removeDirectoryEntry(context.Background(), &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) {
			return &cleanupCoverageEntry{kind: outputcap.EntryRegularFile}, nil
		},
	}, "sessions", relative, &state, &previous, &report); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("changed directory kind error = %v", err)
	}
	pinnedFailure := errors.New("pinned open failed")
	entry := &cleanupCoverageEntry{kind: outputcap.EntryDirectory}
	if err := run.removeDirectoryEntry(context.Background(), &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return nil, pinnedFailure
		},
	}, "sessions", relative, &state, &previous, &report); !errors.Is(err, pinnedFailure) {
		t.Fatalf("pinned directory error = %v", err)
	}
	namesFailure := errors.New("directory enumeration failed")
	if err := run.removeDirectoryEntry(context.Background(), &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return &c5ClosureFaultDirectory{names: func(int) ([]string, error) { return nil, namesFailure }}, nil
		},
	}, "sessions", relative, &state, &previous, &report); !errors.Is(err, namesFailure) {
		t.Fatalf("directory enumeration error = %v", err)
	}
	if err := run.removeDirectoryEntry(context.Background(), &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return &c5ClosureFaultDirectory{names: func(int) ([]string, error) { return []string{"foreign"}, nil }}, nil
		},
	}, "sessions", relative, &state, &previous, &report); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nonempty directory error = %v", err)
	}
	if err := run.removeCoordinator(context.Background(), &state, &previous, &report); err != nil {
		t.Fatalf("absent coordinator removal error = %v", err)
	}
	run.control = &c5ClosureFaultDirectory{}
	if err := run.removeLegacySessions(context.Background(), &state, &previous, &report); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing sessions tree error = %v", err)
	}
}

func TestCleanupStateRejectsOverflowAndOversizedPersistence(t *testing.T) {
	run, _ := c5ClosureStateRun(t)
	state := c5ClosureCanonicalState(run)
	state.RunGeneration = math.MaxUint64
	state.Checksum = stateChecksum(state)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	run.namespace = c5ClosureStateDirectory(encoded)
	run.cleanupLock = &c5ClosureFaultLock{file: &c5ClosureFaultFile{}}
	if _, _, _, err := run.beginState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("cleanup generation overflow error = %v", err)
	}

	oversized := state
	oversized.RunGeneration = 1
	oversized.RootIdentity = make([]byte, maxCleanerStateBytes)
	previous := []byte(nil)
	if err := run.persistState(&oversized, &previous); !errors.Is(err, ErrCheckpointCleanerLimit) {
		t.Fatalf("oversized cleanup state error = %v", err)
	}
	if err := run.persistState(nil, &previous); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("nil cleanup state error = %v", err)
	}
}

func TestCleanerRunAcceptsNilContextButStillRequiresCertifiedGuard(t *testing.T) {
	acquireFailure := errors.New("guard unavailable")
	platform := &c5ClosurePlatform{
		root:       &c5ClosureFaultDirectory{},
		acquireErr: acquireFailure,
	}
	cleaner := &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: legacyresume.NativeFilesystemBackend,
	}}
	if _, err := cleaner.Run(nil); !errors.Is(err, acquireFailure) {
		t.Fatalf("nil-context cleaner error = %v", err)
	}

	nilGuardCleaner := &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
		Platform:  &c5ClosurePlatform{root: &c5ClosureFaultDirectory{}},
		BackendID: legacyresume.NativeFilesystemBackend,
	}}
	if _, err := nilGuardCleaner.acquireCertifiedRun(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil guard error = %v", err)
	}
	nilRootGuard := &c5ClosureStaticGuard{}
	nilRootCleaner := &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
		Platform:  &c5ClosurePlatform{root: &c5ClosureFaultDirectory{}, guard: nilRootGuard},
		BackendID: legacyresume.NativeFilesystemBackend,
	}}
	if _, err := nilRootCleaner.acquireCertifiedRun(); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil guarded root error = %v", err)
	}
}

func TestTreeDirectoryCleanupRetainsEveryUncertainIdentity(t *testing.T) {
	state := cleanerState{}
	previous := []byte("state")
	report := CheckpointCleanupReport{}
	relative := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, "session")
	call := func(run *cleanupRun, parent outputcap.Directory) error {
		return run.removeTreeDirectory(
			context.Background(), parent, "child", relative, &state, &previous, &report,
		)
	}

	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{}); err != nil {
		t.Fatalf("missing tree cleanup error = %v", err)
	}
	openFailure := errors.New("tree entry unavailable")
	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return nil, openFailure },
	}); !errors.Is(err, openFailure) {
		t.Fatalf("tree open error = %v", err)
	}
	entry := &cleanupCoverageEntry{kind: outputcap.EntryDirectory}
	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) {
			return &cleanupCoverageEntry{kind: outputcap.EntryRegularFile}, nil
		},
	}); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("changed tree kind error = %v", err)
	}
	pinnedFailure := errors.New("pinned tree unavailable")
	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return nil, pinnedFailure
		},
	}); !errors.Is(err, pinnedFailure) {
		t.Fatalf("pinned tree error = %v", err)
	}

	lockExpected := &c5ClosureFaultFile{}
	lockCurrent := &c5ClosureFaultFile{same: func(outputcap.File) (bool, error) { return true, nil }}
	lock := &c5ClosureFaultLock{file: lockExpected}
	referenceParent := &c5ClosureFaultDirectory{}
	lockedRun := &cleanupRun{sessionLocks: []cleanupLockRef{{
		parent: referenceParent, name: legacyresume.SessionLock,
		path: path.Join(relative, legacyresume.SessionLock), lock: lock,
	}}}
	child := &c5ClosureFaultDirectory{
		sameDirectory: func(outputcap.Directory) (bool, error) { return false, nil },
		openFile:      func(string, bool, bool) (outputcap.File, error) { return lockCurrent, nil },
	}
	if err := call(lockedRun, &c5ClosureFaultDirectory{
		openEntry:  func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) { return child, nil },
	}); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("changed session directory error = %v", err)
	}

	namesFailure := errors.New("tree enumeration unavailable")
	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{
		openEntry: func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) {
			return &c5ClosureFaultDirectory{names: func(int) ([]string, error) { return nil, namesFailure }}, nil
		},
	}); !errors.Is(err, namesFailure) {
		t.Fatalf("tree enumeration error = %v", err)
	}

	namesCalls := 0
	changedPlan := errors.New("remaining entry classification unavailable")
	plannedChild := &c5ClosureFaultDirectory{
		names: func(int) ([]string, error) {
			namesCalls++
			if namesCalls == 1 {
				return nil, nil
			}
			return []string{"retained"}, nil
		},
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, false, changedPlan
		},
	}
	if err := call(&cleanupRun{}, &c5ClosureFaultDirectory{
		openEntry:  func(string) (outputcap.CurrentEntryReference, error) { return entry, nil },
		openPinned: func(outputcap.CurrentEntryReference, bool) (outputcap.Directory, error) { return plannedChild, nil },
	}); !errors.Is(err, changedPlan) {
		t.Fatalf("changed remaining plan error = %v", err)
	}
}

func TestCleanerStateLoadDistinguishesStorageUncertaintyFromAbsence(t *testing.T) {
	run, _ := c5ClosureStateRun(t)
	classificationFailure := errors.New("state classification unavailable")
	run.namespace = &c5ClosureFaultDirectory{classify: func(string) (outputcap.EntryKind, bool, error) {
		return outputcap.EntryAbsent, false, classificationFailure
	}}
	if _, _, _, err := run.beginState(); !errors.Is(err, classificationFailure) {
		t.Fatalf("state classification error = %v", err)
	}

	run.namespace = &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
		openFile: func(string, bool, bool) (outputcap.File, error) { return nil, fs.ErrNotExist },
	}
	if _, _, _, err := run.loadState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("disappeared state error = %v", err)
	}

	readFailure := errors.New("state read unavailable")
	run.namespace = &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryRegularFile, true, nil
		},
		openFile: func(string, bool, bool) (outputcap.File, error) { return nil, readFailure },
	}
	if _, _, _, err := run.loadState(); !errors.Is(err, readFailure) {
		t.Fatalf("state read error = %v", err)
	}

	oversized := make([]byte, maxCleanerStateBytes+1)
	run.namespace = c5ClosureStateDirectory(oversized)
	if _, _, _, err := run.loadState(); !errors.Is(err, ErrCheckpointCleanerState) {
		t.Fatalf("oversized state error = %v", err)
	}
}

func TestApplyRemovalStopsBeforeMutationOnOverflowOrFailure(t *testing.T) {
	removalFailure := errors.New("removal failed")
	for name, test := range map[string]struct {
		mutations uint64
		remove    func() error
		want      error
	}{
		"mutation counter exhausted": {math.MaxUint64, func() error { return nil }, ErrCheckpointCleanerState},
		"removal failed":             {0, func() error { return removalFailure }, removalFailure},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := c5ClosureAuthorizedMutation(t)
			relative := "planned-entry"
			fixture.run.approved[relative] = outputcap.EntryRegularFile
			state := cleanerState{Mutations: test.mutations}
			previous := append([]byte(nil), fixture.state...)
			report := CheckpointCleanupReport{}
			err := fixture.run.applyRemoval(
				context.Background(), relative, &state, &previous, &report, test.remove,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("apply removal error = %v, want %v", err, test.want)
			}
			if report.Removed != 0 {
				t.Fatal("failed removal was reported as durable")
			}
		})
	}
}
