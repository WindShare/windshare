package outputruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3LifecycleSettlementContractsAndPauseReasons(t *testing.T) {
	t.Parallel()
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

	if _, err := (*Session)(nil).PauseJob(
		context.Background(), transfer.JobPauseInterrupted,
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("nil session pause error = %v", err)
	}
	if _, err := (&Session{}).PauseJob(
		context.Background(), transfer.JobPauseReason(0xff),
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid pause reason error = %v", err)
	}
	if _, err := (*Session)(nil).CompleteJob(
		context.Background(), transfer.JobSucceeded,
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("nil session completion error = %v", err)
	}
	if _, err := (&Session{}).CompleteJob(
		context.Background(), transfer.JobOutcome(0xff),
	); !errors.Is(err, transfer.ErrInvalidOutputSettlement) {
		t.Fatalf("invalid completion outcome error = %v", err)
	}

	if err := (*Session)(nil).beginSettlement(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil begin settlement error = %v", err)
	}
	settling := &Session{}
	if err := settling.beginSettlement(); err != nil {
		t.Fatal(err)
	}
	settling.endSettlement()
	if err := settling.beginSettlement(); !errors.Is(err, outputfault.ErrSessionClosed) {
		t.Fatalf("second settlement error = %v", err)
	}
	for _, unavailable := range []*Session{{closed: true}, {poisoned: true}} {
		if err := unavailable.beginSettlement(); !errors.Is(err, outputfault.ErrSessionClosed) {
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
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)

	injected := errors.New("lifecycle temporary close failed")
	createdFaults := outputV3PublicationFileFaults{closeErr: injected}
	original := session.sessionDir
	session.sessionDir = &outputV3PublicationDirectory{
		Directory: original,
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
	t.Parallel()
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
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	session := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection).Session
	defer v3RecoveryCloseSession(t, session)
	cause := errors.New("settlement failed")
	closeFailure := errors.New("owner close failed")
	session.stagesDir = &outputV3LifecycleCloseFaultDirectory{
		Directory: session.stagesDir,
		closeErr:  closeFailure,
	}

	err := session.failOwnerSettlement(cause)
	if !errors.Is(err, cause) || !errors.Is(err, closeFailure) {
		t.Fatalf("failed owner settlement error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
}

func TestOutputV3AcquireTerminalLocksClassifiesEveryAuthorityBoundary(t *testing.T) {
	t.Parallel()
	injected := errors.New("terminal lock transition failed")
	for _, test := range []struct {
		name      string
		configure func(*Session, *outputV3LifecyclePlatformFaultGate)
		scope     transfer.OutputFaultScope
		code      transfer.OutputFaultCode
		cause     error
	}{
		{
			name: "release live session lock",
			configure: func(session *Session, _ *outputV3LifecyclePlatformFaultGate) {
				session.sessionLock = &outputV3LifecycleCloseFaultLock{
					Lock: session.sessionLock, closeErr: injected,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "coordinator IO",
			configure: func(_ *Session, gate *outputV3LifecyclePlatformFaultGate) {
				gate.coordinatorAcquireErr = injected
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "coordinator busy",
			configure: func(_ *Session, gate *outputV3LifecyclePlatformFaultGate) {
				gate.coordinatorAcquireErr = outputcap.ErrNamespaceLockBusy
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultOwnership, cause: outputfault.ErrSessionActive,
		},
		{
			name: "coordinator recreated",
			configure: func(_ *Session, gate *outputV3LifecyclePlatformFaultGate) {
				gate.coordinatorForceCreated = true
			},
			scope: transfer.OutputFaultRoot, code: transfer.OutputFaultNamespaceUnsafe, cause: outputfault.ErrRootUnsafe,
		},
		{
			name: "session lock IO",
			configure: func(session *Session, _ *outputV3LifecyclePlatformFaultGate) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					Directory: session.sessionDir, acquireErr: injected,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultStateIO, cause: injected,
		},
		{
			name: "session lock busy",
			configure: func(session *Session, _ *outputV3LifecyclePlatformFaultGate) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					Directory: session.sessionDir, acquireErr: outputcap.ErrNamespaceLockBusy,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultOwnership, cause: outputfault.ErrSessionActive,
		},
		{
			name: "session lock recreated",
			configure: func(session *Session, _ *outputV3LifecyclePlatformFaultGate) {
				session.sessionDir = &outputV3LifecycleLockDirectory{
					Directory: session.sessionDir, forceCreated: true,
				}
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultNamespaceUnsafe, cause: outputfault.ErrIntentUnsafe,
		},
		{
			name: "fixed intent cannot be reopened",
			configure: func(session *Session, gate *outputV3LifecyclePlatformFaultGate) {
				gate.sessionsOpenName = resumestate.ResumeNamespaceName(session.resumeIntent)
				gate.sessionsOpenErr = injected
			},
			scope: transfer.OutputFaultSession, code: transfer.OutputFaultNamespaceUnsafe, cause: injected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := &outputV3LifecyclePlatformFaultGate{}
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			authority := v3RecoveryAuthority(t, root, nil)
			nativeFactory := authority.platformFactory
			authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
				platform, err := nativeFactory(path, create)
				if err != nil {
					return nil, err
				}
				return &outputV3LifecyclePlatformFault{Platform: platform, gate: gate}, nil
			}
			session := v3RecoveryOpen(t, authority, root, selection).Session
			defer v3RecoveryCloseSession(t, session)
			test.configure(session, gate)
			gate.enabled = true

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

// outputV3LifecyclePlatformFault decorates the native capability before
// outputnamespace constructs its private control namespace. This keeps the
// test at the same authority boundary as production instead of reaching into
// encapsulated namespace handles.
type outputV3LifecyclePlatformFault struct {
	outputcap.Platform
	gate *outputV3LifecyclePlatformFaultGate
}

type outputV3LifecyclePlatformFaultGate struct {
	enabled                 bool
	coordinatorAcquireErr   error
	coordinatorForceCreated bool
	sessionsOpenName        string
	sessionsOpenErr         error
}

func (platform *outputV3LifecyclePlatformFault) Root() outputcap.Directory {
	if platform == nil {
		return nil
	}
	return outputV3WrapLifecyclePlatformDirectory(platform.Platform.Root(), "", platform.gate)
}

func (platform *outputV3LifecyclePlatformFault) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	if platform == nil || platform.Platform == nil {
		return nil, transfer.ErrInvalidOutputBinding
	}
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return outputV3WrapLifecyclePlatformDirectory(root, "", platform.gate)
		},
	)
}

type outputV3LifecyclePlatformDirectory struct {
	outputcap.Directory
	path string
	gate *outputV3LifecyclePlatformFaultGate
}

func outputV3WrapLifecyclePlatformDirectory(
	directory outputcap.Directory,
	path string,
	gate *outputV3LifecyclePlatformFaultGate,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &outputV3LifecyclePlatformDirectory{Directory: directory, path: path, gate: gate}
}

func unwrapOutputV3LifecyclePlatformDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*outputV3LifecyclePlatformDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *outputV3LifecyclePlatformDirectory) childPath(name string) string {
	if directory == nil || directory.path == "" {
		return name
	}
	return directory.path + "/" + name
}

func (directory *outputV3LifecyclePlatformDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return outputV3WrapLifecyclePlatformDirectory(duplicate, directory.path, directory.gate), nil
}

func (directory *outputV3LifecyclePlatformDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(unwrapOutputV3LifecyclePlatformDirectory(other))
}

func (directory *outputV3LifecyclePlatformDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if directory.gate != nil && directory.gate.enabled &&
		directory.path == resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName &&
		name == directory.gate.sessionsOpenName && directory.gate.sessionsOpenErr != nil {
		return nil, directory.gate.sessionsOpenErr
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return outputV3WrapLifecyclePlatformDirectory(opened, directory.childPath(name), directory.gate), nil
}

func (directory *outputV3LifecyclePlatformDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return outputV3WrapLifecyclePlatformDirectory(created, directory.childPath(name), directory.gate), nil
}

func (directory *outputV3LifecyclePlatformDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		unwrapOutputV3LifecyclePlatformDirectory(candidate), name,
	)
	if err != nil {
		return nil, err
	}
	return outputV3WrapLifecyclePlatformDirectory(installed, directory.childPath(name), directory.gate), nil
}

func (directory *outputV3LifecyclePlatformDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(name, unwrapOutputV3LifecyclePlatformDirectory(expected))
}

func (directory *outputV3LifecyclePlatformDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory.gate != nil && directory.gate.enabled &&
		directory.path == resumestate.ControlDirectoryName && name == resumestate.CoordinatorLockName {
		if directory.gate.coordinatorAcquireErr != nil {
			return nil, false, directory.gate.coordinatorAcquireErr
		}
		lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
		if err == nil && directory.gate.coordinatorForceCreated {
			created = true
		}
		return lock, created, err
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

func TestOutputV3InspectAndRemoveEmptyShardsPreservesAmbiguity(t *testing.T) {
	t.Parallel()
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
	if kind, err := session.stagesDir.ObserveEntry("0a"); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("empty canonical shard after cleanup = (%v, %v)", kind, err)
	}
	if kind, err := session.anchorsDir.ObserveEntry("0b"); err != nil || kind != outputcap.EntryDirectory {
		t.Fatalf("non-empty canonical shard after cleanup = (%v, %v)", kind, err)
	}
	if kind, err := session.filesDir.ObserveEntry("opaque"); err != nil || kind != outputcap.EntryDirectory {
		t.Fatalf("ambiguous shard after cleanup = (%v, %v)", kind, err)
	}
}

func TestOutputV3InspectAndRemoveEmptyShardsClassifiesIOCuts(t *testing.T) {
	t.Parallel()
	injected := errors.New("shard cleanup failed")
	for _, test := range []struct {
		name      string
		configure func(*testing.T, *Session)
	}{
		{
			name: "enumerate parent",
			configure: func(_ *testing.T, session *Session) {
				session.stagesDir = &outputV3LifecycleShardDirectory{
					Directory: session.stagesDir, namesErr: injected,
				}
			},
		},
		{
			name: "open shard",
			configure: func(t *testing.T, session *Session) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					Directory: session.stagesDir, openName: "0a", openErr: injected,
				}
			},
		},
		{
			name: "enumerate shard",
			configure: func(t *testing.T, session *Session) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					Directory:     session.stagesDir,
					childNamesErr: map[string]error{"0a": injected},
				}
			},
		},
		{
			name: "remove sync and close shard",
			configure: func(t *testing.T, session *Session) {
				createLifecycleShard(t, session.stagesDir, "0a", false)
				session.stagesDir = &outputV3LifecycleShardDirectory{
					Directory:     session.stagesDir,
					removeErr:     map[string]error{"0a": injected},
					syncErr:       injected,
					childCloseErr: map[string]error{"0a": injected},
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
	outputcap.Directory
	closeErr error
}

func (directory *outputV3LifecycleCloseFaultDirectory) Close() error {
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

type outputV3LifecycleCloseFaultLock struct {
	outputcap.Lock
	closeErr error
	closed   bool
}

func (lock *outputV3LifecycleCloseFaultLock) Close() error {
	if lock.closed {
		return nil
	}
	lock.closed = true
	return errors.Join(lock.Lock.Close(), lock.closeErr)
}

type outputV3LifecycleLockDirectory struct {
	outputcap.Directory
	acquireErr   error
	forceCreated bool
	openName     string
	openErr      error
}

func (directory *outputV3LifecycleLockDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if directory.acquireErr != nil {
		return nil, false, directory.acquireErr
	}
	lock, created, err := directory.Directory.AcquireLock(name, existingOnly)
	if err == nil && directory.forceCreated {
		created = true
	}
	return lock, created, err
}

func (directory *outputV3LifecycleLockDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	return directory.Directory.OpenDirectory(name, private)
}

type outputV3LifecycleShardDirectory struct {
	outputcap.Directory
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
	return directory.Directory.Names(limit)
}

func (directory *outputV3LifecycleShardDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if name == directory.openName && directory.openErr != nil {
		return nil, directory.openErr
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3LifecycleShardDirectory{
		Directory: opened,
		namesErr:  directory.childNamesErr[name],
		closeErr:  directory.childCloseErr[name],
	}, nil
}

func (directory *outputV3LifecycleShardDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	if err := directory.removeErr[name]; err != nil {
		return err
	}
	if wrapped, ok := expected.(*outputV3LifecycleShardDirectory); ok {
		expected = wrapped.Directory
	}
	return directory.Directory.RemoveDirectory(name, expected)
}

func (directory *outputV3LifecycleShardDirectory) Sync() error {
	if directory.syncErr != nil {
		return directory.syncErr
	}
	return directory.Directory.Sync()
}

func (directory *outputV3LifecycleShardDirectory) Close() error {
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

func createLifecycleShard(t *testing.T, parent outputcap.Directory, name string, nonempty bool) {
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
