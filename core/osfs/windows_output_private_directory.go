//go:build windows

package osfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsV3PrivateDirectoryCreateCut uint8

const (
	windowsV3PrivateDirectoryCutCreated windowsV3PrivateDirectoryCreateCut = iota + 1
	windowsV3PrivateDirectoryCutObjectID
	windowsV3PrivateDirectoryCutACLHidden
	windowsV3PrivateDirectoryCutSynced
	windowsV3PrivateDirectoryCutCommitted
	windowsV3PrivateDirectoryCutClosed
)

func (cut windowsV3PrivateDirectoryCreateCut) String() string {
	switch cut {
	case windowsV3PrivateDirectoryCutCreated:
		return "create"
	case windowsV3PrivateDirectoryCutObjectID:
		return "object-id"
	case windowsV3PrivateDirectoryCutACLHidden:
		return "acl-hidden"
	case windowsV3PrivateDirectoryCutSynced:
		return "sync-reopen"
	case windowsV3PrivateDirectoryCutCommitted:
		return "commit"
	case windowsV3PrivateDirectoryCutClosed:
		return "close"
	default:
		return fmt.Sprintf("unknown(%d)", cut)
	}
}

// The observer is deliberately attached to the fixed directory authority so
// fault tests can stop at native syscall boundaries without introducing a
// process-global hook that would race unrelated output sessions.
type windowsV3PrivateDirectoryCreateObserver interface {
	ObservePrivateDirectoryCreate(parent, target string, cut windowsV3PrivateDirectoryCreateCut) error
}

type windowsV3PrivateDirectoryCreateObserverFunc func(
	parent, target string,
	cut windowsV3PrivateDirectoryCreateCut,
) error

func (observe windowsV3PrivateDirectoryCreateObserverFunc) ObservePrivateDirectoryCreate(
	parent, target string,
	cut windowsV3PrivateDirectoryCreateCut,
) error {
	return observe(parent, target, cut)
}

func (directory *windowsV3Directory) observePrivateDirectoryCreate(
	target string,
	cut windowsV3PrivateDirectoryCreateCut,
) error {
	if directory.createObserver == nil {
		return nil
	}
	return directory.createObserver.ObservePrivateDirectoryCreate(directory.path, target, cut)
}

func (directory *windowsV3Directory) createPrivateDirectory(
	relative string,
) (_ *windowsV3Directory, resultErr error) {
	const operation = "create crash-safe private output directory"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	native, err := windowsV3RelativePath(relative, true)
	if err != nil {
		return nil, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	descriptor, err := directory.policy.descriptor(true)
	if err != nil {
		return nil, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	handle, status, err := windowsV3OpenNativeWithOptions(
		directory.handle(), native, windowsV3DirectoryAccess(), windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE,
		windows.FILE_ATTRIBUTE_HIDDEN, descriptor,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, windowsV3NativeNoReplaceFailure(operation, relative, err)
	}
	createdStatus, statusErr := windowsV3CreationStatus(windows.FILE_CREATE, status)
	if statusErr != nil || !createdStatus {
		return nil, errors.Join(
			windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, statusErr),
			windows.CloseHandle(handle), directory.Sync(),
		)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		return nil, errors.Join(
			windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, errors.New("wrap directory handle")),
			windows.CloseHandle(handle), directory.Sync(),
		)
	}
	created := &windowsV3Directory{
		file: file, path: filepath.Join(directory.path, relative), volume: directory.volume,
		objectIDs: directory.objectIDs, inspector: directory.inspector, policy: directory.policy,
		objectIDState: newWindowsV3PersistentObjectIDState(), ancestryAuthority: directory.ancestryAuthority,
		enumerate: &sync.Mutex{}, createObserver: directory.createObserver, private: true,
	}
	defer func() {
		if created.file != nil {
			resultErr = errors.Join(resultErr, created.Close())
			// Before the commit syscall close removes the entry; afterward it makes
			// the prepared entry independently openable. Syncing after either cut
			// leaves the fixed parent in the exact state recovery will observe.
			resultErr = errors.Join(resultErr, directory.Sync())
		}
	}()

	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutCreated); err != nil {
		return nil, err
	}
	// Validate the restrictive descriptor before CreateOrGet because Object IDs
	// are intentionally never rolled back after a failed admission.
	if err := created.verify(true); err != nil {
		return nil, err
	}
	identity, err := created.preparePrivatePersistentObjectID()
	if err != nil {
		return nil, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutObjectID); err != nil {
		return nil, err
	}
	if err := created.verify(true); err != nil {
		return nil, err
	}
	if err := windowsV3VerifyOpenedLeafAuthority(created.handle(), native, true); err != nil {
		return nil, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutACLHidden); err != nil {
		return nil, err
	}
	if err := errors.Join(created.Sync(), directory.Sync()); err != nil {
		return nil, err
	}
	duplicate, err := created.Duplicate()
	if err != nil {
		return nil, err
	}
	duplicateIdentity, identityPrepared, identityErr := duplicate.cachedPersistentObjectID()
	if !identityPrepared {
		identityErr = errors.Join(identityErr, errors.New("duplicated private directory lost its prepared Object ID"))
	}
	verifyErr := duplicate.verify(true)
	closeDuplicateErr := duplicate.Close()
	if identityErr != nil || verifyErr != nil || closeDuplicateErr != nil || duplicateIdentity != identity {
		return nil, errors.Join(
			windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe,
				errors.New("prepared private directory did not preserve its persistent identity and security")),
			identityErr, verifyErr, closeDuplicateErr,
		)
	}
	preparedFacts, err := created.inspector.Inspect(created.handle())
	if err != nil {
		return nil, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutSynced); err != nil {
		return nil, err
	}
	if err := windowsV3CommitDeleteOnClose(created.handle()); err != nil {
		return nil, windowsV3NativeOperationFailure(operation, relative, err)
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutCommitted); err != nil {
		return nil, err
	}
	if err := directory.Sync(); err != nil {
		return nil, err
	}
	if err := created.Close(); err != nil {
		return nil, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutClosed); err != nil {
		return nil, err
	}
	reopened, err := directory.OpenPrivateDirectory(relative)
	if err != nil {
		return nil, windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	reopenedFacts, factsErr := reopened.inspector.Inspect(reopened.handle())
	reopenedIdentity, reopenedPrepared, reopenedIdentityErr := reopened.cachedPersistentObjectID()
	if factsErr != nil || !preparedFacts.object.same(reopenedFacts.object) ||
		!reopenedPrepared || reopenedIdentity != identity || reopenedIdentityErr != nil {
		return nil, errors.Join(
			windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe,
				errors.New("committed private directory reopened as a different incarnation")),
			factsErr, reopenedIdentityErr, reopened.Close(),
		)
	}
	return reopened, nil
}

func windowsV3CommitDeleteOnClose(handle windows.Handle) error {
	// FileDispositionInfoEx with ON_CLOSE and without DELETE is the NT contract
	// for clearing a FILE_DELETE_ON_CLOSE link. This is the sole transition from
	// rollback-on-process-death to a persistent namespace entry.
	information := windowsV3DispositionInformation{Flags: windows.FILE_DISPOSITION_ON_CLOSE}
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	)
}
