//go:build darwin

package output

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/unix"
	"golang.org/x/text/unicode/norm"
)

const darwinPathBufferBytes = 1024

func setDirModifiedTime(d *Dir, t time.Time) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(d.handle.Fd()), &stat); err != nil {
		return err
	}
	// Darwin may lower birth time when mtime is moved backwards. Set creation
	// and modification times together to preserve the destination's birth time.
	attributes := unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_CRTIME | unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME,
	}
	var encoded []byte
	for _, stamp := range []unix.Timespec{stat.Btim, unix.NsecToTimespec(t.UnixNano()), unix.NsecToTimespec(t.UnixNano())} {
		encoded = binary.NativeEndian.AppendUint64(encoded, uint64(stamp.Sec))
		encoded = binary.NativeEndian.AppendUint64(encoded, uint64(stamp.Nsec))
	}
	err := unix.Setattrlist(d.path, &attributes, encoded, unix.FSOPT_NOFOLLOW)
	runtime.KeepAlive(d.handle)
	return err
}

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
	info, err := handle.Stat()
	if err != nil {
		return false, err
	}
	if info.Mode().IsRegular() && info.Sys().(*syscall.Stat_t).Nlink > 1 {
		return directoryEntryMatchesSpelling(parent, name, info)
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
