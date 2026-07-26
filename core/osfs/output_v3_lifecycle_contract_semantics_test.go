package osfs

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3LifecycleSettlementContractsAndPauseReasons(t *testing.T) {
	for _, test := range []struct {
		job  transfer.JobPauseReason
		file transfer.FilePauseReason
	}{
		{transfer.JobPauseInterrupted, transfer.FilePauseInterrupted},
		{transfer.JobPauseShutdown, transfer.FilePauseShutdown},
		{transfer.JobPauseTransportFailure, transfer.FilePauseTransportFailure},
		{transfer.JobPauseSessionFailure, transfer.FilePauseSessionFailure},
		{transfer.JobPauseOutputFailure, transfer.FilePauseOutputFailure},
	} {
		if actual := filePauseReasonForJob(test.job); actual != test.file {
			t.Fatalf("job pause reason %v mapped to %v, want %v", test.job, actual, test.file)
		}
	}

	if _, err := (*filesystemOutputSession)(nil).PauseJob(
		context.Background(), transfer.JobPauseInterrupted,
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("nil session pause error = %v", err)
	}
	if _, err := (&filesystemOutputSession{}).PauseJob(
		context.Background(), transfer.JobPauseReason(0xff),
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid pause reason error = %v", err)
	}
	if _, err := (*filesystemOutputSession)(nil).CompleteJob(
		context.Background(), transfer.JobSucceeded,
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("nil session completion error = %v", err)
	}
	if _, err := (&filesystemOutputSession{}).CompleteJob(
		context.Background(), transfer.JobOutcome(0xff),
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid completion outcome error = %v", err)
	}

	if err := (*filesystemOutputSession)(nil).beginSettlement(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil begin settlement error = %v", err)
	}
	settling := &filesystemOutputSession{}
	if err := settling.beginSettlement(); err != nil {
		t.Fatal(err)
	}
	settling.endSettlement()
	if err := settling.beginSettlement(); !errors.Is(err, errOutputSessionClosed) {
		t.Fatalf("second settlement error = %v", err)
	}
	for _, unavailable := range []*filesystemOutputSession{{closed: true}, {poisoned: true}} {
		if err := unavailable.beginSettlement(); !errors.Is(err, errOutputSessionClosed) {
			t.Fatalf("unavailable settlement error = %v", err)
		}
	}

	injected := errors.New("shard close failed")
	if err := outputV3CloseShardFault(nil); err != nil {
		t.Fatalf("nil shard close fault = %v", err)
	}
	shardErr := outputV3CloseShardFault(injected)
	if !errors.Is(shardErr, injected) {
		t.Fatalf("shard close fault = %v", shardErr)
	}
	outputV3SemanticRequireFault(t, shardErr, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
}

func TestOutputV3InstallLifecycleAdoptsVerifiedGenerationBeforeCleanupFailure(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)

	injected := errors.New("lifecycle temporary close failed")
	createdFaults := outputV3PublicationFileFaults{closeErr: injected}
	original := session.sessionDir
	session.sessionDir = &outputV3PublicationDirectory{
		outputV3Directory: original,
		faults: &outputV3PublicationDirectoryFaults{
			createdFaults: &createdFaults,
		},
	}

	err := session.installLifecycle(resumestate.SessionPausing)
	if !errors.Is(err, injected) {
		t.Fatalf("lifecycle install error = %v, want injected cleanup failure", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
	if session.stateSnapshot().Header().Lifecycle() != resumestate.SessionPausing {
		t.Fatalf("in-memory lifecycle = %v, want adopted pausing generation",
			session.stateSnapshot().Header().Lifecycle())
	}
	session.mu.Lock()
	poisoned := session.poisoned
	session.mu.Unlock()
	if !poisoned {
		t.Fatal("adopted lifecycle cleanup failure did not poison the current owner")
	}
}

func TestOutputV3CompleteJobRejectsCanceledAndActiveOwnership(t *testing.T) {
	t.Run("canceled", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, false, 0)
		session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
		defer v3RecoveryCloseSession(t, session)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if settlement, err := session.CompleteJob(ctx, transfer.JobSucceeded); !errors.Is(err, context.Canceled) || settlement.Kind() != 0 {
			t.Fatalf("canceled completion = (%+v, %v)", settlement, err)
		}
	})

	t.Run("active file transaction", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, true, 1)
		session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
		defer v3RecoveryCloseSession(t, session)
		start, err := session.BeginFile(context.Background(), v3RecoveryOutputFile(t, session, selection, 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, ok := start.Transaction(); !ok {
			t.Fatal("admitted file did not start a transaction")
		}

		settlement, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
		if !errors.Is(err, transfer.ErrOutputContract) || settlement.Kind() != 0 {
			t.Fatalf("completion with active transaction = (%+v, %v)", settlement, err)
		}
		outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultContract)
	})
}

func TestOutputV3FailOwnerSettlementPreservesCauseAndCloseFailure(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)
	cause := errors.New("settlement failed")
	closeFailure := errors.New("owner close failed")
	session.stagesDir = &outputV3LifecycleCloseFaultDirectory{
		outputV3Directory: session.stagesDir,
		closeErr:          closeFailure,
	}

	err := session.failOwnerSettlement(cause)
	if !errors.Is(err, cause) || !errors.Is(err, closeFailure) {
		t.Fatalf("failed owner settlement error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
}

func TestOutputV3AcquireTerminalLocksClassifiesEveryAuthorityBoundary(t *testing.T) {
	injected := errors.New("terminal lock transition failed")
	for _, test := range []struct {
		name      string
		configure func(*filesystemOutputSession)
		scope     transfer.OutputFaultScope
		code      transfer.OutputFaultCode
		cause     error
	}{
		{
			name: "release live session lock",
			configure: func(session *filesystemOutputSession) {
				session.sessionLock = &outputV3LifecycleCloseFaultLock{
					outputV3Lock: session.sessionLock, closeErr: injected,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "coordinator IO",
			configure: func(session *filesystemOutputSession) {
				session.control.directory = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.control.directory, acquireErr: injected,
				}
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "coordinator busy",
			configure: func(session *filesystemOutputSession) {
				session.control.directory = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.control.directory, acquireErr: errOutputV3LockBusy,
				}
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultOwnership, cause: errOutputSessionActive,
		},
		{
			name: "coordinator recreated",
			configure: func(session *filesystemOutputSession) {
				session.control.directory = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.control.directory, forceCreated: true,
				}
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultNamespaceUnsafe, cause: errOutputRootUnsafe,
		},
		{
			name: "session lock IO",
			configure: func(session *filesystemOutputSession) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.sessionDir, acquireErr: injected,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "session lock busy",
			configure: func(session *filesystemOutputSession) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.sessionDir, acquireErr: errOutputV3LockBusy,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultOwnership, cause: errOutputSessionActive,
		},
		{
			name: "session lock recreated",
			configure: func(session *filesystemOutputSession) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.sessionDir, forceCreated: true,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultNamespaceUnsafe, cause: errOutputIntentUnsafe,
		},
		{
			name: "fixed intent cannot be reopened",
			configure: func(session *filesystemOutputSession) {
				session.control.sessions = &outputV3LifecycleLockDirectory{
					outputV3Directory: session.control.sessions,
					openName:          resumestate.ResumeNamespaceName(session.resumeIntent),
					openErr:           injected,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultNamespaceUnsafe, cause: injected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
			defer v3RecoveryCloseSession(t, session)
			test.configure(session)

			coordinator, err := session.acquireTerminalLocks()
			if coordinator != nil {
				_ = coordinator.Close()
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("terminal lock transition error = %v", err)
			}
			outputV3SemanticRequireFault(t, err, test.scope, test.code)
		})
	}
}

func TestOutputV3InspectAndRemoveEmptyShardsPreservesAmbiguity(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)

	createLifecycleShard(t, session.stagesDir, "0a", false)
	createLifecycleShard(t, session.anchorsDir, "0b", true)
	createLifecycleShard(t, session.filesDir, "opaque", false)
	attention, err := session.inspectAndRemoveEmptyShards()
	if err != nil || !attention {
		t.Fatalf("shard cleanup = (attention=%t, %v)", attention, err)
	}
	if kind, err := session.stagesDir.ObserveEntry("0a"); err != nil || kind != outputV3EntryAbsent {
		t.Fatalf("empty canonical shard after cleanup = (%v, %v)", kind, err)
	}
	if kind, err := session.anchorsDir.ObserveEntry("0b"); err != nil || kind != outputV3EntryDirectory {
		t.Fatalf("non-empty canonical shard after cleanup = (%v, %v)", kind, err)
	}
	if kind, err := session.filesDir.ObserveEntry("opaque"); err != nil || kind != outputV3EntryDirectory {
		t.Fatalf("ambiguous shard after cleanup = (%v, %v)", kind, err)
	}
}

func TestOutputV3InspectAndRemoveEmptyShardsClassifiesIOCuts(t *testing.T) {
	injected := errors.New("shard cleanup failed")
	for _, test := range []struct {
		name      string
		configure func(*testing.T, *filesystemOutputSession)
	}{
		{
			name: "enumerate parent",
			configure: func(_ *testing.T, session *filesystemOutputSession) {
				session.stagesDir = &outputV3LifecycleShardDirectory{
					outputV3Directory: session.stagesDir, namesErr: injected,
				}
			},
		},
		{
			name: "open shard",
			configure: func(t *testing.T, session *filesystemOutputSession) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					outputV3Directory: session.stagesDir, openName: "0a", openErr: injected,
				}
			},
		},
		{
			name: "enumerate shard",
			configure: func(t *testing.T, session *filesystemOutputSession) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					outputV3Directory: session.stagesDir,
					childNamesErr:     map[string]error{"0a": injected},
				}
			},
		},
		{
			name: "remove sync and close shard",
			configure: func(t *testing.T, session *filesystemOutputSession) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					outputV3Directory: session.stagesDir,
					removeErr:         map[string]error{"0a": injected},
					syncErr:           injected,
					childCloseErr:     map[string]error{"0a": injected},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
			defer v3RecoveryCloseSession(t, session)
			test.configure(t, session)

			attention, err := session.inspectAndRemoveEmptyShards()
			if attention || !errors.Is(err, injected) {
				t.Fatalf("faulted shard cleanup = (attention=%t, %v)", attention, err)
			}
			outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
		})
	}
}

type outputV3LifecycleCloseFaultDirectory struct {
	outputV3Directory
	closeErr error
}

func (directory *outputV3LifecycleCloseFaultDirectory) Close() error {
	return errors.Join(directory.outputV3Directory.Close(), directory.closeErr)
}

type outputV3LifecycleCloseFaultLock struct {
	outputV3Lock
	closeErr error
	closed   bool
}

func (lock *outputV3LifecycleCloseFaultLock) Close() error {
	if lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(lock.outputV3Lock.Close(), lock.closeErr)
}

type outputV3LifecycleLockDirectory struct {
	outputV3Directory
	acquireErr   error
	forceCreated bool
	openName     string
	openErr      error
}

func (directory *outputV3LifecycleLockDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputV3Lock, bool, error) {
	if directory.acquireErr != nil {
		return nil, false, directory.acquireErr
	}
	lock, created, err := directory.outputV3Directory.AcquireLock(name, existingOnly)
	if err == nil && directory.forceCreated {
		created = true
	}
	return lock, created, err
}

func (directory *outputV3LifecycleLockDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	return directory.outputV3Directory.OpenDirectory(name, private)
}

type outputV3LifecycleShardDirectory struct {
	outputV3Directory
	namesErr      error
	openName      string
	openErr       error
	removeErr     map[string]error
	syncErr       error
	childNamesErr map[string]error
	childCloseErr map[string]error
	closeErr      error
}

func (directory *outputV3LifecycleShardDirectory) Names(limit int) ([]string, error) {
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	return directory.outputV3Directory.Names(limit)
}

func (directory *outputV3LifecycleShardDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3LifecycleShardDirectory{
		outputV3Directory: opened,
		namesErr:          directory.childNamesErr[name],
		closeErr:          directory.childCloseErr[name],
	}, nil
}

func (directory *outputV3LifecycleShardDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	if err := directory.removeErr[name]; err != nil {
		return err
	}
	if wrapped, ok := expected.(*outputV3LifecycleShardDirectory); ok {
		expected = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.RemoveDirectory(name, expected)
}

func (directory *outputV3LifecycleShardDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.outputV3Directory.Sync()
}

func (directory *outputV3LifecycleShardDirectory) Close() error {
	return errors.Join(directory.outputV3Directory.Close(), directory.closeErr)
}

func createLifecycleShard(t *testing.T, parent outputV3Directory, name string, nonempty bool) {
	t.Helper()
	shard, err := parent.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if nonempty {
		child, err := shard.CreateFile("witness", true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(child.Sync(), shard.Sync(), child.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(shard.Sync(), parent.Sync(), shard.Close()); err != nil {
		t.Fatal(err)
	}
}
