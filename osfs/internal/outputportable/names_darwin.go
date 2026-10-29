//go:build darwin

package outputportable

import (
	"errors"
	"path/filepath"
	"runtime"
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
	// FcntlInt uses libSystem; pin the buffer while its pointer travels as an integer.
	var pinned runtime.Pinner
	pinned.Pin(&buffer[0])
	defer pinned.Unpin()
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buffer[0]))))
	if err != nil {
		return "", err
	}
	return filepath.Base(unix.ByteSliceToString(buffer[:])), nil
}

func exactEntrySpelling(requested, actual string) bool {
	// macOS may expose decomposed names even when the catalog spelling is NFC.
	// This accepts normalization only after native lookup proved the same entry.
	return requested == norm.NFC.String(actual)
}
