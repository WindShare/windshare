//go:build windows

package perfevidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/windshare/windshare/internal/processowner/protocol"
	"golang.org/x/sys/windows"
)

func openWindowsConsumptionFile(path string) (*os.File, os.FileInfo, directoryIdentity, error) {
	return openWindowsConsumptionFileShared(path, windows.FILE_SHARE_READ)
}

func openWindowsConsumptionFileShared(
	path string,
	share uint32,
) (*os.File, os.FileInfo, directoryIdentity, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, directoryIdentity{}, err
	}
	handle, err := windows.CreateFile(
		name, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return nil, nil, directoryIdentity{}, err
	}
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		return nil, nil, directoryIdentity{}, errors.Join(err, windows.CloseHandle(handle))
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		opened.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, nil, directoryIdentity{}, errors.Join(
			errors.New("consumption byte is not a regular non-reparse file"), windows.CloseHandle(handle),
		)
	}
	file := os.NewFile(uintptr(handle), path)
	info, err := file.Stat()
	if err != nil {
		return nil, nil, directoryIdentity{}, errors.Join(err, file.Close())
	}
	return file, info, windowsFileIdentity(opened), nil
}

func (authority *windowsConsumptionAuthority) Verify() error {
	if authority == nil {
		return errors.New("consumption authority is nil")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	var errs []error
	errs = append(errs, authority.verifyPublicationWatchLocked())
	for _, directory := range authority.directories {
		identity, err := directoryIdentityAt(directory.path)
		if err != nil || identity != directory.identity {
			errs = append(errs, fmt.Errorf(
				"consumption directory %s no longer names its retained authority: %w", directory.path, err,
			))
		}
	}
	for _, protected := range authority.files {
		identity, err := windowsFileIdentityAt(protected.path)
		if err != nil || identity != protected.identity {
			errs = append(errs, fmt.Errorf(
				"consumption path %s no longer names its retained authority: %w", protected.path, err,
			))
		}
	}
	return errors.Join(errs...)
}

func (authority *windowsConsumptionAuthority) preparePublicationRename(source string) error {
	if err := authority.Verify(); err != nil {
		return err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return errors.New("consumption authority is closed")
	}
	for _, protected := range authority.files {
		if _, inside := relativeWithin(source, protected.path); !inside {
			return fmt.Errorf("sealed publication byte %s is outside its staging root", protected.path)
		}
	}
	if err := authority.startPublicationWatchLocked(source); err != nil {
		return err
	}
	// Windows refuses to rename a directory with open descendants even when
	// those handles share delete access. The subtree change ledger is started
	// while the exact file authorities still deny mutation; only then are the
	// descendant handles released for the no-replace rename. Any byte or
	// namespace event in that boundary makes the monotonic seal fail closed.
	var errs []error
	for _, protected := range authority.files {
		errs = append(errs, protected.file.Close())
	}
	authority.files = nil
	for _, directory := range authority.directories {
		errs = append(errs, windows.CloseHandle(directory.handle))
	}
	authority.directories = nil
	authority.publicationSource = filepath.Clean(source)
	return errors.Join(errs...)
}

func (authority *windowsConsumptionAuthority) completePublicationRename(destination string) error {
	authority.mu.Lock()
	if authority.closed {
		authority.mu.Unlock()
		return errors.New("consumption authority is closed")
	}
	if authority.publicationSource == "" {
		authority.mu.Unlock()
		return errors.New("publication rename was not prepared")
	}
	_ = destination
	authority.publicationSource = ""
	authority.mu.Unlock()
	return authority.Verify()
}

func (authority *windowsConsumptionAuthority) startPublicationWatchLocked(source string) error {
	var expected *protectedWindowsDirectory
	for index := range authority.directories {
		if samePath(authority.directories[index].path, source) {
			expected = &authority.directories[index]
			break
		}
	}
	if expected == nil {
		return errors.New("publication root is not covered by its retained directory authorities")
	}
	encoded, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_OVERLAPPED,
		0,
	)
	if err != nil {
		return err
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil || identity != expected.identity {
		return errors.Join(
			errors.New("publication mutation ledger retained a substituted stage"),
			err, windows.CloseHandle(handle),
		)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return errors.Join(err, windows.CloseHandle(handle))
	}
	const publicationWatchBufferBytes = 64 << 10
	authority.publicationWatchHandle = handle
	authority.publicationWatchEvent = event
	authority.publicationWatchOverlapped = windows.Overlapped{HEvent: event}
	authority.publicationWatchBuffer = make([]byte, publicationWatchBufferBytes)
	const mutationMask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE |
		windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION |
		windows.FILE_NOTIFY_CHANGE_SECURITY
	err = windows.ReadDirectoryChanges(
		handle,
		&authority.publicationWatchBuffer[0],
		uint32(len(authority.publicationWatchBuffer)),
		true,
		mutationMask,
		nil,
		&authority.publicationWatchOverlapped,
		0,
	)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return errors.Join(err, authority.closePublicationWatchLocked())
	}
	authority.publicationWatchPending = true
	return nil
}

func (authority *windowsConsumptionAuthority) verifyPublicationWatchLocked() error {
	if !authority.publicationWatchPending {
		return nil
	}
	var transferred uint32
	err := windows.GetOverlappedResult(
		authority.publicationWatchHandle,
		&authority.publicationWatchOverlapped,
		&transferred,
		false,
	)
	if errors.Is(err, windows.ERROR_IO_INCOMPLETE) {
		return nil
	}
	authority.publicationWatchPending = false
	if err != nil {
		return fmt.Errorf("read publication mutation ledger: %w", err)
	}
	return fmt.Errorf("staged publication mutated after sealing (%d notification bytes)", transferred)
}

func (authority *windowsConsumptionAuthority) closePublicationWatchLocked() error {
	if authority.publicationWatchHandle == 0 || authority.publicationWatchHandle == windows.InvalidHandle {
		return nil
	}
	cancelErr := windows.CancelIoEx(authority.publicationWatchHandle, &authority.publicationWatchOverlapped)
	if errors.Is(cancelErr, windows.ERROR_NOT_FOUND) || errors.Is(cancelErr, windows.ERROR_OPERATION_ABORTED) {
		cancelErr = nil
	}
	handleErr := windows.CloseHandle(authority.publicationWatchHandle)
	eventErr := windows.CloseHandle(authority.publicationWatchEvent)
	authority.publicationWatchHandle = windows.InvalidHandle
	authority.publicationWatchEvent = windows.InvalidHandle
	authority.publicationWatchBuffer = nil
	authority.publicationWatchPending = false
	return errors.Join(cancelErr, handleErr, eventErr)
}

func windowsFileIdentityAt(path string) (directoryIdentity, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return directoryIdentity{}, err
	}
	handle, err := windows.CreateFile(
		name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return directoryIdentity{}, err
	}
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(infoErr, closeErr); err != nil {
		return directoryIdentity{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return directoryIdentity{}, errors.New("consumption path is not a regular non-reparse file")
	}
	return windowsFileIdentity(info), nil
}

func (authority *windowsConsumptionAuthority) VerifyProcessStart(
	evidence protocol.StartEvidence,
	executable string,
) (bool, error) {
	if err := authority.Verify(); err != nil {
		return false, err
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	var expected *protectedWindowsFile
	for index := range authority.files {
		if samePath(authority.files[index].path, executable) {
			expected = &authority.files[index]
			break
		}
	}
	if expected == nil {
		return false, nil
	}
	if evidence.Platform != protocol.PlatformWindowsJob {
		return true, errors.New("contained process start evidence has the wrong platform")
	}
	expectedIdentity, err := windowsProtocolObjectIdentity(windows.Handle(expected.file.Fd()))
	if err != nil {
		return true, fmt.Errorf("identify retained executable authority: %w", err)
	}
	if evidence.Executable != expectedIdentity {
		return true, errors.New("contained process start evidence differs from its retained executable authority")
	}
	instance, err := strconv.ParseUint(evidence.ProcessInstance, 10, 64)
	if err != nil {
		return true, fmt.Errorf("parse contained process instance: %w", err)
	}
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false,
		uint32(evidence.ProcessID),
	)
	if err != nil {
		return true, fmt.Errorf("open contained process: %w", err)
	}
	var creation, exit, kernel, user windows.Filetime
	timeErr := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user)
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	queryErr := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size)
	closeErr := windows.CloseHandle(handle)
	if err := errors.Join(timeErr, queryErr, closeErr); err != nil {
		return true, fmt.Errorf("identify contained process instance and image: %w", err)
	}
	observedInstance := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if observedInstance != instance {
		return true, errors.New("contained process instance differs from authenticated start evidence")
	}
	imagePath := windows.UTF16ToString(buffer[:size])
	imageIdentity, err := windowsProtocolObjectIdentityAt(imagePath)
	if err != nil {
		return true, fmt.Errorf("identify contained process image bytes: %w", err)
	}
	if imageIdentity != expectedIdentity || imageIdentity != evidence.Executable {
		return true, errors.New("contained process image differs from its retained executable authority")
	}
	return true, nil
}

type windowsProtocolFileIDInfo struct {
	volume uint64
	object [16]byte
}

func windowsProtocolObjectIdentity(handle windows.Handle) (protocol.ObjectIdentity, error) {
	var identity windowsProtocolFileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&identity)),
		uint32(unsafe.Sizeof(identity)),
	); err != nil {
		return protocol.ObjectIdentity{}, err
	}
	return protocol.NewObjectIdentity128(identity.volume, identity.object), nil
}

func windowsProtocolObjectIdentityAt(path string) (protocol.ObjectIdentity, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return protocol.ObjectIdentity{}, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return protocol.ObjectIdentity{}, err
	}
	identity, identityErr := windowsProtocolObjectIdentity(handle)
	closeErr := windows.CloseHandle(handle)
	return identity, errors.Join(identityErr, closeErr)
}

func (authority *windowsConsumptionAuthority) Close() error {
	if authority == nil {
		return nil
	}
	verifyErr := authority.Verify()
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return errors.Join(verifyErr, authority.closeWithoutVerifyLocked())
}

func (authority *windowsConsumptionAuthority) closeWithoutVerify() error {
	authority.mu.Lock()
	defer authority.mu.Unlock()
	return authority.closeWithoutVerifyLocked()
}

func (authority *windowsConsumptionAuthority) closeWithoutVerifyLocked() error {
	if authority.closed {
		return nil
	}
	authority.closed = true
	var errs []error
	errs = append(errs, authority.closePublicationWatchLocked())
	for _, protected := range authority.files {
		errs = append(errs, protected.file.Close())
	}
	authority.files = nil
	for _, directory := range authority.directories {
		errs = append(errs, windows.CloseHandle(directory.handle))
	}
	authority.directories = nil
	return errors.Join(errs...)
}

type windowsFileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	attributes     uint32
}

type windowsFileRenameInformation struct {
	flags          uint32
	rootDirectory  windows.Handle
	fileNameLength uint32
	fileName       [1]uint16
}

func openOutputRootAuthority(path string) (*outputRootAuthority, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence output root: %w", err)
	}
	parent := filepath.Dir(absolute)
	if !samePath(parent, absolute) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fmt.Errorf("create evidence output parent: %w", err)
		}
	}
	handle, created, err := openOrCreateWindowsOutputRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open evidence output authority: %w", err)
	}
	resolved, err := resolveDirectoryAuthority(absolute)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("resolve evidence output authority: %w", err),
			windows.CloseHandle(handle),
		)
	}
	identity, err := windowsDirectoryIdentity(handle)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("identify evidence output authority: %w", err),
			windows.CloseHandle(handle),
		)
	}
	if err := requireSecureWindowsAuthority(handle); err != nil {
		return nil, errors.Join(
			fmt.Errorf("validate evidence output security: %w", err),
			windows.CloseHandle(handle),
		)
	}
	resolvedIdentity, err := directoryIdentityAt(resolved)
	if err != nil || resolvedIdentity != identity {
		return nil, errors.Join(
			errors.New("resolved evidence output path does not identify its retained authority"),
			err,
			windows.CloseHandle(handle),
		)
	}
	if created {
		entries, readErr := readWindowsDirectory(handle, resolved)
		if readErr != nil || len(entries) != 0 {
			return nil, errors.Join(
				errors.New("new evidence output root was not empty after secure creation"),
				readErr,
				windows.CloseHandle(handle),
			)
		}
	}
	return &outputRootAuthority{path: resolved, identity: identity, handle: handle}, nil
}

func openOrCreateWindowsOutputRoot(path string) (windows.Handle, bool, error) {
	descriptor, err := privateWindowsDirectoryDescriptor()
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	ntPath := `\??\` + path
	if uncPath, found := strings.CutPrefix(path, `\\`); found {
		ntPath = `\??\UNC\` + uncPath
	}
	objectName, err := windows.NewNTUnicodeString(ntPath)
	if err != nil {
		return windows.InvalidHandle, false, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: descriptor,
	}
	var status windows.IO_STATUS_BLOCK
	allocationSize := int64(0)
	handle := windows.InvalidHandle
	createErr := windows.NtCreateFile(
		&handle,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_GENERIC_WRITE,
		&attributes,
		&status,
		&allocationSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if createErr == nil {
		return handle, true, nil
	}
	handle, openErr := openWindowsDirectoryAccess(
		path,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|
			windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_GENERIC_WRITE,
	)
	if openErr != nil {
		return windows.InvalidHandle, false, errors.Join(createErr, openErr)
	}
	return handle, false, nil
}

func privateWindowsDirectoryDescriptor() (*windows.SECURITY_DESCRIPTOR, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;OICI;FA;;;%s)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)",
		user.User.Sid.String(),
	))
}
