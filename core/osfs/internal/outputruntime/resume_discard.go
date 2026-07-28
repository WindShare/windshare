package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (authority *Authority) DiscardResumeState(
	ctx context.Context,
	reference ResumeStateRef,
) (resultSettlement DiscardSettlement, resultErr error) {
	if authority == nil || authority.platformFactory == nil || reference.inventory == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	if err := ctx.Err(); err != nil {
		return DiscardSettlement{}, err
	}
	resolved, err := reference.inventory.consume(reference)
	if err != nil {
		return DiscardSettlement{}, err
	}
	reference = resolved
	defer func() {
		resultErr = errors.Join(resultErr, reference.releaseAuthority())
		if resultErr != nil {
			resultSettlement = DiscardSettlement{}
		}
	}()
	return authority.discardConsumedResumeState(reference)
}

func (authority *Authority) discardConsumedResumeState(
	reference ResumeStateRef,
) (resultSettlement DiscardSettlement, resultErr error) {
	if !reference.validAuthority() {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	platform, err := authority.platformFactory(reference.rootPath, false)
	if err != nil {
		return DiscardSettlement{}, outputnamespace.RootFault("certify discard root", err)
	}
	defer platform.Close()
	operation, acquireErr, acquireCloseErr := acquireResumePublicRootOperation(platform)
	if acquireErr != nil || acquireCloseErr != nil {
		return DiscardSettlement{}, errors.Join(
			resumeRootRevalidationFault("acquire resume discard root guard", acquireErr),
			resumeRootGuardCleanupFault("close unusable resume discard root guard", acquireCloseErr),
		)
	}
	defer func() {
		finishResumePublicRootOperation(operation, "resume discard", &resultErr)
		if resultErr != nil {
			resultSettlement = DiscardSettlement{}
		}
	}()
	guardedRoot := operation.root
	if reference.kind == ResumeStateLegacyUntrusted {
		return discardLegacyState(guardedRoot, reference)
	}
	return authority.discardGuardedResumeState(guardedRoot, platform, reference)
}

func (authority *Authority) discardGuardedResumeState(
	guardedRoot outputcap.Directory,
	platform outputcap.Platform,
	reference ResumeStateRef,
) (DiscardSettlement, error) {
	listedEntry := reference.sessionPin.take()
	if listedEntry == nil {
		return DiscardSettlement{}, transfer.ErrInvalidOutputBinding
	}
	defer listedEntry.Close()
	if reference.sessionName == "" || reference.namespaceName == "" {
		return DiscardSettlement{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputBinding,
		)
	}
	control, err := authority.namespaceController().OpenInstalledControl(guardedRoot, platform)
	if errors.Is(err, fs.ErrNotExist) {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if err != nil {
		return DiscardSettlement{}, err
	}
	defer control.Close()
	if control.Control().OutputRoot() != reference.root {
		return DiscardSettlement{}, outputnamespace.RootFault("bind discard root", outputfault.ErrRootUnsafe)
	}
	coordinator, err := authority.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return control.Directory().AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: reference.intent, sessionID: reference.session,
			scope: FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		outputnamespace.RootFault("acquire discard coordinator", outputfault.ErrRootUnsafe),
	)
	if err != nil {
		return DiscardSettlement{}, err
	}
	defer coordinator.Close()
	intentDir, err := control.Sessions().OpenDirectory(reference.namespaceName, true)
	if errors.Is(err, fs.ErrNotExist) {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("open discard intent", err)
	}
	defer intentDir.Close()
	entryKind, err := intentDir.ObserveEntry(reference.sessionName)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("observe discard session entry", err)
	}
	if entryKind == outputcap.EntryAbsent {
		return DiscardSettlement{Kind: DiscardAlreadyAbsent}, nil
	}
	if entryKind != reference.sessionKind || listedEntry.Kind() != reference.sessionKind {
		return DiscardSettlement{}, intentOutputFault(
			"bind listed discard session entry", outputfault.ErrIntentUnsafe,
		)
	}
	return authority.discardListedSession(control, intentDir, reference, listedEntry, entryKind)
}

func (authority *Authority) discardListedSession(
	control *outputnamespace.ControlNamespace,
	intentDir outputcap.Directory,
	reference ResumeStateRef,
	listedEntry outputcap.CurrentEntryReference,
	entryKind outputcap.EntryKind,
) (DiscardSettlement, error) {
	verifyDiscardEntry := func() error {
		return verifyListedDiscardEntry(control, intentDir, reference, listedEntry)
	}
	if err := verifyDiscardEntry(); err != nil {
		return DiscardSettlement{}, err
	}
	if entryKind != outputcap.EntryDirectory {
		return discardOpaqueSessionEntry(
			control.Sessions(), intentDir, reference, listedEntry, verifyDiscardEntry,
		)
	}
	privateSession := reference.kind != ResumeStateOpaqueUnsafe
	sessionDir, err := intentDir.OpenPinnedDirectory(listedEntry, privateSession)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("open discard session directory", err)
	}
	defer sessionDir.Close()
	if err := outputnamespace.VerifyPinnedDirectoryEntry(control.Sessions(), reference.namespaceName, intentDir); err != nil {
		return DiscardSettlement{}, intentOutputFault("pin discard intent", err)
	}
	return authority.discardPrivateSession(
		control, intentDir, sessionDir, reference, listedEntry, verifyDiscardEntry,
	)
}

func verifyListedDiscardEntry(
	control *outputnamespace.ControlNamespace,
	intentDir outputcap.Directory,
	reference ResumeStateRef,
	listedEntry outputcap.CurrentEntryReference,
) error {
	if err := outputnamespace.VerifyPinnedDirectoryEntry(
		control.Sessions(), reference.namespaceName, intentDir,
	); err != nil {
		return intentOutputFault("verify discard intent", err)
	}
	matches, err := intentDir.EntryMatches(reference.sessionName, listedEntry)
	if err != nil || !matches {
		return intentOutputFault(
			"verify discard entry identity", errors.Join(outputfault.ErrIntentUnsafe, err),
		)
	}
	return nil
}

func (authority *Authority) discardPrivateSession(
	control *outputnamespace.ControlNamespace,
	intentDir outputcap.Directory,
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
	listedEntry outputcap.CurrentEntryReference,
	verifyDiscardAuthority func() error,
) (DiscardSettlement, error) {
	lock, err := authority.acquireDiscardSessionLock(sessionDir, reference)
	if err != nil {
		return DiscardSettlement{}, err
	}
	lockOwned := true
	defer func() {
		if lockOwned && lock != nil {
			_ = lock.Close()
		}
	}()
	if err := verifyDiscardAuthority(); err != nil {
		return DiscardSettlement{}, err
	}
	preview, err := previewPrivateTree(sessionDir)
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("preview discard session", err)
	}
	store := authority.stateStore(reference.intent, reference.session)
	discardState, validHeader, corruptHeader, err := installDiscardingHeader(
		store, control.Control(), sessionDir, reference, lock != nil, verifyDiscardAuthority,
	)
	if err != nil {
		return DiscardSettlement{}, err
	}
	if err := authority.authorizeLocklessDiscard(
		sessionDir, reference, lock, discardState, validHeader, corruptHeader,
	); err != nil {
		return DiscardSettlement{}, err
	}
	if err := recoverCorruptDiscard(
		control, intentDir, sessionDir, reference, listedEntry, lock, verifyDiscardAuthority,
	); err != nil {
		return DiscardSettlement{}, err
	}
	lock, lockOwned = nil, false
	return DiscardSettlement{Kind: Discarded, RemovedBytes: preview.allocatedBytes}, nil
}

func (authority *Authority) acquireDiscardSessionLock(
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
) (outputcap.Lock, error) {
	lockKind, err := outputnamespace.ObserveExactEntry(sessionDir, resumestate.SessionLockName)
	if err != nil {
		return nil, intentOutputFault("observe discard session lock", err)
	}
	switch lockKind {
	case outputcap.EntryRegularFile:
		return authority.acquireRuntimeNativeLock(
			func() (outputcap.Lock, bool, error) {
				return sessionDir.AcquireLock(resumestate.SessionLockName, true)
			},
			filesystemOutputNativeLockContext{
				resumeIntent: reference.intent, sessionID: reference.session,
				scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
			},
			intentOutputFault("revalidate discard session lock", outputfault.ErrIntentUnsafe),
		)
	case outputcap.EntryAbsent:
		// A valid terminal suffix is verified before any child is removed.
		return nil, nil
	default:
		return nil, intentOutputFault("classify discard session lock", outputfault.ErrIntentUnsafe)
	}
}

func (authority *Authority) authorizeLocklessDiscard(
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
	lock outputcap.Lock,
	discardState resumestate.SessionNamespaceAuthority,
	validHeader bool,
	corruptHeader bool,
) error {
	if lock != nil {
		return nil
	}
	if validHeader {
		return authority.validateLocklessDiscardSuffix(sessionDir, reference, discardState)
	}
	if corruptHeader {
		return nil
	}
	remaining, err := sessionDir.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentOutputFault(
			"authorize lockless explicit discard",
			errors.Join(outputfault.ErrIntentUnsafe, err),
		)
	}
	return nil
}

func (authority *Authority) validateLocklessDiscardSuffix(
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
	discardState resumestate.SessionNamespaceAuthority,
) error {
	layout, err := outputnamespace.InspectTerminalLayout(
		sessionDir,
		discardState.Header(),
		func() (outputcap.Lock, error) {
			return authority.acquireRuntimeNativeLock(
				func() (outputcap.Lock, bool, error) {
					return sessionDir.AcquireLock(resumestate.SessionLockName, true)
				},
				filesystemOutputNativeLockContext{
					resumeIntent: reference.intent, sessionID: reference.session,
					scope: FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
				},
				intentOutputFault("acquire terminal discard session lock", outputfault.ErrIntentUnsafe),
			)
		},
	)
	if layout != nil {
		err = errors.Join(err, layout.Close())
	}
	if err != nil {
		return intentOutputFault("validate lockless discard suffix", err)
	}
	return nil
}

func discardOpaqueSessionEntry(
	sessionsDirectory outputcap.Directory,
	intentDirectory outputcap.Directory,
	reference ResumeStateRef,
	entry outputcap.CurrentEntryReference,
	verifyAuthority func() error,
) (DiscardSettlement, error) {
	if entry == nil || entry.Kind() == outputcap.EntryAbsent || entry.Kind() == outputcap.EntryDirectory {
		return DiscardSettlement{}, intentOutputFault("classify opaque session entry", outputfault.ErrIntentUnsafe)
	}
	removedBytes, err := entry.AllocatedSize()
	if err != nil {
		return DiscardSettlement{}, intentOutputFault("preview opaque session entry", err)
	}
	if err := verifyAuthority(); err != nil {
		return DiscardSettlement{}, err
	}
	if err := intentDirectory.RemoveEntry(reference.sessionName, entry); err != nil {
		return DiscardSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := removeEmptyIntentShell(
		sessionsDirectory, intentDirectory, reference.namespaceName,
	); err != nil {
		return DiscardSettlement{}, err
	}
	return DiscardSettlement{Kind: Discarded, RemovedBytes: removedBytes}, nil
}

func removeEmptyIntentShell(
	sessionsDirectory outputcap.Directory,
	intentDirectory outputcap.Directory,
	intentName string,
) error {
	remaining, err := intentDirectory.Names(1)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if len(remaining) != 0 {
		return nil
	}
	if err := outputnamespace.VerifyPinnedDirectoryEntry(sessionsDirectory, intentName, intentDirectory); err != nil {
		return intentOutputFault("verify empty explicit-discard intent", err)
	}
	if err := sessionsDirectory.RemoveDirectory(intentName, intentDirectory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionsDirectory.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func recoverCorruptDiscard(
	control *outputnamespace.ControlNamespace,
	intentDir outputcap.Directory,
	sessionDir outputcap.Directory,
	reference ResumeStateRef,
	sessionEntry outputcap.CurrentEntryReference,
	lock outputcap.Lock,
	verifyParents func() error,
) error {
	if err := verifyParents(); err != nil {
		return err
	}
	for _, name := range []string{
		resumestate.StagesDirectoryName, resumestate.AnchorsDirectoryName, resumestate.FilesDirectoryName,
	} {
		if err := removePrivateEntry(sessionDir, name, 0, verifyParents); err != nil {
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}

	names, err := sessionDir.Names(resumeDirectoryChildLimit)
	if err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	slices.Sort(names)
	for _, name := range names {
		if name == resumestate.HeaderRecordName || name == resumestate.SessionLockName {
			continue
		}
		if err := verifyParents(); err != nil {
			return err
		}
		if err := removePrivateEntry(sessionDir, name, 0, verifyParents); err != nil {
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}

	if err := retireCorruptDiscardLock(sessionDir, lock, verifyParents); err != nil {
		return err
	}
	// The envelope is removed last, after every data-bearing child and the lock.
	// Keeping it until this cut makes explicit-discard restart diagnosis stable.
	if err := removePrivateEntry(
		sessionDir, resumestate.HeaderRecordName, 0, verifyParents,
	); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return removeEmptyPinnedSessionShell(
		control.Sessions(), intentDir, sessionDir, reference.namespaceName, reference.sessionName,
		sessionEntry, verifyParents,
	)
}

func retireCorruptDiscardLock(
	sessionDir outputcap.Directory,
	lock outputcap.Lock,
	verifyParents func() error,
) error {
	if lock == nil {
		if err := removePrivateEntry(
			sessionDir, resumestate.SessionLockName, 0, verifyParents,
		); err != nil {
			return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		return nil
	}
	if err := verifyParents(); err != nil {
		return err
	}
	lockFile := lock.File()
	if lockFile == nil {
		return intentOutputFault("remove corrupt-discard lock", outputfault.ErrIntentUnsafe)
	}
	if err := sessionDir.RemoveFile(resumestate.SessionLockName, lockFile); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := sessionDir.Sync(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	if err := lock.Close(); err != nil {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return nil
}

func removeEmptyPinnedSessionShell(
	sessionsDirectory outputcap.Directory,
	intentDirectory outputcap.Directory,
	sessionDirectory outputcap.Directory,
	intentName string,
	sessionName string,
	sessionEntry outputcap.CurrentEntryReference,
	verifyAuthority func() error,
) error {
	remaining, err := sessionDirectory.Names(1)
	if err != nil || len(remaining) != 0 {
		return intentOutputFault("verify explicit-discard session shell", errors.Join(outputfault.ErrIntentUnsafe, err))
	}
	if err := verifyAuthority(); err != nil {
		return err
	}
	if err := intentDirectory.RemoveEntry(sessionName, sessionEntry); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return removeEmptyIntentShell(sessionsDirectory, intentDirectory, intentName)
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
		if err := outputnamespace.RemovePrivateDirectoryContents(child, depth+1, verifyChildAuthority); err != nil {
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
