package outputruntime

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3UpdateTemporaryInspectionRejectsUntrustedTargetEvidence(t *testing.T) {
	t.Parallel()
	failure := errors.New("update target observation failed")

	t.Run("target observation failure", func(t *testing.T) {
		session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		directory := &outputV3RecoveryEdgeDirectory{
			Directory: shard,
			observe: func(string) (outputcap.EntryKind, error) {
				return outputcap.EntryAbsent, failure
			},
		}
		_, _, _, err := session.inspectUpdateTemporaryTarget(
			directory, classified, outputcap.EntryRegularFile,
		)
		if !errors.Is(err, failure) {
			t.Fatalf("target observation error = %v", err)
		}
	})

	for _, test := range []struct {
		name       string
		targetKind outputcap.EntryKind
		want       resumestate.UpdateTargetObservation
	}{
		{name: "missing target", targetKind: outputcap.EntryAbsent, want: resumestate.UpdateTargetMissing},
		{name: "non-file target", targetKind: outputcap.EntryDirectory, want: resumestate.UpdateTargetInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
			directory := &outputV3RecoveryEdgeDirectory{
				Directory: shard,
				observe:   func(string) (outputcap.EntryKind, error) { return test.targetKind, nil },
			}
			_, target, _, err := session.inspectUpdateTemporaryTarget(
				directory, classified, outputcap.EntryRegularFile,
			)
			if err != nil || target != test.want {
				t.Fatalf("target inspection = (target=%v, err=%v), want %v", target, err, test.want)
			}
		})
	}

	t.Run("unreadable target", func(t *testing.T) {
		session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		directory := &outputV3RecoveryEdgeDirectory{
			Directory: shard,
			observe:   func(string) (outputcap.EntryKind, error) { return outputcap.EntryRegularFile, nil },
			open: func(string, bool, bool) (outputcap.File, error) {
				return nil, failure
			},
		}
		_, target, _, err := session.inspectUpdateTemporaryTarget(
			directory, classified, outputcap.EntryRegularFile,
		)
		if err != nil || target != resumestate.UpdateTargetInvalid {
			t.Fatalf("unreadable target = (target=%v, err=%v)", target, err)
		}
	})

	t.Run("target close failure", func(t *testing.T) {
		session, _, shard, classified, bound := outputV3UpdateTemporaryCoverageFixture(t)
		directory := outputV3RecoveryEdgeCloseDirectory(shard, failure)
		_, target, inspected, err := session.inspectUpdateTemporaryTarget(
			directory, classified, outputcap.EntryRegularFile,
		)
		if target != resumestate.UpdateTargetValid ||
			inspected.Record().LocatorDigest() != bound.Record().LocatorDigest() ||
			!errors.Is(err, failure) || !outputV3FailureRequiresJobPause(err) {
			t.Fatalf("close-faulted target = (target=%v, locator=%v, err=%v)",
				target, inspected.Record().LocatorDigest(), err)
		}
	})
}

func TestOutputV3ObservationFailureReconciliationRetainsOnlyCanonicalAuthority(t *testing.T) {
	t.Parallel()
	observeFailure := errors.New("temporary observation failed")
	mutationFailure := errors.New("quarantine install failed")
	closeFailure := errors.New("target close failed")

	t.Run("non-temporary entry", func(t *testing.T) {
		session, recordName, shard, _, _ := outputV3UpdateTemporaryCoverageFixture(t)
		classified := resumestate.ClassifyFileShardEntry(recordName.Shard(), recordName.Name())
		attention, err := session.reconcileFileShardObservationFailure(shard, classified, observeFailure)
		if attention || !errors.Is(err, observeFailure) {
			t.Fatalf("non-temporary reconciliation = (attention=%t, err=%v)", attention, err)
		}
	})

	t.Run("missing target", func(t *testing.T) {
		session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		directory := &outputV3RecoveryEdgeDirectory{
			Directory: shard,
			observe:   func(string) (outputcap.EntryKind, error) { return outputcap.EntryAbsent, nil },
		}
		attention, err := session.reconcileFileShardObservationFailure(directory, classified, observeFailure)
		if !attention || err != nil {
			t.Fatalf("missing-target reconciliation = (attention=%t, err=%v)", attention, err)
		}
	})

	t.Run("unreadable target", func(t *testing.T) {
		session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		directory := &outputV3RecoveryEdgeDirectory{
			Directory: shard,
			observe:   func(string) (outputcap.EntryKind, error) { return outputcap.EntryRegularFile, nil },
			open:      func(string, bool, bool) (outputcap.File, error) { return nil, mutationFailure },
		}
		attention, err := session.reconcileFileShardObservationFailure(directory, classified, observeFailure)
		if !attention || err != nil {
			t.Fatalf("unreadable-target reconciliation = (attention=%t, err=%v)", attention, err)
		}
	})

	for _, test := range []struct {
		name       string
		closeError error
	}{
		{name: "install failure"},
		{name: "install and close failure", closeError: closeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, shard, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
			directory := outputV3RecoveryEdgeCloseDirectory(shard, test.closeError)
			directory.create = func(string, bool, int64) (outputcap.File, error) {
				return nil, mutationFailure
			}
			attention, err := session.reconcileFileShardObservationFailure(
				directory, classified, observeFailure,
			)
			if attention || !errors.Is(err, mutationFailure) {
				t.Fatalf("failed quarantine reconciliation = (attention=%t, err=%v)", attention, err)
			}
			if test.closeError != nil && (!errors.Is(err, closeFailure) || !outputV3FailureRequiresJobPause(err)) {
				t.Fatalf("combined quarantine cleanup error = %v", err)
			}
		})
	}
}

func TestOutputV3UpdateTemporaryQuarantineHonorsGenerationCAS(t *testing.T) {
	t.Parallel()
	session, _, shard, classified, bound := outputV3UpdateTemporaryCoverageFixture(t)
	installFailure := errors.New("quarantine state install failed")
	decision, err := resumestate.ReduceUpdateTemporary(
		session.stateSnapshot().NamespaceAuthority(), classified,
		resumestate.UpdateTemporaryEntryUnsafe, resumestate.UpdateTargetValid,
	)
	if err != nil || decision.Action() != resumestate.UpdateTemporaryInstallFileQuarantine {
		t.Fatalf("quarantine decision = (action=%v, err=%v)", decision.Action(), err)
	}

	if attention, err := session.installUpdateTemporaryQuarantine(
		shard, classified, resumestate.BoundFileRecord{}, decision,
	); attention || !errors.Is(err, resumestate.ErrInvalidState) {
		t.Fatalf("invalid quarantine authority = (attention=%t, err=%v)", attention, err)
	}

	quarantined, err := resumestate.ApplyUpdateTemporaryQuarantine(bound, decision)
	if err != nil {
		t.Fatal(err)
	}
	if attention, err := session.installUpdateTemporaryQuarantine(
		shard, classified, quarantined, decision,
	); !attention || err != nil {
		t.Fatalf("already-quarantined target = (attention=%t, err=%v)", attention, err)
	}
	faulted := &outputV3RecoveryEdgeDirectory{
		Directory: shard,
		create: func(string, bool, int64) (outputcap.File, error) {
			return nil, installFailure
		},
	}
	if attention, err := session.installUpdateTemporaryQuarantine(
		faulted, classified, bound, decision,
	); attention || !errors.Is(err, installFailure) {
		t.Fatalf("failed quarantine install = (attention=%t, err=%v)", attention, err)
	}

	if attention, err := session.installUpdateTemporaryQuarantine(
		shard, classified, bound, decision,
	); !attention || err != nil {
		t.Fatalf("quarantine install = (attention=%t, err=%v)", attention, err)
	}
}

func TestOutputV3HeldRecoveryObservationCleanupRequiresPause(t *testing.T) {
	t.Parallel()
	failure := errors.New("held recovery observation close failed")
	if _, err := (&Session{}).holdRecoveredQuarantine(
		transfer.OutputFile{}, fileRecoveryState{}, failure,
	); !errors.Is(err, failure) || !outputV3FailureRequiresJobPause(err) {
		t.Fatalf("held recovery cleanup error = %v", err)
	}
}

func TestOutputV3UnauthorizedUpdateRemovalPreservesTemporary(t *testing.T) {
	t.Parallel()
	closeFailure := errors.New("unauthorized temporary close failed")

	for _, test := range []struct {
		name       string
		closeError error
	}{
		{name: "clean close"},
		{name: "close failure", closeError: closeFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, shard, classified, bound := outputV3UpdateTemporaryCoverageFixture(t)
			temporary := resumestate.UpdateTemporaryName(classified.Locator(), classified.Nonce())
			outputV3CreateStateTemporary(t, shard, temporary.Name())
			directory := outputV3RecoveryEdgeCloseDirectory(shard, test.closeError)
			attention, err := session.removeAndSyncUpdateTemporary(
				temporary.Shard(), directory, temporary.Name(), classified, bound,
				resumestate.UpdateTargetValid, resumestate.UpdateTemporaryDecision{},
			)
			if attention || !errors.Is(err, resumestate.ErrInvalidState) {
				t.Fatalf("unauthorized removal = (attention=%t, err=%v)", attention, err)
			}
			if test.closeError != nil && (!errors.Is(err, closeFailure) || !outputV3FailureRequiresJobPause(err)) {
				t.Fatalf("unauthorized removal cleanup error = %v", err)
			}
			kind, observeErr := shard.ObserveEntry(temporary.Name())
			if observeErr != nil || kind != outputcap.EntryRegularFile {
				t.Fatalf("retained temporary = (kind=%v, err=%v)", kind, observeErr)
			}
		})
	}
}

func TestOutputV3UpdateOpenFailureSeparatesCleanupFromQuarantineAuthority(t *testing.T) {
	t.Parallel()
	openFailure := errors.New("update temporary open failed")
	closeFailure := errors.New("update temporary close failed")

	t.Run("invalid target", func(t *testing.T) {
		result, err := (&Session{}).handleUpdateTemporaryOpenFailure(
			updateTemporaryRecoveryContext{targetObservation: resumestate.UpdateTargetInvalid},
			nil, openFailure,
		)
		if result || !errors.Is(err, openFailure) || outputV3FailureRequiresJobPause(err) {
			t.Fatalf("invalid-target open failure = (result=%t, err=%v)", result, err)
		}
	})

	t.Run("invalid target close failure", func(t *testing.T) {
		result, err := (&Session{}).handleUpdateTemporaryOpenFailure(
			updateTemporaryRecoveryContext{targetObservation: resumestate.UpdateTargetInvalid},
			&outputV3RecoveryEdgeFile{closeErr: closeFailure}, openFailure,
		)
		if result || !errors.Is(err, closeFailure) || !outputV3FailureRequiresJobPause(err) {
			t.Fatalf("invalid-target cleanup failure = (result=%t, err=%v)", result, err)
		}
	})

	t.Run("invalid quarantine authority", func(t *testing.T) {
		_, _, _, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		recovery := updateTemporaryRecoveryContext{
			classified: classified, targetObservation: resumestate.UpdateTargetValid,
		}
		result, err := (&Session{}).handleUpdateTemporaryOpenFailure(
			recovery, &outputV3RecoveryEdgeFile{closeErr: closeFailure}, openFailure,
		)
		if result || !errors.Is(err, resumestate.ErrInvalidState) || !errors.Is(err, closeFailure) {
			t.Fatalf("invalid quarantine with cleanup failure = (result=%t, err=%v)", result, err)
		}

		result, err = (&Session{}).handleUpdateTemporaryOpenFailure(recovery, nil, openFailure)
		if result || !errors.Is(err, resumestate.ErrInvalidState) {
			t.Fatalf("invalid quarantine authority = (result=%t, err=%v)", result, err)
		}
	})

	t.Run("installed quarantine with close failure", func(t *testing.T) {
		session, recordName, shard, classified, bound := outputV3UpdateTemporaryCoverageFixture(t)
		recovery := updateTemporaryRecoveryContext{
			shard: shard, classified: classified, bound: bound,
			targetObservation: resumestate.UpdateTargetValid,
		}
		result, err := session.handleUpdateTemporaryOpenFailure(
			recovery, &outputV3RecoveryEdgeFile{closeErr: closeFailure}, openFailure,
		)
		if result || !errors.Is(err, closeFailure) || !outputV3FailureRequiresJobPause(err) {
			t.Fatalf("installed quarantine cleanup failure = (result=%t, err=%v)", result, err)
		}
		persisted, persistedCloseErr, persistedErr := session.openBoundFileRecord(shard, recordName)
		if persistedCloseErr != nil || persistedErr != nil || persisted.Record().Phase() != resumestate.FileQuarantined {
			t.Fatalf("persisted quarantine = (phase=%v, close=%v, err=%v)",
				persisted.Record().Phase(), persistedCloseErr, persistedErr)
		}
	})
}

func TestOutputV3AmbiguousUpdateQuarantineReportsInstallAndCleanupFailures(t *testing.T) {
	t.Parallel()
	closeFailure := errors.New("ambiguous update close failed")

	t.Run("invalid authority", func(t *testing.T) {
		_, _, _, classified, _ := outputV3UpdateTemporaryCoverageFixture(t)
		recovery := updateTemporaryRecoveryContext{classified: classified}
		result, err := (&Session{}).quarantineAmbiguousUpdateTemporary(recovery, "close update", closeFailure)
		if result || !errors.Is(err, resumestate.ErrInvalidState) || !errors.Is(err, closeFailure) {
			t.Fatalf("invalid ambiguous quarantine = (result=%t, err=%v)", result, err)
		}

		result, err = (&Session{}).quarantineAmbiguousUpdateTemporary(recovery, "close update", nil)
		if result || !errors.Is(err, resumestate.ErrInvalidState) {
			t.Fatalf("invalid ambiguous quarantine without cleanup = (result=%t, err=%v)", result, err)
		}
	})

	t.Run("installed quarantine", func(t *testing.T) {
		session, _, shard, classified, bound := outputV3UpdateTemporaryCoverageFixture(t)
		result, err := session.quarantineAmbiguousUpdateTemporary(
			updateTemporaryRecoveryContext{shard: shard, classified: classified, bound: bound},
			"close update", nil,
		)
		if !result || err != nil {
			t.Fatalf("installed ambiguous quarantine = (result=%t, err=%v)", result, err)
		}
	})
}

func TestOutputV3StateRecoveryHelpersFailClosedWithoutAuthority(t *testing.T) {
	t.Parallel()
	var authority *Authority
	if err := authority.revalidateOutputAdmissionAncestry(outputSelectionAdmission{}); !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("missing admission ancestry error = %v", err)
	}
	_ = authority.namespaceController()
	authority.traceAdoptedStateInstallCut(
		transfer.ResumeIntent{}, transfer.OutputSessionID{}, outputnamespace.StateInstallCut{},
	)
	if got := filesystemOutputStateInstallStage(0); got != 0 {
		t.Fatalf("unknown state-install stage = %v", got)
	}
	if _, err := validateSessionChildren(&outputV3RecoveryEdgeDirectory{
		names: func(int) ([]string, error) { return nil, outputcap.ErrUnsafeNamespace },
	}); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("session-child enumeration error = %v", err)
	}
	if err := (*Session)(nil).shutdownOwner(); err != nil {
		t.Fatalf("nil session shutdown error = %v", err)
	}
}

func TestOutputV3LifecycleRecoveryRejectsTerminalAndInvalidState(t *testing.T) {
	t.Parallel()

	if err := (&Session{}).resumeLifecycle(); err == nil {
		t.Fatal("zero lifecycle resumed")
	}
	if err := (&Session{closed: true}).installLifecycle(resumestate.SessionActive); !errors.Is(err, outputfault.ErrSessionClosed) {
		t.Fatalf("closed lifecycle install error = %v", err)
	}

	session, _, _, _, _ := outputV3UpdateTemporaryCoverageFixture(t)
	terminal, err := session.state.WithLifecycle(resumestate.SessionCompleting)
	if err != nil {
		t.Fatal(err)
	}
	session.state = terminal
	if err := session.resumeLifecycle(); err == nil {
		t.Fatal("terminal lifecycle resumed")
	}
	if err := session.installLifecycle(0); !errors.Is(err, resumestate.ErrInvalidTransition) {
		t.Fatalf("invalid lifecycle install error = %v", err)
	}
}

func TestOutputV3SettledNamespaceRejectsNonRecordsAndMissingInventory(t *testing.T) {
	t.Parallel()
	if attention, err := (*Session)(nil).completeSettledFileEntry(
		nil, nil, "aa", outputV3FileNamespaceEntry{},
	); !attention || err != nil {
		t.Fatalf("non-record settlement = (attention=%t, err=%v)", attention, err)
	}

	digest := resumestate.DigestCanonicalLocator("missing.txt")
	recordName := resumestate.FileRecordName(digest)
	entry := outputV3FileNamespaceEntry{
		name:           recordName.Name(),
		classification: resumestate.ClassifyFileShardEntry(recordName.Shard(), recordName.Name()),
	}
	if attention, err := (*Session)(nil).completeSettledFileEntry(
		map[string]outputV3FileNamespaceRecord{}, nil, recordName.Shard(), entry,
	); !attention || err != nil {
		t.Fatalf("missing inventory settlement = (attention=%t, err=%v)", attention, err)
	}
}

func outputV3UpdateTemporaryCoverageFixture(
	t *testing.T,
) (
	*Session,
	resumestate.ShardedName,
	outputcap.Directory,
	resumestate.ClassifiedFileShardEntry,
	resumestate.BoundFileRecord,
) {
	t.Helper()
	session, _, recordName, shard, temporary := outputV3FileShardFixture(t)
	bound, closeErr, err := session.openBoundFileRecord(shard, recordName)
	if closeErr != nil || err != nil {
		t.Fatal(errors.Join(closeErr, err))
	}
	classified := resumestate.ClassifyFileShardEntry(temporary.Shard(), temporary.Name())
	if classified.Classification() != resumestate.FileShardEntryUpdateTemporary {
		t.Fatalf("fixture temporary classification = %v", classified.Classification())
	}
	return session, recordName, shard, classified, bound
}

type outputV3RecoveryEdgeDirectory struct {
	outputcap.Directory
	observe func(string) (outputcap.EntryKind, error)
	open    func(string, bool, bool) (outputcap.File, error)
	create  func(string, bool, int64) (outputcap.File, error)
	names   func(int) ([]string, error)
}

func (directory *outputV3RecoveryEdgeDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if directory.observe != nil {
		return directory.observe(name)
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *outputV3RecoveryEdgeDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if directory.open != nil {
		return directory.open(name, private, writable)
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *outputV3RecoveryEdgeDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if directory.create != nil {
		return directory.create(name, private, size)
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *outputV3RecoveryEdgeDirectory) Names(limit int) ([]string, error) {
	if directory.names != nil {
		return directory.names(limit)
	}
	return directory.Directory.Names(limit)
}

type outputV3RecoveryEdgeFile struct {
	outputcap.File
	closeErr error
}

func (file *outputV3RecoveryEdgeFile) Close() error {
	if file.File == nil {
		return file.closeErr
	}
	return errors.Join(file.File.Close(), file.closeErr)
}

func outputV3RecoveryEdgeCloseDirectory(
	directory outputcap.Directory,
	closeErr error,
) *outputV3RecoveryEdgeDirectory {
	wrapped := &outputV3RecoveryEdgeDirectory{Directory: directory}
	wrapped.observe = func(name string) (outputcap.EntryKind, error) {
		return directory.ObserveEntry(name)
	}
	wrapped.open = func(name string, private bool, writable bool) (outputcap.File, error) {
		opened, err := directory.OpenFile(name, private, writable)
		if err != nil {
			return opened, err
		}
		return &outputV3RecoveryEdgeFile{File: opened, closeErr: closeErr}, nil
	}
	return wrapped
}
