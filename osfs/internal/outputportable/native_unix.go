//go:build linux || darwin

package outputportable

import (
	"encoding/binary"
	"errors"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"syscall"
	"time"
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
	stamp := binary.LittleEndian.AppendUint64(nil, uint64(stat.Dev))
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
func setFileTimes(f *file, t time.Time) error {
	times := []unix.Timeval{unix.NsecToTimeval(t.UnixNano()), unix.NsecToTimeval(t.UnixNano())}
	return unix.Futimes(int(f.handle.Fd()), times)
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
