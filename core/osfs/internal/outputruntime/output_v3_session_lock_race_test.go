package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const v3RecoveryLockGateTimeout = 10 * time.Second

func TestOutputV3OpenerRereadsHeaderAndCleansTemporariesOnlyAfterSessionLock(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	incumbentAuthority := v3RecoveryAuthority(t, root, sessionIDs)
	incumbent := v3RecoveryOpen(t, incumbentAuthority, root, selection).Session
	initialHeader := incumbent.state.Header()

	updated, err := incumbent.state.WithLifecycle(resumestate.SessionPausing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := resumestate.EncodeHeader(updated.Header())
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x77}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	temporaryName, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := incumbent.sessionDir.CreateFile(temporaryName, true, int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	written, err := temporary.WriteAt(encoded, 0)
	if err != nil || written != len(encoded) {
		t.Fatalf("write header update cut = (%d, %v), want %d", written, err, len(encoded))
	}
	if err := errors.Join(temporary.Sync(), incumbent.sessionDir.Sync(), temporary.Close()); err != nil {
		t.Fatal(err)
	}
	temporaryPath := filepath.Join(v3RecoverySessionPath(root, selection, incumbent.SessionID()), temporaryName)

	gate := &v3RecoverySessionLockGate{
		ready: make(chan struct{}), release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	openerAuthority := v3RecoveryAuthority(t, root, sessionIDs)
	openerAuthority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapPlatform(platform, gate), nil
	}
	type openResult struct {
		opened v3OpenedSelection
		err    error
	}
	result := make(chan openResult, 1)
	go func() {
		opened, err := v3OpenSelection(context.Background(), openerAuthority, selection)
		result <- openResult{opened: opened, err: err}
	}()

	select {
	case <-gate.ready:
	case <-time.After(v3RecoveryLockGateTimeout):
		t.Fatal("opener did not reach the session-lock acquisition gate")
	}
	_, temporaryErrBeforeLock := os.Stat(temporaryPath)
	settlement, err := incumbent.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Kind() != transfer.JobPaused {
		t.Fatalf("incumbent pause settlement = %v, want %v", settlement.Kind(), transfer.JobPaused)
	}
	close(gate.release)

	var opened openResult
	select {
	case opened = <-result:
	case <-time.After(v3RecoveryLockGateTimeout):
		t.Fatal("opener did not finish after the incumbent released its lock")
	}
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	if temporaryErrBeforeLock != nil {
		t.Fatalf("header temporary was removed before session-lock acquisition: %v", temporaryErrBeforeLock)
	}
	if _, err := os.Stat(temporaryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("header temporary after locked recovery stat error = %v, want not exist", err)
	}
	encodedDisk, err := outputnamespace.ReadRecord(
		opened.opened.Session.sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	diskHeader, err := resumestate.DecodeHeader(encodedDisk)
	if err != nil {
		t.Fatal(err)
	}
	if diskHeader != opened.opened.Session.state.Header() {
		t.Fatalf("returned header generation %d differs from disk generation %d",
			opened.opened.Session.state.Header().StateGeneration(), diskHeader.StateGeneration())
	}
	if diskHeader.Lifecycle() != resumestate.SessionActive ||
		diskHeader.StateGeneration() <= initialHeader.StateGeneration() {
		t.Fatalf("reopened header = lifecycle %v generation %d, want newer Active authority",
			diskHeader.Lifecycle(), diskHeader.StateGeneration())
	}
	v3RecoveryCloseSession(t, opened.opened.Session)
}

type v3RecoverySessionLockGate struct {
	ready       chan struct{}
	release     chan struct{}
	acquireOnce sync.Once
}

type v3RecoveryGatePlatform struct {
	outputcap.Platform
	root outputcap.Directory
}

func v3RecoveryWrapPlatform(platform outputcap.Platform, gate *v3RecoverySessionLockGate) outputcap.Platform {
	return &v3RecoveryGatePlatform{
		Platform: platform,
		root:     v3RecoveryWrapDirectory(platform.Root(), gate),
	}
}

func (platform *v3RecoveryGatePlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryGatePlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryGateDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return v3RecoveryWrapDirectory(root, decorated.gate)
		},
	)
}

type v3RecoveryGateDirectory struct {
	outputcap.Directory
	gate *v3RecoverySessionLockGate
}

func v3RecoveryWrapDirectory(
	directory outputcap.Directory,
	gate *v3RecoverySessionLockGate,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryGateDirectory{Directory: directory, gate: gate}
}

func v3RecoveryUnwrapDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryGateDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *v3RecoveryGateDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapDirectory(duplicate, directory.gate), err
}

func (directory *v3RecoveryGateDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapDirectory(other))
}

func (directory *v3RecoveryGateDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryGateDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryGateDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapDirectory(created, directory.gate), err
}

func (directory *v3RecoveryGateDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapDirectory(candidate), name,
	)
	return v3RecoveryWrapDirectory(installed, directory.gate), err
}

func (directory *v3RecoveryGateDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(name, v3RecoveryUnwrapDirectory(expected))
}

func (directory *v3RecoveryGateDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if name == resumestate.SessionLockName {
		directory.gate.acquireOnce.Do(func() {
			close(directory.gate.ready)
			<-directory.gate.release
		})
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}
