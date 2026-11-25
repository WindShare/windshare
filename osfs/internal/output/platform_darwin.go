//go:build darwin

package output

import (
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

const darwinPathBufferBytes = 1024

func renameNoReplace(from int, old string, to int, name string) error {
	return noReplaceError(unix.RenameatxNp(from, old, to, name, unix.RENAME_EXCL))
}

func entryMatchesSpelling(parent *Dir, name string) (exact bool, resultErr error) {
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_EVTONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	runtime.KeepAlive(parent.handle)
	if err != nil {
		return false, err
	}
	handle := os.NewFile(uintptr(fd), name)
	defer func() { _ = handle.Close() }()
	var observed unix.Stat_t
	if err := unix.Fstat(fd, &observed); err != nil {
		return false, err
	}
	if observed.Mode&unix.S_IFMT == unix.S_IFREG && observed.Nlink > 1 {
		return directoryEntryMatchesSpelling(parent, name, observed)
	}
	var buffer [darwinPathBufferBytes]byte
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
	return requested == norm.NFC.String(actual)
}

func validateNativeName(string) error    { return nil }
func isNativeNameRefusal(err error) bool { return pathfailure.IsTooLong(err) }
