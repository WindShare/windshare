package outputnamespace

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var terminalRemovalOrder = []string{
	resumestate.StagesDirectoryName,
	resumestate.AnchorsDirectoryName,
	resumestate.FilesDirectoryName,
	resumestate.SessionLockName,
	resumestate.HeaderRecordName,
}

type TerminalLayout struct {
	cut     int
	stages  outputcap.Directory
	anchors outputcap.Directory
	files   outputcap.Directory
	lock    outputcap.Lock
}

func (layout *TerminalLayout) Cut() int                     { return layout.cut }
func (layout *TerminalLayout) Stages() outputcap.Directory  { return layout.stages }
func (layout *TerminalLayout) Anchors() outputcap.Directory { return layout.anchors }
func (layout *TerminalLayout) Files() outputcap.Directory   { return layout.files }
func (layout *TerminalLayout) Lock() outputcap.Lock         { return layout.lock }

func (layout *TerminalLayout) Close() error {
	if layout == nil {
		return nil
	}
	return errors.Join(
		closeLock(layout.lock),
		closeDirectory(layout.stages),
		closeDirectory(layout.anchors),
		closeDirectory(layout.files),
	)
}

func RecoverTerminalNamespace(
	control *ControlNamespace,
	intentDirectory outputcap.Directory,
	sessionDirectory outputcap.Directory,
	header resumestate.Header,
	layout *TerminalLayout,
	discard bool,
) error {
	verifyAuthority := func() error {
		return VerifyTerminalAuthority(control, intentDirectory, sessionDirectory, header)
	}
	if err := retireTerminalDirectories(sessionDirectory, layout, discard, verifyAuthority); err != nil {
		return err
	}
	if err := retireTerminalLock(sessionDirectory, layout, verifyAuthority); err != nil {
		return err
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	if err := removeTerminalHeader(sessionDirectory, header); err != nil {
		return err
	}
	intentName := resumestate.ResumeNamespaceName(header.ResumeIntent())
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	return RemoveEmptySessionShell(
		control.Sessions(), intentDirectory, sessionDirectory, intentName, sessionName,
	)
}

func retireTerminalDirectories(
	sessionDirectory outputcap.Directory,
	layout *TerminalLayout,
	discard bool,
	verifyAuthority func() error,
) error {
	for _, child := range []struct {
		name      string
		directory *outputcap.Directory
	}{
		{resumestate.StagesDirectoryName, &layout.stages},
		{resumestate.AnchorsDirectoryName, &layout.anchors},
		{resumestate.FilesDirectoryName, &layout.files},
	} {
		if err := retireTerminalDirectory(
			sessionDirectory, child.name, child.directory, discard, verifyAuthority,
		); err != nil {
			return err
		}
	}
	return nil
}

func retireTerminalDirectory(
	sessionDirectory outputcap.Directory,
	name string,
	directory *outputcap.Directory,
	discard bool,
	verifyAuthority func() error,
) error {
	if *directory == nil {
		return nil
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	if err := prepareTerminalDirectoryForRemoval(*directory, discard, verifyAuthority); err != nil {
		return err
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	if err := sessionDirectory.RemoveDirectory(name, *directory); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionDirectory.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := (*directory).Close(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	*directory = nil
	return nil
}

func prepareTerminalDirectoryForRemoval(
	directory outputcap.Directory,
	discard bool,
	verifyAuthority func() error,
) error {
	if discard {
		if err := RemovePrivateDirectoryContents(directory, 0, verifyAuthority); err != nil {
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		return nil
	}
	remaining, err := directory.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentFault("verify completing directory is empty", errors.Join(outputfault.ErrIntentUnsafe, err))
	}
	return nil
}

func retireTerminalLock(
	sessionDirectory outputcap.Directory,
	layout *TerminalLayout,
	verifyAuthority func() error,
) error {
	if layout.lock == nil {
		return nil
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	lockFile := layout.lock.File()
	if lockFile == nil {
		return intentFault("remove terminal session lock", outputfault.ErrIntentUnsafe)
	}
	if err := sessionDirectory.RemoveFile(resumestate.SessionLockName, lockFile); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionDirectory.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := layout.lock.Close(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	layout.lock = nil
	return nil
}

func InspectTerminalLayout(
	sessionDirectory outputcap.Directory,
	header resumestate.Header,
	acquireSessionLock func() (outputcap.Lock, error),
) (*TerminalLayout, error) {
	names, err := sessionDirectory.Names(len(terminalRemovalOrder) + 1)
	if err != nil {
		return nil, err
	}

	cut, valid := terminalPublicationCut(names)
	if !valid {
		return nil, outputfault.ErrIntentUnsafe
	}
	if err := verifyTerminalHeader(sessionDirectory, header); err != nil {
		return nil, err
	}
	layout := &TerminalLayout{cut: cut}
	if err := openTerminalDirectories(sessionDirectory, names, layout); err != nil {
		_ = layout.Close()
		return nil, err
	}
	if slices.Contains(names, resumestate.SessionLockName) {
		layout.lock, err = acquireTerminalLayoutLock(sessionDirectory, acquireSessionLock)
		if err != nil {
			_ = layout.Close()
			return nil, err
		}
	}
	return layout, nil
}

func terminalPublicationCut(names []string) (int, bool) {
	for candidateCut := range len(terminalRemovalOrder) {
		expected := slices.Clone(terminalRemovalOrder[candidateCut:])
		actual := slices.Clone(names)
		slices.Sort(expected)
		slices.Sort(actual)
		if slices.Equal(actual, expected) {
			return candidateCut, true
		}
	}
	return 0, false
}

func verifyTerminalHeader(sessionDirectory outputcap.Directory, header resumestate.Header) error {
	encoded, err := ReadRecord(sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return err
	}
	actualHeader, err := resumestate.DecodeHeader(encoded)
	if err != nil || actualHeader != header {
		return errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	return nil
}

func openTerminalDirectories(
	sessionDirectory outputcap.Directory,
	names []string,
	layout *TerminalLayout,
) error {
	for _, binding := range []struct {
		name   string
		target *outputcap.Directory
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
			return err
		}
		*binding.target = opened
	}
	return nil
}

func acquireTerminalLayoutLock(
	sessionDirectory outputcap.Directory,
	acquireSessionLock func() (outputcap.Lock, error),
) (outputcap.Lock, error) {
	if acquireSessionLock != nil {
		lock, err := acquireSessionLock()
		if err != nil {
			return nil, err
		}
		return validateTerminalLayoutLock(lock)
	}
	lock, created, err := sessionDirectory.AcquireLock(resumestate.SessionLockName, true)
	if err != nil {
		return nil, classifyTerminalLockFailure(transfer.OutputFaultSession, err)
	}
	if created {
		_ = closeLock(lock)
		return nil, outputfault.ErrIntentUnsafe
	}
	return validateTerminalLayoutLock(lock)
}

func validateTerminalLayoutLock(lock outputcap.Lock) (outputcap.Lock, error) {
	if lock == nil || lock.File() == nil {
		_ = closeLock(lock)
		return nil, outputfault.ErrIntentUnsafe
	}
	size, err := lock.File().Size()
	if err != nil || size != 0 {
		_ = lock.Close()
		return nil, errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	return lock, nil
}

func VerifyTerminalAuthority(
	control *ControlNamespace,
	intentDirectory outputcap.Directory,
	sessionDirectory outputcap.Directory,
	header resumestate.Header,
) error {
	intentName := resumestate.ResumeNamespaceName(header.ResumeIntent())
	if err := VerifyPinnedDirectoryEntry(control.Sessions(), intentName, intentDirectory); err != nil {
		return intentFault("verify terminal intent binding", err)
	}
	sessionName := resumestate.SessionDirectoryName(header.SessionID())
	if err := VerifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory); err != nil {
		return intentFault("verify terminal session binding", err)
	}
	encoded, err := ReadRecord(sessionDirectory, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return intentFault("reread terminal session header", err)
	}
	actual, err := resumestate.DecodeHeader(encoded)
	if err != nil || actual != header {
		return intentFault("verify terminal session header", errors.Join(outputfault.ErrIntentUnsafe, err))
	}
	return nil
}

func removeTerminalHeader(sessionDirectory outputcap.Directory, expected resumestate.Header) error {
	expectedBytes, err := resumestate.EncodeHeader(expected)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	headerFile, err := sessionDirectory.OpenFile(resumestate.HeaderRecordName, true, false)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	actual, readErr := ReadFile(headerFile, resumestate.MaxSessionHeaderBytes)
	if readErr != nil || !slices.Equal(actual, expectedBytes) {
		_ = headerFile.Close()
		return intentFault("verify terminal header before removal", errors.Join(outputfault.ErrIntentUnsafe, readErr))
	}
	if err := sessionDirectory.RemoveFile(resumestate.HeaderRecordName, headerFile); err != nil {
		_ = headerFile.Close()
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := errors.Join(sessionDirectory.Sync(), headerFile.Close()); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func RemoveEmptySessionShell(
	sessionsDirectory outputcap.Directory,
	intentDirectory outputcap.Directory,
	sessionDirectory outputcap.Directory,
	intentName string,
	sessionName string,
) error {
	remaining, err := sessionDirectory.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentFault("verify terminal session shell", errors.Join(outputfault.ErrIntentUnsafe, err))
	}
	if err := VerifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentFault("verify terminal intent shell", err)
	}
	if err := VerifyPinnedDirectoryEntry(intentDirectory, sessionName, sessionDirectory); err != nil {
		return intentFault("verify terminal session shell binding", err)
	}
	if err := intentDirectory.RemoveDirectory(sessionName, sessionDirectory); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := intentDirectory.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	intentChildren, err := intentDirectory.Names(1)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if len(intentChildren) != 0 {
		return nil
	}
	if err := VerifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentFault("verify empty terminal intent binding", err)
	}
	if err := sessionsDirectory.RemoveDirectory(intentName, intentDirectory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionsDirectory.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func closeLock(lock outputcap.Lock) error {
	if lock == nil {
		return nil
	}
	return lock.Close()
}

func classifyTerminalLockFailure(scope transfer.OutputFaultScope, err error) error {
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return outputfault.New(scope, transfer.OutputFaultOwnership, errors.Join(outputfault.ErrSessionActive, err))
	}
	return outputfault.New(scope, transfer.OutputFaultStateIO, err)
}

func intentFault(operation string, cause error) error {
	return transfer.NewOutputSessionError(
		outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			errors.Join(outputfault.ErrIntentUnsafe, fmt.Errorf("%s: %w", operation, cause))),
		true,
	)
}

func RemovePrivateDirectoryContents(
	directory outputcap.Directory,
	depth int,
	verifyAuthority func() error,
) error {
	if depth > resumestate.MaxStateNestingDepth {
		return outputfault.ErrInspectionLimit
	}
	names, err := directory.Names(FileShardInspectionLimit)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		if err := removePrivateEntry(directory, name, depth, verifyAuthority); err != nil {
			return err
		}
	}
	return nil
}

func removePrivateEntry(
	parent outputcap.Directory,
	name string,
	depth int,
	verifyAuthority func() error,
) error {
	entry, err := parent.OpenEntry(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer entry.Close()
	switch entry.Kind() {
	case outputcap.EntryDirectory:
		child, err := parent.OpenPinnedDirectory(entry, false)
		if err != nil {
			return err
		}
		verifyChildAuthority := func() error {
			if err := verifyAuthority(); err != nil {
				return err
			}
			matches, err := parent.EntryMatches(name, entry)
			if err != nil || !matches {
				return errors.Join(outputcap.ErrUnsafeNamespace, err)
			}
			return nil
		}
		if err := verifyChildAuthority(); err != nil {
			_ = child.Close()
			return err
		}
		if err := RemovePrivateDirectoryContents(child, depth+1, verifyChildAuthority); err != nil {
			_ = child.Close()
			return err
		}
		if err := verifyChildAuthority(); err != nil {
			_ = child.Close()
			return err
		}
		removeErr := parent.RemoveEntry(name, entry)
		return errors.Join(removeErr, child.Close())
	case outputcap.EntryAbsent:
		return nil
	case outputcap.EntryRegularFile, outputcap.EntryOther:
		if err := verifyAuthority(); err != nil {
			return err
		}
		return parent.RemoveEntry(name, entry)
	default:
		return outputcap.ErrUnsafeNamespace
	}
}
