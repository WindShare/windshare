//go:build linux

package output

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/unix"
)

const procFileDescriptorDirectory = "/proc/self/fd"

func setDirModifiedTime(d *Dir, t time.Time) error {
	if d == nil || d.handle == nil {
		return fs.ErrClosed
	}
	mtime, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	times := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, mtime}
	err = unix.UtimesNanoAt(int(d.handle.Fd()), ".", times, 0)
	runtime.KeepAlive(d.handle)
	return err
}

func setFileModifiedTime(handle *os.File, t time.Time) error {
	if handle == nil {
		return fs.ErrClosed
	}
	mtime, err := unix.TimeToTimespec(t)
	if err != nil {
		return err
	}
	// AT_EMPTY_PATH binds the update to the open file descriptor without
	// depending on procfs, working reliably inside chroot fixtures.
	times := []unix.Timespec{{Nsec: unix.UTIME_OMIT}, mtime}
	err = unix.UtimesNanoAt(int(handle.Fd()), "", times, unix.AT_EMPTY_PATH)
	runtime.KeepAlive(handle)
	return err
}

func renameNoReplace(from int, old string, to int, name string) error {
	return noReplaceError(unix.Renameat2(from, old, to, name, unix.RENAME_NOREPLACE))
}

func entryMatchesSpelling(parent *Dir, name string) (exact bool, resultErr error) {
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	runtime.KeepAlive(parent.handle)
	if err != nil {
		return false, err
	}
	handle := os.NewFile(uintptr(fd), name)
	defer func() { _ = handle.Close() }()
	path, err := os.Readlink(filepath.Join(procFileDescriptorDirectory, strconv.Itoa(fd)))
	if err == nil {
		return exactEntrySpelling(name, filepath.Base(path)), nil
	}
	var observed unix.Stat_t
	if statErr := unix.Fstat(fd, &observed); statErr != nil {
		return false, statErr
	}
	return directoryEntryMatchesSpelling(parent, name, observed)
}

func exactEntrySpelling(requested, actual string) bool { return requested == actual }
func validateNativeName(string) error                  { return nil }
func isNativeNameRefusal(err error) bool               { return pathfailure.IsTooLong(err) }
