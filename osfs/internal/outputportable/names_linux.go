//go:build linux

package outputportable

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/windshare/windshare/osfs/internal/pathfailure"
	"golang.org/x/sys/unix"
)

const procFileDescriptorDirectory = "/proc/self/fd"

func entryMatchesSpelling(parent *directory, name string) (exact bool, resultErr error) {
	// O_PATH observes the resolved dentry without reading foreign file data.
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	handle := os.NewFile(uintptr(fd), name)
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	path, err := os.Readlink(filepath.Join(procFileDescriptorDirectory, strconv.Itoa(fd)))
	if err == nil {
		return exactEntrySpelling(name, filepath.Base(path)), nil
	}
	// Chroots need not mount procfs. Enumeration is only the fallback; normal
	// admission reads the already-open dentry without scanning siblings.
	info, statErr := handle.Stat()
	if statErr != nil {
		return false, statErr
	}
	return directoryEntryMatchesSpelling(parent, name, info)
}

func exactEntrySpelling(requested, actual string) bool { return requested == actual }

func validateNativeName(string) error { return nil }

func isNativeNameRefusal(err error) bool { return pathfailure.IsTooLong(err) }
