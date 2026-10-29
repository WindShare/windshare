//go:build windows

package outputportable

import (
	"errors"
	"path/filepath"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

const initialResolvedPathUnits = 512

func entrySpelling(parent *directory, name string) (spelling string, resultErr error) {
	encoded, err := windows.UTF16PtrFromString(filepath.Join(parent.path, name))
	if err != nil {
		return "", err
	}
	// Inspecting a collision must not require permission to read foreign content.
	handle, err := windows.CreateFile(encoded, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", outputcap.ErrUnsafeNamespace
	}
	buffer := make([]uint16, initialResolvedPathUnits)
	for {
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return filepath.Base(windows.UTF16ToString(buffer[:length])), nil
		}
		buffer = make([]uint16, length+1)
	}
}

func exactEntrySpelling(requested, actual string) bool { return requested == actual }
