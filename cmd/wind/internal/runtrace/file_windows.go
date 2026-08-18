//go:build windows

package runtrace

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsOwnerOnlyFileAccess = windows.GENERIC_WRITE

func createOwnerOnlyFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	ownerSID := user.User.Sid.String()
	// A protected DACL must be supplied during CREATE_NEW. Tightening an
	// inherited ACL after creation would expose both trace contents and the
	// evidence-integrity boundary before the second syscall completed.
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + ownerSID + "D:P(A;;GA;;;" + ownerSID + ")",
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	security := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(
		name,
		windowsOwnerOnlyFileAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		&security,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		return nil, errors.Join(
			&os.PathError{Op: "open", Path: path, Err: errors.New("created Windows trace handle is invalid")},
			windows.CloseHandle(handle),
		)
	}
	return file, nil
}
