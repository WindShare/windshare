//go:build windows

package mutationdomain

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func cleanupWindowsPrivateChildren(rootPath string, names []string) error {
	root, err := openWindowsCleanupRoot(rootPath)
	if err != nil {
		return err
	}
	budget := productionMutationTraversalBudget()
	var errs []error
	for _, name := range names {
		if err := budget.admitCandidate(name); err != nil {
			errs = append(errs, err)
			break
		}
		if err := removeWindowsTreeAt(root, name, 0, budget); err != nil {
			errs = append(errs, fmt.Errorf("remove private mutation subtree %s: %w", name, err))
		}
	}
	return errors.Join(errors.Join(errs...), windows.CloseHandle(root))
}

func openWindowsCleanupRoot(path string) (windows.Handle, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var information windows.ByHandleFileInformation
	if infoErr := windows.GetFileInformationByHandle(handle, &information); infoErr != nil ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windows.InvalidHandle, errors.Join(
			errors.New("private mutation cleanup root is not a no-follow directory"),
			infoErr,
			windows.CloseHandle(handle),
		)
	}
	return handle, nil
}

func removeWindowsTreeAt(
	parent windows.Handle,
	leaf string,
	depth int,
	budget *mutationTraversalBudget,
) (resultErr error) {
	if filepath.Base(leaf) != leaf || leaf == "." || leaf == ".." {
		return fmt.Errorf("private mutation cleanup leaf %q is invalid", leaf)
	}
	handle, information, err := openWindowsCleanupObject(parent, leaf)
	if err != nil {
		return err
	}
	object := os.NewFile(uintptr(handle), leaf)
	defer func() { resultErr = errors.Join(resultErr, object.Close()) }()
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 &&
		information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		seen := make(map[string]struct{})
		for {
			entries, readErr := object.ReadDir(mutationTreeBatchSize)
			for _, entry := range entries {
				name := entry.Name()
				if filepath.Base(name) != name || name == "." || name == ".." {
					return fmt.Errorf("private mutation cleanup enumerated unsafe leaf %q", name)
				}
				if _, duplicate := seen[name]; duplicate {
					return fmt.Errorf("private mutation cleanup enumerated duplicate leaf %q", name)
				}
				seen[name] = struct{}{}
				info, infoErr := entry.Info()
				if infoErr != nil {
					return infoErr
				}
				contentBytes := int64(0)
				if info.Mode().IsRegular() {
					contentBytes = info.Size()
				}
				if err := budget.admitObject(filepath.Join(leaf, name), depth+1, contentBytes); err != nil {
					return err
				}
				if err := removeWindowsTreeAt(handle, name, depth+1, budget); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return markWindowsHandleForDeletion(handle)
}

func openWindowsCleanupObject(
	parent windows.Handle,
	leaf string,
) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: parent,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, information, nil
}
