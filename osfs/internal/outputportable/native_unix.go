//go:build linux || darwin

package outputportable

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func metadataStamp(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, outputcap.ErrUnsafeNamespace
	}
	stamp := binary.LittleEndian.AppendUint64(nil, deviceID(stat))
	return binary.LittleEndian.AppendUint64(stamp, stat.Ino), nil
}
func openDirectoryHandle(root *os.Root) (*os.File, error) { return root.Open(".") }
func openFileAt(d *directory, name string, flags int, mode fs.FileMode) (*os.File, error) {
	return d.root.OpenFile(name, flags, mode)
}
func syncDirectory(f *os.File) error {
	err := f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}
func lockFile(f *os.File) (func() error, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(outputcap.ErrNamespaceLockBusy, err)
		}
		return nil, err
	}
	return func() error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }, nil
}

// Normalize only the flagged syscall result: a rejected flag has not published
// anything, whereas transport or storage errors can leave the result uncertain.
func noReplaceError(err error) error {
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
		return errors.Join(outputcap.ErrOrdinaryOutputUnsupported, err)
	}
	return err
}

func renameFile(from *file, to *directory, name string, replace bool) error {
	current, err := from.parent.root.Lstat(from.name)
	expected, statErr := from.handle.Stat()
	if err != nil || statErr != nil || !os.SameFile(current, expected) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err, statErr)
	}
	if replace {
		return unix.Renameat(int(from.parent.handle.Fd()), from.name, int(to.handle.Fd()), name)
	}
	return renameNoReplace(int(from.parent.handle.Fd()), from.name, int(to.handle.Fd()), name)
}
func renameDirectory(from, to *directory, name string) error {
	if err := from.validate(); err != nil {
		return err
	}
	return renameNoReplace(unix.AT_FDCWD, from.path, int(to.handle.Fd()), name)
}
func linkFile(from *file, to *directory, name string) error {
	return unix.Linkat(int(from.parent.handle.Fd()), from.name, int(to.handle.Fd()), name, 0)
}
