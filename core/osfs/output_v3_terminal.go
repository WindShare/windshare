package osfs

import (
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var outputTerminalRemovalOrder = []string{
	resumestate.StagesDirectoryName,
	resumestate.AnchorsDirectoryName,
	resumestate.FilesDirectoryName,
	resumestate.SessionLockName,
	resumestate.HeaderRecordName,
}

type outputTerminalLayout struct {
	cut     int
	stages  outputV3Directory
	anchors outputV3Directory
	files   outputV3Directory
	lock    outputV3Lock
}

func (layout *outputTerminalLayout) close() error {
	if layout == nil {
		return nil
	}
	return errors.Join(
		closeOutputV3Lock(layout.lock),
		closeOutputV3Directory(layout.stages),
		closeOutputV3Directory(layout.anchors),
		closeOutputV3Directory(layout.files),
	)
}

func (authority *FilesystemOutputAuthority) recoverTerminalSession(
	platform outputV3Platform,
	control *outputControlNamespace,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	state resumestate.SessionAuthority,
	admission outputSelectionAdmission,
) (bool, error) {
	layout, err := inspectTerminalSessionLayoutWithLock(
		sessionDirectory,
		state.Header(),
		func() (outputV3Lock, error) {
			return authority.acquireRuntimeNativeLock(
				func() (outputV3Lock, bool, error) {
					return sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
				},
				filesystemOutputNativeLockContext{
					resumeIntent: state.Header().ResumeIntent(), sessionID: state.Header().SessionID(),
					selectionIdentity:    state.Header().SelectionIdentity(),
					outputAncestryDigest: filesystemOutputAncestryDigestFromState(state.Header().OutputAncestry()),
					certification:        filesystemOutputCertificationFromState(platform.Certification()),
					scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
				},
				intentOutputFault("acquire terminal-recovery session lock", errOutputIntentUnsafe),
			)
		},
	)
	if err != nil {
		return false, intentOutputFault("inspect terminal session cut", err)
	}
	defer layout.close()
	if err := verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, state.Header()); err != nil {
		return false, err
	}

	session := &filesystemOutputSession{
		owner: authority, platform: platform, control: control,
		intentDir: intentDirectory, sessionDir: sessionDirectory,
		filesDir: layout.files, anchorsDir: layout.anchors, stagesDir: layout.stages,
		sessionLock: layout.lock, state: state,
		sessionID: state.Header().SessionID(), selection: state.Selection(), resumeIntent: state.Header().ResumeIntent(),
		ancestry:         admission.ancestry,
		store:            authority.stateStore(state.Header().ResumeIntent(), state.Header().SessionID()),
		selectedFiles:    admission.files,
		selectedDirs:     admission.dirs,
		objectClaims:     make(map[resumestate.OutputObjectID]resumestate.LocatorDigest),
		duplicateObjects: make(map[resumestate.LocatorDigest]struct{}),
		beginning:        make(map[resumestate.LocatorDigest]struct{}),
		active:           make(map[resumestate.LocatorDigest]*filesystemFileTransaction),
	}

	if state.Header().Lifecycle() == resumestate.SessionCompleting && layout.cut == 0 {
		preflight, err := scanOutputV3FileNamespace(session)
		if err != nil {
			return false, err
		}
		if err := session.adoptFileNamespaceSnapshot(preflight); err != nil {
			return false, err
		}
		attention, err := session.completeFileRecords(preflight)
		if err != nil {
			return false, err
		}
		if !attention {
			attention, err = session.inspectAndRemoveEmptyShards()
			if err != nil {
				return false, err
			}
		}
		if attention {
			if err := session.installLifecycle(resumestate.SessionPausedNeedsAttention); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	return false, recoverTerminalNamespace(
		control, intentDirectory, sessionDirectory, state.Header(), layout,
		state.Header().Lifecycle() == resumestate.SessionDiscarding,
	)
}

func recoverTerminalNamespace(
	control *outputControlNamespace,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	header resumestate.Header,
	layout *outputTerminalLayout,
	discard bool,
) error {
	verifyAuthority := func() error {
		return verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, header)
	}
	for _, child := range []struct {
		name      string
		directory *outputV3Directory
	}{
		{resumestate.StagesDirectoryName, &layout.stages},
		{resumestate.AnchorsDirectoryName, &layout.anchors},
		{resumestate.FilesDirectoryName, &layout.files},
	} {
		if *child.directory == nil {
			continue
		}
		if err := verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, header); err != nil {
			return err
		}
		if discard {
			if err := removePrivateDirectoryContents(*child.directory, 0, verifyAuthority); err != nil {
				return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
			}
		} else {
			remaining, err := (*child.directory).Names(1)
			if err != nil || len(remaining) != 0 {
				return intentOutputFault("verify completing directory is empty", errors.Join(errOutputIntentUnsafe, err))
			}
		}
		if err := verifyAuthority(); err != nil {
			return err
		}
		if err := sessionDirectory.RemoveDirectory(child.name, *child.directory); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := sessionDirectory.Sync(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := (*child.directory).Close(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		*child.directory = nil
	}

	if layout.lock != nil {
		if err := verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, header); err != nil {
			return err
		}
		lockFile := layout.lock.File()
		if lockFile == nil {
			return intentOutputFault("remove terminal session lock", errOutputIntentUnsafe)
		}
		if err := sessionDirectory.RemoveFile(resumestate.SessionLockName, lockFile); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := sessionDirectory.Sync(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		if err := layout.lock.Close(); err != nil {
			return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		layout.lock = nil
	}

	if err := verifyTerminalSessionAuthority(control, intentDirectory, sessionDirectory, header); err != nil {
		return err
	}
	if err := removeTerminalHeader(sessionDirectory, header); err != nil {
		return err
	}
	intentName := resumestate.ResumeNamespaceName(header.ResumeIntent())
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	return removeEmptySessionShell(
		control.sessions, intentDirectory, sessionDirectory, intentName, sessionName,
	)
}

func inspectTerminalSessionLayout(
	sessionDirectory outputV3Directory,
	header resumestate.Header,
) (*outputTerminalLayout, error) {
	authority := &FilesystemOutputAuthority{}
	return inspectTerminalSessionLayoutWithLock(
		sessionDirectory,
		header,
		func() (outputV3Lock, error) {
			return authority.acquireRuntimeNativeLock(
				func() (outputV3Lock, bool, error) {
					return sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
				},
				filesystemOutputNativeLockContext{
					resumeIntent: header.ResumeIntent(), sessionID: header.SessionID(),
					scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
				},
				intentOutputFault("acquire terminal session lock", errOutputIntentUnsafe),
			)
		},
	)
}

func inspectTerminalSessionLayoutWithLock(
	sessionDirectory outputV3Directory,
	header resumestate.Header,
	acquireSessionLock func() (outputV3Lock, error),
) (*outputTerminalLayout, error) {
	names, err := sessionDirectory.Names(len(outputTerminalRemovalOrder) + 1)
	if err != nil {
		return nil, err
	}
	cut := -1
	for candidateCut := 0; candidateCut < len(outputTerminalRemovalOrder); candidateCut++ {
		expected := slices.Clone(outputTerminalRemovalOrder[candidateCut:])
		actual := slices.Clone(names)
		slices.Sort(expected)
		slices.Sort(actual)
		if slices.Equal(actual, expected) {
			cut = candidateCut
			break
		}
	}
	if cut < 0 {
		return nil, errOutputIntentUnsafe
	}
	encoded, err := readStateRecord(sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return nil, err
	}
	actualHeader, err := resumestate.DecodeHeader(encoded)
	if err != nil || actualHeader != header {
		return nil, errors.Join(errOutputIntentUnsafe, err)
	}
	layout := &outputTerminalLayout{cut: cut}
	for _, binding := range []struct {
		name   string
		target *outputV3Directory
	}{
		{resumestate.StagesDirectoryName, &layout.stages},
		{resumestate.AnchorsDirectoryName, &layout.anchors},
		{resumestate.FilesDirectoryName, &layout.files},
	} {
		if !slices.Contains(names, binding.name) {
			continue
		}
		opened, err := sessionDirectory.OpenDirectory(binding.name, true)
		if err != nil {
			_ = layout.close()
			return nil, err
		}
		*binding.target = opened
	}
	if slices.Contains(names, resumestate.SessionLockName) {
		var lock outputV3Lock
		unexpectedCreated := false
		if acquireSessionLock != nil {
			lock, err = acquireSessionLock()
		} else {
			var created bool
			lock, created, err = sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
			if err == nil && created {
				_ = lock.Close()
				lock = nil
				unexpectedCreated = true
				err = errOutputIntentUnsafe
			}
		}
		if err != nil {
			_ = layout.close()
			if acquireSessionLock != nil {
				return nil, err
			}
			if unexpectedCreated {
				return nil, err
			}
			return nil, classifyLockFailure(transfer.OutputFaultSession, err)
		}
		lockFile := lock.File()
		if lockFile == nil {
			_ = lock.Close()
			_ = layout.close()
			return nil, errOutputIntentUnsafe
		}
		size, sizeErr := lockFile.Size()
		if sizeErr != nil || size != 0 {
			_ = lock.Close()
			_ = layout.close()
			return nil, errors.Join(errOutputIntentUnsafe, sizeErr)
		}
		layout.lock = lock
	}
	return layout, nil
}

func verifyTerminalSessionAuthority(
	control *outputControlNamespace,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	header resumestate.Header,
) error {
	intentName := resumestate.ResumeNamespaceName(header.ResumeIntent())
	if err := verifyPinnedDirectoryEntry(control.sessions, intentName, intentDirectory); err != nil {
		return intentOutputFault("verify terminal intent binding", err)
	}
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	if err := verifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory); err != nil {
		return intentOutputFault("verify terminal session binding", err)
	}
	encoded, err := readStateRecord(sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return intentOutputFault("reread terminal session header", err)
	}
	actual, err := resumestate.DecodeHeader(encoded)
	if err != nil || actual != header {
		return intentOutputFault("verify terminal session header", errors.Join(errOutputIntentUnsafe, err))
	}
	return nil
}

func removeTerminalHeader(sessionDirectory outputV3Directory, expected resumestate.Header) error {
	expectedBytes, err := resumestate.EncodeHeader(expected)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	headerFile, err := sessionDirectory.OpenFile(resumestate.HeaderRecordName, true, false)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	actual, readErr := readStateFile(headerFile, resumestate.MaxSessionHeaderBytes)
	if readErr != nil || !slices.Equal(actual, expectedBytes) {
		_ = headerFile.Close()
		return intentOutputFault("verify terminal header before removal", errors.Join(errOutputIntentUnsafe, readErr))
	}
	if err := sessionDirectory.RemoveFile(resumestate.HeaderRecordName, headerFile); err != nil {
		_ = headerFile.Close()
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := errors.Join(sessionDirectory.Sync(), headerFile.Close()); err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func removeEmptySessionShell(
	sessionsDirectory outputV3Directory,
	intentDirectory outputV3Directory,
	sessionDirectory outputV3Directory,
	intentName string,
	sessionName string,
) error {
	remaining, err := sessionDirectory.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentOutputFault("verify terminal session shell", errors.Join(errOutputIntentUnsafe, err))
	}
	if err := verifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentOutputFault("verify terminal intent shell", err)
	}
	if err := verifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory); err != nil {
		return intentOutputFault("verify terminal session shell binding", err)
	}
	if err := intentDirectory.RemoveDirectory(sessionName, sessionDirectory); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := intentDirectory.Sync(); err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	intentChildren, err := intentDirectory.Names(1)
	if err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if len(intentChildren) != 0 {
		return nil
	}
	if err := verifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentOutputFault("verify empty terminal intent binding", err)
	}
	if err := sessionsDirectory.RemoveDirectory(intentName, intentDirectory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionsDirectory.Sync(); err != nil {
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func closeOutputV3Directory(directory outputV3Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeOutputV3Lock(lock outputV3Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}
