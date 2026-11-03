//go:build darwin

package outputportable

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

// F_GETPATH requires a MAXPATHLEN buffer from the Darwin fcntl contract.
const darwinPathBufferBytes = 1024

func entryMatchesSpelling(parent *directory, name string) (exact bool, resultErr error) {
	// O_EVTONLY permits metadata inspection without reading foreign file data.
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	handle := os.NewFile(uintptr(fd), name)
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	info, err := handle.Stat()
	if err != nil {
		return false, err
	}
	// F_GETPATH describes the vnode's cached path, not the directory entry used
	// by Openat. Other hard-link lookups can change it while this handle is open.
	// Keep the constant-time path only for objects with one namespace entry.
	if info.Mode().IsRegular() && info.Sys().(*syscall.Stat_t).Nlink > 1 {
		return directoryEntryMatchesSpelling(parent, name, info)
	}
	var buffer [darwinPathBufferBytes]byte
	// FcntlInt uses libSystem; pin the buffer while its pointer travels as an integer.
	var pinned runtime.Pinner
	pinned.Pin(&buffer[0])
	defer pinned.Unpin()
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buffer[0]))))
	if err != nil {
		return false, err
	}
	return exactEntrySpelling(name, filepath.Base(unix.ByteSliceToString(buffer[:]))), nil
}

func exactEntrySpelling(requested, actual string) bool {
	// macOS may expose decomposed names even when the catalog spelling is NFC.
	// This accepts normalization only after native lookup proved the same entry.
	return requested == norm.NFC.String(actual)
}

func validateNativeName(string) error { return nil }

func isNativeNameRefusal(err error) bool { return pathfailure.IsTooLong(err) }
