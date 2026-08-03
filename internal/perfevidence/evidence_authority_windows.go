//go:build windows

package perfevidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openTreeAuthority(path string) (*stageDirectoryAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if isReparsePointInfo(info) || !info.IsDir() {
		return nil, fmt.Errorf("artifact tree %s is not a real directory", absolute)
	}
	handle, err := openWindowsDirectory(absolute, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: absolute, name: filepath.Base(absolute), identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func requireSecureWindowsAuthority(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return errors.New("evidence directory has no security descriptor")
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("evidence output root must be owned by the current process user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("evidence directory has a null DACL")
	}
	trusted := map[string]struct{}{
		user.User.Sid.String(): {},
		"S-1-5-18":             {}, // LocalSystem
		"S-1-5-32-544":         {}, // BUILTIN\Administrators
		"S-1-3-0":              {}, // Creator Owner
		"S-1-3-4":              {}, // Owner Rights
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("evidence directory DACL contains unsupported ACE type %d", ace.Header.AceType)
		}
		if ace.Mask&windowsMutationAccess == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return errors.New("evidence directory DACL contains an invalid SID")
		}
		if _, ok := trusted[sid.String()]; !ok {
			return fmt.Errorf("evidence directory grants mutation access to untrusted principal %s", sid.String())
		}
	}
	return nil
}

func directoryIdentityAt(path string) (identity directoryIdentity, resultErr error) {
	handle, err := openWindowsDirectory(
		path, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
	)
	if err != nil {
		return directoryIdentity{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	return windowsDirectoryIdentity(handle)
}

func openWindowsDirectory(path string, share uint32) (windows.Handle, error) {
	return openWindowsDirectoryAccess(
		path, share,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
}

func openWindowsDirectoryAccess(path string, share uint32, access uint32) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		name,
		access,
		share,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func windowsDirectoryIdentity(handle windows.Handle) (directoryIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return directoryIdentity{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return directoryIdentity{}, errors.New("filesystem authority is a reparse point")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return directoryIdentity{}, errors.New("filesystem authority is not a directory")
	}
	return directoryIdentity{
		volume: uint64(info.VolumeSerialNumber),
		object: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func (authority *outputRootAuthority) verifyPath() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return errors.New("evidence output authority is closed")
	}
	identity, err := directoryIdentityAt(authority.path)
	if err != nil {
		return fmt.Errorf("reidentify evidence output path: %w", err)
	}
	if identity != authority.identity {
		return errors.New("evidence output path no longer names the retained directory authority")
	}
	return nil
}

func (authority *outputRootAuthority) close() error {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil
	}
	handle := authority.handle
	authority.handle = windows.InvalidHandle
	return windows.CloseHandle(handle)
}

func (authority *outputRootAuthority) createChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	if err := authority.verifyPath(); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (authority *outputRootAuthority) openChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil, errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (authority *outputRootAuthority) openRecoveryChildAuthority(name string) (*stageDirectoryAuthority, error) {
	if authority == nil || authority.handle == windows.InvalidHandle {
		return nil, errors.New("evidence output authority is closed")
	}
	if err := requireDirectChildName(name); err != nil {
		return nil, err
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(err, windows.CloseHandle(handle))
	}
	return &stageDirectoryAuthority{
		path: filepath.Join(authority.path, name), name: name, identity: identity, handle: handle,
		leaseHandle: windows.InvalidHandle,
	}, nil
}

func (stage *stageDirectoryAuthority) acquireLiveLease(*outputRootAuthority) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	// The creator handle omits FILE_SHARE_DELETE, so the kernel rejects every
	// competing rename/delete authority until this handle is closed.
	stage.liveLease = true
	return nil
}

func (stage *stageDirectoryAuthority) tryAcquireRecoveryLease(
	authority *outputRootAuthority,
) (bool, error) {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return false, errors.New("stage directory authority is closed")
	}
	handle, err := openWindowsRelativeAccess(
		authority.handle, stage.name, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
	)
	if windowsSharingViolation(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire recovery delete lease: %w", err)
	}
	identity, identityErr := windowsDirectoryIdentity(handle)
	if identityErr != nil || identity != stage.identity {
		return false, errors.Join(
			errors.New("recovery delete lease names a substituted stage"), identityErr, windows.CloseHandle(handle),
		)
	}
	stage.leaseHandle = handle
	return true, nil
}

func windowsSharingViolation(err error) bool {
	if status, ok := errors.AsType[windows.NTStatus](err); ok {
		err = status.Errno()
	}
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

func (stage *stageDirectoryAuthority) modTime() (time.Time, error) {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return time.Time{}, errors.New("stage directory authority is closed")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(stage.handle, &info); err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, info.LastWriteTime.Nanoseconds()), nil
}

func (authority *outputRootAuthority) removeRetainedChild(
	stage *stageDirectoryAuthority,
	transition func(string) error,
) error {
	if stage == nil || stage.handle == windows.InvalidHandle || stage.leaseHandle == windows.InvalidHandle {
		return errors.New("recovery removal requires a retained delete-leased child authority")
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	if err := emptyWindowsDirectory(stage.handle, stage.name, transition); err != nil {
		return err
	}
	if err := stage.verifyName(authority); err != nil {
		return err
	}
	return markWindowsHandleForDeletion(stage.leaseHandle)
}

func (stage *stageDirectoryAuthority) verifyName(authority *outputRootAuthority) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	identity, err := directoryIdentityAt(filepath.Join(authority.path, stage.name))
	if err != nil {
		return err
	}
	if identity != stage.identity {
		return errors.New("stage name no longer identifies its retained directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) close() error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return nil
	}
	handle := stage.handle
	stage.handle = windows.InvalidHandle
	leaseHandle := stage.leaseHandle
	stage.leaseHandle = windows.InvalidHandle
	stage.liveLease = false
	var leaseCloseErr error
	if leaseHandle != windows.InvalidHandle {
		leaseCloseErr = windows.CloseHandle(leaseHandle)
	}
	return errors.Join(windows.CloseHandle(handle), leaseCloseErr)
}

func (stage *stageDirectoryAuthority) matchesAuthority(other *stageDirectoryAuthority) error {
	if stage == nil || other == nil || other.handle == windows.InvalidHandle {
		return errors.New("cannot compare closed directory authorities")
	}
	if stage.identity != other.identity {
		return errors.New("published authority does not identify the retained stage directory")
	}
	return nil
}

func (stage *stageDirectoryAuthority) openRegularFile(name string) (*os.File, os.FileInfo, error) {
	if filepath.Base(name) != name {
		return nil, nil, fmt.Errorf("artifact filename %s is not root-relative", name)
	}
	handle, err := openWindowsRelativeAccess(
		stage.handle, name, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
	)
	if err != nil {
		return nil, nil, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return nil, nil, errors.Join(err, windows.CloseHandle(handle))
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, nil, errors.Join(
			fmt.Errorf("artifact %s is not a regular non-reparse file", name),
			windows.CloseHandle(handle),
		)
	}
	file := os.NewFile(uintptr(handle), name)
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(err, file.Close())
	}
	return file, info, nil
}

func (stage *stageDirectoryAuthority) walkRegularFiles(visitor regularFileVisitor) error {
	return stage.walkEvidenceStore(&evidenceStoreWalk{meter: defaultEvidenceStoreMeter(), visitor: visitor})
}

func (stage *stageDirectoryAuthority) walkEvidenceStore(walk *evidenceStoreWalk) error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	return walkWindowsRegularFiles(stage.handle, "", walk, stage.transition)
}

func walkWindowsRegularFiles(
	parent windows.Handle,
	relative string,
	walk *evidenceStoreWalk,
	transition func(string, string) error,
) error {
	entries, err := readWindowsDirectory(parent, relative)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		childRelative := filepath.ToSlash(filepath.Join(relative, entry.Name()))
		handle, err := openWindowsRelativeAccess(
			parent, entry.Name(), windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		)
		if err != nil {
			return err
		}
		var opened windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
			return errors.Join(err, windows.CloseHandle(handle))
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.Join(
				fmt.Errorf("artifact %s is a reparse point", childRelative),
				windows.CloseHandle(handle),
			)
		}
		if opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			if err := walk.observeDirectory(childRelative); err != nil {
				return errors.Join(err, windows.CloseHandle(handle))
			}
			var walkErr error
			if transition != nil {
				walkErr = transition(childRelative, "directory-opened")
			}
			if walkErr == nil {
				walkErr = walkWindowsRegularFiles(handle, childRelative, walk, transition)
			}
			closeErr := windows.CloseHandle(handle)
			if err := errors.Join(walkErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
				return err
			}
			continue
		}
		file := os.NewFile(uintptr(handle), childRelative)
		info, statErr := file.Stat()
		visitErr := statErr
		if visitErr == nil {
			if transition != nil {
				visitErr = transition(childRelative, "file-opened")
			}
		}
		if visitErr == nil {
			visitErr = walk.observeFile(childRelative, file, info)
		}
		closeErr := file.Close()
		if err := errors.Join(visitErr, closeErr, verifyWindowsEntryIdentity(parent, entry.Name(), opened)); err != nil {
			return err
		}
	}
	return nil
}

func (stage *stageDirectoryAuthority) syncContents() error {
	if stage == nil || stage.handle == windows.InvalidHandle {
		return errors.New("stage directory authority is closed")
	}
	return syncWindowsDirectoryContents(stage.handle, "", stage.transition, defaultEvidenceStoreMeter())
}
