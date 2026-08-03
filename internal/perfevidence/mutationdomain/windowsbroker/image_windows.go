//go:build windows

package windowsbroker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type retainedBrokerImage struct {
	path        string
	file        *os.File
	directories []windows.Handle
}

func createRetainedBrokerImage(_ string) (*retainedBrokerImage, error) {
	currentPath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	currentName, err := windows.UTF16PtrFromString(currentPath)
	if err != nil {
		return nil, err
	}
	currentHandle, err := windows.CreateFile(
		currentName, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return nil, err
	}
	image := &retainedBrokerImage{path: currentPath, file: os.NewFile(uintptr(currentHandle), currentPath)}
	fail := func(operationErr error) (*retainedBrokerImage, error) {
		return nil, errors.Join(operationErr, image.close())
	}
	for directory := filepath.Dir(currentPath); ; directory = filepath.Dir(directory) {
		encoded, err := windows.UTF16PtrFromString(directory)
		if err != nil {
			return fail(err)
		}
		handle, err := windows.CreateFile(
			encoded, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
		)
		if err != nil {
			return fail(fmt.Errorf("retain broker image ancestor %s: %w", directory, err))
		}
		image.directories = append(image.directories, handle)
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
	}
	finalPath, err := finalWindowsHandlePath(currentHandle)
	if err != nil {
		return fail(err)
	}
	if !strings.EqualFold(normalizeWindowsPath(finalPath), normalizeWindowsPath(currentPath)) {
		return fail(fmt.Errorf("broker image path changed while its ancestor authority was acquired"))
	}
	return image, nil
}

func (image *retainedBrokerImage) close() error {
	if image == nil || image.file == nil {
		return nil
	}
	var errs []error
	errs = append(errs, image.file.Close())
	image.file = nil
	for _, directory := range image.directories {
		errs = append(errs, windows.CloseHandle(directory))
	}
	image.directories = nil
	return errors.Join(errs...)
}

func markWindowsHandleForDeletion(handle windows.Handle) error {
	information := uint32(1)
	return windows.SetFileInformationByHandle(
		handle, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&information)), uint32(unsafe.Sizeof(information)),
	)
}

func openWindowsObjectForDeletion(
	root windows.Handle,
	leaf string,
	directory bool,
) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: root,
		ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.DELETE|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options,
		0,
		0,
	)
	return handle, err
}

func duplicateInheritableHandle(source windows.Handle) (windows.Handle, error) {
	var duplicate windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(), source, windows.CurrentProcess(), &duplicate,
		windows.GENERIC_READ, true, 0,
	)
	return duplicate, err
}

func verifyWindowsProcessImage(
	process windows.Handle,
	expected *os.File,
	expectedPath string,
	reopen bool,
) error {
	if process == 0 || process == windows.InvalidHandle || expected == nil {
		return errors.New("process image authority is unavailable")
	}
	buffer := make([]uint16, 32_768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return err
	}
	observedPath := windows.UTF16ToString(buffer[:size])
	if !strings.EqualFold(normalizeWindowsPath(observedPath), normalizeWindowsPath(expectedPath)) {
		return fmt.Errorf("launched image path %s does not match retained image %s", observedPath, expectedPath)
	}
	if !reopen {
		// The retained handle denies write and delete sharing, so equality of the
		// kernel-reported path binds the suspended process to that exact object.
		return nil
	}
	encoded, err := windows.UTF16PtrFromString(observedPath)
	if err != nil {
		return err
	}
	observed, err := windows.CreateFile(
		encoded, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0,
	)
	if err != nil {
		return err
	}
	var expectedInfo, observedInfo windows.ByHandleFileInformation
	expectedErr := windows.GetFileInformationByHandle(windows.Handle(expected.Fd()), &expectedInfo)
	observedErr := windows.GetFileInformationByHandle(observed, &observedInfo)
	closeErr := windows.CloseHandle(observed)
	if err := errors.Join(expectedErr, observedErr, closeErr); err != nil {
		return err
	}
	if expectedInfo.VolumeSerialNumber != observedInfo.VolumeSerialNumber ||
		expectedInfo.FileIndexHigh != observedInfo.FileIndexHigh || expectedInfo.FileIndexLow != observedInfo.FileIndexLow {
		return errors.New("launched private mutation process image was substituted")
	}
	return nil
}

func finalWindowsHandlePath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 512)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:length]), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func normalizeWindowsPath(path string) string {
	clean := filepath.Clean(path)
	if uncPath, found := strings.CutPrefix(clean, `\\?\UNC\`); found {
		return `\\` + uncPath
	}
	return strings.TrimPrefix(clean, `\\?\`)
}

func windowsEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := append([]string(nil), environment...)
	sort.Slice(entries, func(left, right int) bool {
		return strings.ToLower(entries[left]) < strings.ToLower(entries[right])
	})
	for _, entry := range entries {
		if strings.ContainsRune(entry, '\x00') || !strings.Contains(entry, "=") {
			return nil, fmt.Errorf("invalid Windows environment entry %q", entry)
		}
	}
	return utf16.Encode([]rune(strings.Join(entries, "\x00") + "\x00\x00")), nil
}

func windowsNTPath(path string) string {
	clean := filepath.Clean(path)
	switch {
	case strings.HasPrefix(clean, `\\?\UNC\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\?\UNC\`)
	case strings.HasPrefix(clean, `\\?\`):
		return `\??\` + strings.TrimPrefix(clean, `\\?\`)
	case strings.HasPrefix(clean, `\\`):
		return `\??\UNC\` + strings.TrimPrefix(clean, `\\`)
	default:
		return `\??\` + clean
	}
}

type RetainedImage = retainedBrokerImage

func CreateRetainedImage(runtimeRoot string) (*RetainedImage, error) {
	return createRetainedBrokerImage(runtimeRoot)
}

func (image *retainedBrokerImage) Path() string {
	if image == nil {
		return ""
	}
	return image.path
}

func (image *retainedBrokerImage) File() *os.File {
	if image == nil {
		return nil
	}
	return image.file
}

func (image *retainedBrokerImage) Close() error {
	return image.close()
}

func DuplicateInheritableHandle(source windows.Handle) (windows.Handle, error) {
	return duplicateInheritableHandle(source)
}

func VerifyProcessImage(
	process windows.Handle,
	expected *os.File,
	expectedPath string,
	reopen bool,
) error {
	return verifyWindowsProcessImage(process, expected, expectedPath, reopen)
}

func FinalHandlePath(handle windows.Handle) (string, error) {
	return finalWindowsHandlePath(handle)
}

func NormalizePath(path string) string {
	return normalizeWindowsPath(path)
}

func NTPath(path string) string {
	return windowsNTPath(path)
}
