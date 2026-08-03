//go:build windows

package windowsbroker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func (creator sealedObjectCreator) create(
	root windows.Handle,
	name string,
	directory bool,
) (windows.Handle, error) {
	if creator.token == 0 {
		handle, err := createSealedObject(root, name, directory, creator.descriptor)
		if err == nil && creator.finalDescriptor != "" {
			if sealErr := sealWindowsNamedDACL(handle, creator.finalDescriptor); sealErr != nil {
				err = fmt.Errorf("seal newly created object DACL: %w", sealErr)
			}
		}
		if err != nil && handle != 0 && handle != windows.InvalidHandle {
			return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
		}
		return handle, err
	}
	if err := windows.SetThreadToken(nil, creator.token); err != nil {
		return windows.InvalidHandle, err
	}
	handle, createErr := createSealedObject(root, name, directory, creator.descriptor)
	revertErr := windows.RevertToSelf()
	if createErr != nil && handle != windows.InvalidHandle {
		return windows.InvalidHandle, errors.Join(createErr, revertErr, windows.CloseHandle(handle))
	}
	return handle, errors.Join(createErr, revertErr)
}

func sealWindowsNamedDACL(handle windows.Handle, descriptorText string) error {
	path, err := finalWindowsHandlePath(handle)
	if err != nil {
		return err
	}
	descriptor, err := windows.SecurityDescriptorFromString(descriptorText)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("set named DACL revision %d: %w", *(*byte)(unsafe.Pointer(dacl)), err)
	}
	return nil
}

func createSealedObject(
	root windows.Handle,
	name string,
	directory bool,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	desired := uint32(
		windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
			windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_EXECUTE |
			windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.WRITE_DAC |
			windows.DELETE | windows.SYNCHRONIZE,
	)
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	share := uint32(windows.FILE_SHARE_READ)
	if directory {
		const (
			fileAddSubdirectory = 0x00000004
			fileDeleteChild     = 0x00000040
		)
		desired = windows.FILE_LIST_DIRECTORY | windows.FILE_WRITE_DATA | fileAddSubdirectory |
			windows.FILE_READ_EA | windows.FILE_WRITE_EA | windows.FILE_TRAVERSE | fileDeleteChild |
			windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.WRITE_DAC |
			windows.DELETE | windows.SYNCHRONIZE
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle, desired, attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
		share, windows.FILE_CREATE, options, 0, 0,
	)
	return handle, err
}

func (authority *appContainerAuthority) close() error {
	if authority == nil {
		return nil
	}
	var errs []error
	if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
		teardownDescriptor, err := appContainerHelperFileTeardownDescriptor()
		if err == nil {
			err = sealWindowsHandleDACL(authority.helperSecurity, teardownDescriptor)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("enter retained helper image teardown phase: %w", err))
		}
	}
	if authority.helper != nil {
		errs = append(errs, authority.helper.Close())
		authority.helper = nil
	}
	if authority.helperDirectory != 0 && authority.helperDirectory != windows.InvalidHandle {
		teardownDescriptor, err := appContainerHelperTeardownDescriptor()
		granted, grantedErr := windowsHandleGrantedAccess(authority.helperDirectory)
		if err == nil {
			err = sealWindowsHandleDACL(authority.helperDirectory, teardownDescriptor)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"enter retained helper teardown phase (granted=0x%08x): %w",
				granted,
				errors.Join(err, grantedErr),
			))
		}
		if authority.helperLeaf != "" {
			err := removeWindowsImageAfterSettlement(authority.helperPath)
			if err != nil {
				errs = append(errs, fmt.Errorf("unlink retained helper image: %w", err))
			}
			authority.helperLeaf = ""
		}
		if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
			errs = append(errs, windows.CloseHandle(authority.helperSecurity))
			authority.helperSecurity = 0
		}
		if err := markWindowsHandleForDeletion(authority.helperDirectory); err != nil {
			errs = append(errs, fmt.Errorf("unlink retained helper directory: %w", err))
		}
		errs = append(errs, windows.CloseHandle(authority.helperDirectory))
		authority.helperDirectory = 0
	}
	if authority.helperSecurity != 0 && authority.helperSecurity != windows.InvalidHandle {
		errs = append(errs, windows.CloseHandle(authority.helperSecurity))
		authority.helperSecurity = 0
	}
	if authority.root != 0 && authority.root != windows.InvalidHandle {
		if err := markWindowsHandleForDeletion(authority.root); err != nil {
			errs = append(errs, fmt.Errorf("unlink private AppContainer root: %w", err))
		}
		errs = append(errs, windows.CloseHandle(authority.root))
		authority.root = 0
	}
	if authority.packageSID != nil {
		errs = append(errs, releaseNativeAppContainerSID(authority.packageSID))
		authority.packageSID = nil
	}
	authority.traditionalSID = nil
	authority.capabilitySID = nil
	if authority.profileName != "" {
		profileErr := deleteEphemeralAppContainerProfile(authority.profileName)
		if profileErr == nil && authority.profileMarker != "" {
			profileErr = os.Remove(authority.profileMarker)
			if errors.Is(profileErr, os.ErrNotExist) {
				profileErr = nil
			}
		}
		if profileErr == nil {
			authority.profileName = ""
			authority.profileMarker = ""
		}
		errs = append(errs, profileErr)
	}
	return errors.Join(errs...)
}

func removeWindowsImageAfterSettlement(path string) error {
	deadline := time.Now().Add(windowsImageTeardownTimeout)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("image section did not settle before teardown deadline: %w", err)
		}
		time.Sleep(windowsImageTeardownPollInterval)
	}
}

func windowsHandleGrantedAccess(handle windows.Handle) (uint32, error) {
	information := windowsObjectBasicInformation{}
	status, _, _ := ntQueryObject.Call(
		uintptr(handle),
		0,
		uintptr(unsafe.Pointer(&information)),
		unsafe.Sizeof(information),
		0,
	)
	if int32(status) < 0 {
		return 0, windows.NTStatus(uint32(status))
	}
	return information.GrantedAccess, nil
}

func copySealedFile(
	source io.Reader,
	parent windows.Handle,
	name string,
	creator ObjectCreator,
	retain bool,
) (*os.File, string, error) {
	if creator == nil {
		return nil, "", errors.New("sealed object creator is unavailable")
	}
	handle, err := creator(parent, name, false)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(handle), name)
	hasher := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hasher), source)
	flushErr := windows.FlushFileBuffers(handle)
	_, seekErr := file.Seek(0, io.SeekStart)
	if err := errors.Join(copyErr, flushErr, seekErr); err != nil {
		return nil, "", errors.Join(err, file.Close())
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if retain {
		return file, digest, nil
	}
	return nil, digest, file.Close()
}

func CopySealedFile(
	source io.Reader,
	parent windows.Handle,
	name string,
	creator ObjectCreator,
	retain bool,
) (*os.File, string, error) {
	return copySealedFile(source, parent, name, creator, retain)
}

func NewObjectCreator(
	token windows.Token,
	descriptor *windows.SECURITY_DESCRIPTOR,
	finalDescriptor string,
) ObjectCreator {
	return sealedObjectCreator{
		token: token, descriptor: descriptor, finalDescriptor: finalDescriptor,
	}.create
}

func CreateSealedObject(
	root windows.Handle,
	name string,
	directory bool,
	descriptor *windows.SECURITY_DESCRIPTOR,
) (windows.Handle, error) {
	return createSealedObject(root, name, directory, descriptor)
}

func AppContainerObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return appContainerObjectDescriptor(traditionalUserSID, capabilitySID)
}

func AppContainerReadOnlyObjectDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return appContainerReadOnlyObjectDescriptor(traditionalUserSID, capabilitySID)
}

func AppContainerProcessDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return appContainerProcessDescriptor(traditionalUserSID, capabilitySID)
}

func AppContainerThreadDescriptor(traditionalUserSID, capabilitySID *windows.SID) string {
	return appContainerThreadDescriptor(traditionalUserSID, capabilitySID)
}

func FinalizeExecutableFile(
	writable *os.File,
	directory windows.Handle,
	leaf string,
	path string,
	fileDescriptor string,
	directoryDescriptor string,
) (*os.File, windows.Handle, error) {
	return finalizeWindowsExecutableFile(
		writable, directory, leaf, path, fileDescriptor, directoryDescriptor,
	)
}

func SealKernelHandleDACL(handle windows.Handle, descriptorText string) error {
	return sealWindowsKernelHandleDACL(handle, descriptorText)
}

func SealHandleDACL(handle windows.Handle, descriptorText string) error {
	return sealWindowsHandleDACL(handle, descriptorText)
}

func RandomBytes(count int) ([]byte, error) {
	return randomBytes(count)
}

func MarkHandleForDeletion(handle windows.Handle) error {
	return markWindowsHandleForDeletion(handle)
}

func OpenObjectForDeletion(root windows.Handle, leaf string, directory bool) (windows.Handle, error) {
	return openWindowsObjectForDeletion(root, leaf, directory)
}

func sameWindowsObject(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber &&
		left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}
