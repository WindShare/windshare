//go:build windows

package deliveryreceipts

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const maximumScratchCreateAttempts = 8

func newScratchFile() (scratchFile, error) {
	for range maximumScratchCreateAttempts {
		path := filepath.Join(os.TempDir(), "windshare-receipts-"+rand.Text())
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		// Delete-on-close is established by the exclusive create itself; there
		// is no pathname reopen or crash window leaving a receipt inventory.
		handle, err := windows.CreateFile(name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
			0, nil, windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(handle), path), nil
	}
	return nil, os.ErrExist
}
