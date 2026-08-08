//go:build windows

package outputwindows

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

	identity, preparedFacts, err := directory.prepareCreatedPrivateDirectory(created, relative, native)
	if err != nil {
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

func (directory *windowsV3Directory) prepareCreatedPrivateDirectory(
	created *windowsV3Directory,
	relative string,
	native string,
) (windowsV3PersistentObjectID, windowsV3HandleFacts, error) {
	const operation = "create crash-safe private output directory"
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutCreated); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	// Validate the restrictive descriptor before CreateOrGet because Object IDs
	// are intentionally never rolled back after a failed admission.
	if err := created.verify(true); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	identity, err := created.preparePrivatePersistentObjectID()
	if err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutObjectID); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := created.verify(true); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := windowsV3VerifyOpenedLeafAuthority(created.handle(), native, true); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutACLHidden); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := errors.Join(created.Sync(), directory.Sync()); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	if err := windowsV3VerifyPreparedPrivateDirectoryDuplicate(created, identity, operation, relative); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	preparedFacts, err := created.inspector.Inspect(created.handle())
	if err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{},
			windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe, err)
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsV3PrivateDirectoryCutSynced); err != nil {
		return windowsV3PersistentObjectID{}, windowsV3HandleFacts{}, err
	}
	return identity, preparedFacts, nil
}

func windowsV3VerifyPreparedPrivateDirectoryDuplicate(
	created *windowsV3Directory,
	identity windowsV3PersistentObjectID,
	operation string,
	relative string,
) error {
	duplicate, err := created.Duplicate()
	if err != nil {
		return err
	}
	duplicateIdentity, identityPrepared, identityErr := duplicate.cachedPersistentObjectID()
	if !identityPrepared {
		identityErr = errors.Join(identityErr, errors.New("duplicated private directory lost its prepared Object ID"))
	}
	verifyErr := duplicate.verify(true)
	closeDuplicateErr := duplicate.Close()
	if identityErr == nil && verifyErr == nil && closeDuplicateErr == nil && duplicateIdentity == identity {
		return nil
	}
	return errors.Join(
		windowsV3Failure(operation, relative, errWindowsV3OutputUnsafe,
			errors.New("prepared private directory did not preserve its persistent identity and security")),
		identityErr, verifyErr, closeDuplicateErr,
	)
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

type windowsV3PrivatePolicy struct {
	userSID             *windows.SID
	systemSID           *windows.SID
	administratorsSID   *windows.SID
	trustedInstallerSID *windows.SID
}

const windowsV3TrustedInstallerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func newWindowsV3PrivatePolicy() (*windowsV3PrivatePolicy, error) {
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// Copy the token-owned SID so the policy cannot outlive its backing token
	// information buffer.
	userSID, err := windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return nil, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	administratorsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	trustedInstallerSID, err := windows.StringToSid(windowsV3TrustedInstallerSID)
	if err != nil {
		return nil, err
	}
	return &windowsV3PrivatePolicy{
		userSID: userSID, systemSID: systemSID,
		administratorsSID: administratorsSID, trustedInstallerSID: trustedInstallerSID,
	}, nil
}

func (policy *windowsV3PrivatePolicy) descriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	if policy == nil || policy.userSID == nil || policy.systemSID == nil {
		return nil, errors.New("windows private ACL policy is unavailable")
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	entries := fmt.Sprintf("(A;%s;GA;;;%s)", inheritance, policy.userSID.String())
	if !policy.userSID.Equals(policy.systemSID) {
		entries += fmt.Sprintf("(A;%s;GA;;;%s)", inheritance, policy.systemSID.String())
	}
	// P protects the DACL from parent inheritance. Only the effective user and
	// LocalSystem retain access; inherited broad desktop ACLs never enter the
	// recovery namespace.
	return windows.SecurityDescriptorFromString("O:" + policy.userSID.String() + "D:P" + entries)
}

func (policy *windowsV3PrivatePolicy) verify(handle windows.Handle, directory bool) error {
	expectedFlags := uint8(0)
	if directory {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return policy.verifyObjectPolicy(
		handle, windows.SE_FILE_OBJECT, windowsV3FileAllAccess, expectedFlags,
	)
}

func (policy *windowsV3PrivatePolicy) verifyKernelMutex(handle windows.Handle) error {
	return policy.verifyObjectPolicy(handle, windows.SE_KERNEL_OBJECT, windows.MUTEX_ALL_ACCESS, 0)
}

func (policy *windowsV3PrivatePolicy) verifyObjectPolicy(
	handle windows.Handle,
	objectType windows.SE_OBJECT_TYPE,
	expectedMask windows.ACCESS_MASK,
	expectedFlags uint8,
) error {
	if policy == nil || policy.userSID == nil || policy.systemSID == nil {
		return errors.New("windows private ACL policy is unavailable")
	}
	descriptor, err := windows.GetSecurityInfo(
		handle, objectType, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("private DACL is not protected from inheritance")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || !owner.Equals(policy.userSID) {
		return errors.Join(errors.New("private object owner differs from the effective user"), err)
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted {
		return errors.Join(errors.New("private object DACL is absent or defaulted"), err)
	}

	expectedCount := uint16(2)
	if policy.userSID.Equals(policy.systemSID) {
		expectedCount = 1
	}
	if dacl.AceCount != expectedCount {
		return fmt.Errorf("private DACL contains %d entries; expected %d", dacl.AceCount, expectedCount)
	}
	userFound, systemFound := false, policy.userSID.Equals(policy.systemSID)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags != expectedFlags || ace.Mask != expectedMask {
			if ace == nil {
				return errors.New("private DACL contains a nil access entry")
			}
			return fmt.Errorf("private DACL access entry type=%d flags=%#x mask=%#x; expected type=%d flags=%#x mask=%#x",
				ace.Header.AceType, ace.Header.AceFlags, ace.Mask,
				windows.ACCESS_ALLOWED_ACE_TYPE, expectedFlags, expectedMask)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case sid.Equals(policy.userSID) && !userFound:
			userFound = true
		case sid.Equals(policy.systemSID) && !systemFound:
			systemFound = true
		default:
			return errors.New("private DACL grants an unexpected or duplicate principal")
		}
	}
	if !userFound || !systemFound {
		return errors.New("private DACL omits a required principal")
	}
	return nil
}
