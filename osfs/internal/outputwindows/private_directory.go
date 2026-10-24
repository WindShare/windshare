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

type windowsPrivateDirectoryCreateCut uint8

const (
	windowsPrivateDirectoryCutCreated windowsPrivateDirectoryCreateCut = iota + 1
	windowsPrivateDirectoryCutACLHidden
	windowsPrivateDirectoryCutSynced
	windowsPrivateDirectoryCutCommitted
	windowsPrivateDirectoryCutClosed
)

func (cut windowsPrivateDirectoryCreateCut) String() string {
	switch cut {
	case windowsPrivateDirectoryCutCreated:
		return "create"
	case windowsPrivateDirectoryCutACLHidden:
		return "acl-hidden"
	case windowsPrivateDirectoryCutSynced:
		return "sync-reopen"
	case windowsPrivateDirectoryCutCommitted:
		return "commit"
	case windowsPrivateDirectoryCutClosed:
		return "close"
	default:
		return fmt.Sprintf("unknown(%d)", cut)
	}
}

// The observer is deliberately attached to the fixed directory authority so
// fault tests can stop at native syscall boundaries without introducing a
// process-global hook that would race unrelated output sessions.
type windowsPrivateDirectoryCreateObserver interface {
	ObservePrivateDirectoryCreate(parent, target string, cut windowsPrivateDirectoryCreateCut) error
}

type windowsPrivateDirectoryCreateObserverFunc func(
	parent, target string,
	cut windowsPrivateDirectoryCreateCut,
) error

func (observe windowsPrivateDirectoryCreateObserverFunc) ObservePrivateDirectoryCreate(
	parent, target string,
	cut windowsPrivateDirectoryCreateCut,
) error {
	return observe(parent, target, cut)
}

func (directory *windowsDirectory) observePrivateDirectoryCreate(
	target string,
	cut windowsPrivateDirectoryCreateCut,
) error {
	if directory.createObserver == nil {
		return nil
	}
	observer, ok := directory.createObserver.(windowsPrivateDirectoryCreateObserver)
	if !ok {
		return nil
	}
	return observer.ObservePrivateDirectoryCreate(directory.path, target, cut)
}

func (directory *windowsDirectory) createPrivateDirectory(
	relative string,
) (_ *windowsDirectory, resultErr error) {
	const operation = "create crash-safe private output directory"
	if err := directory.usable(); err != nil {
		return nil, err
	}
	native, err := windowsRelativePath(relative, true)
	if err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	descriptor, err := directory.policy.descriptor(true)
	if err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	handle, status, err := windowsOpenNativeWithOptions(
		directory.handle(), native, windowsDirectoryAccess(), windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE,
		windows.FILE_ATTRIBUTE_HIDDEN, descriptor,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
	)
	runtime.KeepAlive(descriptor)
	if err != nil {
		return nil, windowsNativeNoReplaceFailure(operation, relative, err)
	}
	createdStatus, statusErr := windowsCreationStatus(windows.FILE_CREATE, status)
	if statusErr != nil || !createdStatus {
		return nil, errors.Join(
			windowsFailure(operation, relative, errWindowsOutputUnsafe, statusErr),
			windows.CloseHandle(handle), directory.Sync(),
		)
	}
	file := os.NewFile(uintptr(handle), relative)
	if file == nil {
		return nil, errors.Join(
			windowsFailure(operation, relative, errWindowsOutputUnsafe, errors.New("wrap directory handle")),
			windows.CloseHandle(handle), directory.Sync(),
		)
	}
	created := &windowsDirectory{
		file: file, metadataHandle: windows.InvalidHandle,
		path: filepath.Join(directory.path, relative), volume: directory.volume,
		objectIDs: directory.objectIDs, inspector: directory.inspector, policy: directory.policy,
		objectIDState: newWindowsPersistentObjectIDState(), ancestryAuthority: directory.ancestryAuthority,
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

	preparedFacts, err := directory.prepareCreatedPrivateDirectory(created, relative, native)
	if err != nil {
		return nil, err
	}
	if err := windowsCommitDeleteOnClose(created.handle()); err != nil {
		return nil, windowsNativeOperationFailure(operation, relative, err)
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsPrivateDirectoryCutCommitted); err != nil {
		return nil, err
	}
	if err := directory.Sync(); err != nil {
		return nil, err
	}
	if err := created.Close(); err != nil {
		return nil, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsPrivateDirectoryCutClosed); err != nil {
		return nil, err
	}
	reopened, err := directory.OpenPrivateDirectory(relative)
	if err != nil {
		return nil, windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	reopenedFacts, factsErr := reopened.inspector.Inspect(reopened.handle())
	if factsErr != nil || !preparedFacts.object.same(reopenedFacts.object) {
		return nil, errors.Join(
			windowsFailure(operation, relative, errWindowsOutputUnsafe,
				errors.New("committed private directory reopened as a different incarnation")),
			factsErr, reopened.Close(),
		)
	}
	return reopened, nil
}

func (directory *windowsDirectory) prepareCreatedPrivateDirectory(
	created *windowsDirectory,
	relative string,
	native string,
) (windowsObjectFacts, error) {
	const operation = "create crash-safe private output directory"
	if err := directory.observePrivateDirectoryCreate(relative, windowsPrivateDirectoryCutCreated); err != nil {
		return windowsObjectFacts{}, err
	}
	// Protected ACL verification is a private-namespace invariant. Object IDs are
	// deliberately absent here: live crash cleanup must remain available when
	// restart identity enrollment is not.
	if err := created.verify(true); err != nil {
		return windowsObjectFacts{}, err
	}
	if err := windowsVerifyOpenedLeafAuthority(created.handle(), native, true); err != nil {
		return windowsObjectFacts{}, err
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsPrivateDirectoryCutACLHidden); err != nil {
		return windowsObjectFacts{}, err
	}
	if err := errors.Join(created.Sync(), directory.Sync()); err != nil {
		return windowsObjectFacts{}, err
	}
	duplicate, err := created.Duplicate()
	if err != nil {
		return windowsObjectFacts{}, err
	}
	verifyErr := duplicate.verify(true)
	closeErr := duplicate.Close()
	if verifyErr != nil || closeErr != nil {
		return windowsObjectFacts{}, errors.Join(
			windowsFailure(operation, relative, errWindowsOutputUnsafe,
				errors.New("private directory did not preserve its protected security")),
			verifyErr, closeErr,
		)
	}
	preparedFacts, err := created.inspector.Inspect(created.handle())
	if err != nil {
		return windowsObjectFacts{},
			windowsFailure(operation, relative, errWindowsOutputUnsafe, err)
	}
	if err := directory.observePrivateDirectoryCreate(relative, windowsPrivateDirectoryCutSynced); err != nil {
		return windowsObjectFacts{}, err
	}
	return preparedFacts, nil
}

func windowsCommitDeleteOnClose(handle windows.Handle) error {
	// FileDispositionInfoEx with ON_CLOSE and without DELETE is the NT contract
	// for clearing a FILE_DELETE_ON_CLOSE link. This is the sole transition from
	// rollback-on-process-death to a persistent namespace entry.
	information := windowsDispositionInformation{Flags: windows.FILE_DISPOSITION_ON_CLOSE}
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
	)
}

type windowsPrivatePolicy struct {
	userSID           *windows.SID
	systemSID         *windows.SID
	administratorsSID *windows.SID
}

func newWindowsPrivatePolicy() (*windowsPrivatePolicy, error) {
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
	return &windowsPrivatePolicy{
		userSID: userSID, systemSID: systemSID, administratorsSID: administratorsSID,
	}, nil
}

func (policy *windowsPrivatePolicy) descriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
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

func (policy *windowsPrivatePolicy) verify(handle windows.Handle, directory bool) error {
	expectedFlags := uint8(0)
	if directory {
		expectedFlags = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
	}
	return policy.verifyObjectPolicy(
		handle, windows.SE_FILE_OBJECT, windowsFileAllAccess, expectedFlags,
	)
}

func (policy *windowsPrivatePolicy) verifyKernelMutex(handle windows.Handle) error {
	return policy.verifyObjectPolicy(handle, windows.SE_KERNEL_OBJECT, windows.MUTEX_ALL_ACCESS, 0)
}

func (policy *windowsPrivatePolicy) verifyObjectPolicy(
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
