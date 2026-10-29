//go:build darwin

package outputportable

import (
	"errors"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

// F_GETPATH requires a MAXPATHLEN buffer from the Darwin fcntl contract.
const darwinPathBufferBytes = 1024

func entrySpelling(parent *directory, name string) (spelling string, resultErr error) {
	// O_EVTONLY permits metadata inspection without reading foreign file data.
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer func() { resultErr = errors.Join(resultErr, unix.Close(fd)) }()
	var buffer [darwinPathBufferBytes]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), unix.F_GETPATH, uintptr(unsafe.Pointer(&buffer[0])))
	if errno != 0 {
		return "", errno
	}
	return filepath.Base(unix.ByteSliceToString(buffer[:])), nil
}

func exactEntrySpelling(requested, actual string) bool {
	// macOS may expose decomposed names even when the catalog spelling is NFC.
	// This accepts normalization only after native lookup proved the same entry.
	return requested == norm.NFC.String(actual)
}
