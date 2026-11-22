//go:build linux || darwin

package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

// A destination the user names through a final symlink (a linked download
// folder, or a working directory entered through one) is accepted only when the
// kernel itself follows that link. filepath.EvalSymlinks resolves in user space
// and so bypasses kernel link policy such as Linux fs.protected_symlinks, which
// refuses links planted by another user in sticky world-writable directories.
// Non-symlink containers pass through unchanged (leaving intermediate links
// unexpanded), while a final symlink expands to its real directory and pins the
// opened handle against the kernel-followed identity; every name inside the
// container stays no-follow.
func resolveContainerTarget(
	path string,
	info os.FileInfo,
	follow func(string) (os.FileInfo, error),
) (string, os.FileInfo, error) {
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.IsDir() {
			return "", nil, outputcap.ErrUnsafeNamespace
		}
		return path, info, nil
	}
	followed, err := follow(path)
	if err != nil || !followed.IsDir() {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil || !target.IsDir() || target.Mode()&os.ModeSymlink != 0 || !os.SameFile(followed, target) {
		return "", nil, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	return resolved, followed, nil
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
