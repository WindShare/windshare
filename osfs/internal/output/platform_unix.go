//go:build linux || darwin

package output

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

const nativeSpellingBatchSize = 128

// A destination named through a final symlink is accepted only when the kernel
// itself follows that link (respecting Linux fs.protected_symlinks). Every name
// inside the container stays strictly O_NOFOLLOW.
func resolveContainerTarget(
	path string,
	info os.FileInfo,
	follow func(string) (os.FileInfo, error),
) (string, os.FileInfo, error) {
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.IsDir() {
			return "", nil, ErrUnsafeNamespace
		}
		return path, info, nil
	}
	followed, err := follow(path)
	if err != nil || !followed.IsDir() {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil || !target.IsDir() || target.Mode()&os.ModeSymlink != 0 || !os.SameFile(followed, target) {
		return "", nil, errors.Join(ErrUnsafeNamespace, err)
	}
	return resolved, followed, nil
}

func openDirHandle(path string) (*Dir, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, errors.Join(ErrUnsafeNamespace, err)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle := os.NewFile(uintptr(fd), path)
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		return nil, errors.Join(ErrUnsafeNamespace, err, handle.Close())
	}
	return &Dir{path: path, handle: handle, info: info}, nil
}

func openVerifiedDirHandle(path string, expected os.FileInfo) (*Dir, error) {
	dir, err := openDirHandle(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, dir.info) {
		return nil, errors.Join(ErrUnsafeNamespace, dir.Close())
	}
	return dir, nil
}

func openChildDir(parent *Dir, name string) (*Dir, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.handle.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(parent.handle)
	if err != nil {
		if isNativeNameRefusal(err) {
			return nil, errors.Join(ErrNameRejected, err)
		}
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, errors.Join(ErrUnsafeNamespace, err)
		}
		return nil, &os.PathError{Op: "openat", Path: filepath.Join(parent.path, name), Err: err}
	}
	childPath := filepath.Join(parent.path, name)
	handle := os.NewFile(uintptr(fd), childPath)
	info, err := handle.Stat()
	if err != nil || !info.IsDir() {
		return nil, errors.Join(ErrUnsafeNamespace, err, handle.Close())
	}
	return &Dir{path: childPath, handle: handle, info: info}, nil
}

func openChildFile(parent *Dir, name string, flags int, mode fs.FileMode) (*os.File, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.handle.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode))
	runtime.KeepAlive(parent.handle)
	if err != nil {
		if isNativeNameRefusal(err) {
			return nil, errors.Join(ErrNameRejected, err)
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.Join(ErrUnsafeNamespace, err)
		}
		return nil, &os.PathError{Op: "openat", Path: filepath.Join(parent.path, name), Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(ErrUnsafeNamespace, err, f.Close())
	}
	return f, nil
}

func openJournalHeaderFile(opDir *Dir) (*os.File, error) {
	return openChildFile(opDir, journalFileName, os.O_RDONLY, 0)
}

func lstatChild(parent *Dir, name string) (EntryKind, uint64, error) {
	if err := validName(name); err != nil {
		return 0, 0, err
	}
	var st unix.Stat_t
	err := unix.Fstatat(int(parent.handle.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW)
	runtime.KeepAlive(parent.handle)
	if errors.Is(err, unix.ENOENT) {
		return EntryAbsent, 0, nil
	}
	if err != nil {
		if isNativeNameRefusal(err) {
			return 0, 0, errors.Join(ErrNameRejected, err)
		}
		return 0, 0, &os.PathError{Op: "fstatat", Path: filepath.Join(parent.path, name), Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return EntryRegularFile, uint64(max(st.Size, 0)), nil
	case unix.S_IFDIR:
		return EntryDirectory, 0, nil
	default:
		return EntryOther, 0, nil
	}
}

func mkdirChild(parent *Dir, name string, mode fs.FileMode) error {
	if err := validName(name); err != nil {
		return err
	}
	err := unix.Mkdirat(int(parent.handle.Fd()), name, uint32(mode))
	runtime.KeepAlive(parent.handle)
	if err != nil {
		if isNativeNameRefusal(err) {
			return errors.Join(ErrNameRejected, err)
		}
		if errors.Is(err, unix.EEXIST) {
			return errors.Join(ErrNamespaceCollision, fs.ErrExist, err)
		}
		return &os.PathError{Op: "mkdirat", Path: filepath.Join(parent.path, name), Err: err}
	}
	return nil
}

func removeChild(parent *Dir, name string, isDir bool) error {
	if err := ValidComponent(name); err != nil {
		return err
	}
	flags := 0
	if isDir {
		flags = unix.AT_REMOVEDIR
	}
	err := unix.Unlinkat(int(parent.handle.Fd()), name, flags)
	runtime.KeepAlive(parent.handle)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(parent.path, name), Err: err}
	}
	return nil
}

func readDirNames(dir *Dir) ([]string, error) {
	fd, err := unix.Openat(int(dir.handle.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(dir.handle)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: dir.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), dir.path)
	defer f.Close()
	return f.Readdirnames(-1)
}

func syncDir(f *os.File) error {
	err := f.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

func acquireDirLock(root *Dir) (*DirLock, error) {
	if root == nil || root.handle == nil {
		return nil, fs.ErrClosed
	}
	// Open a fresh file description pinned to root's directory inode so
	// concurrent callers in the same process also serialize under flock(2),
	// while never creating or unlinking a lock file inside .windshare-tmp.
	fd, err := unix.Openat(int(root.handle.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(root.handle)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: root.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), root.path)
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX)
		runtime.KeepAlive(f)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		break
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &DirLock{
		release: func() error {
			unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
			runtime.KeepAlive(f)
			return errors.Join(unlockErr, f.Close())
		},
	}, nil
}

func lockFile(f *os.File) (func() error, error) {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	runtime.KeepAlive(f)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(ErrNamespaceLockBusy, err)
		}
		return nil, err
	}
	return func() error {
		unlockErr := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		runtime.KeepAlive(f)
		return unlockErr
	}, nil
}

func noReplaceError(err error) error {
	if errors.Is(err, unix.EEXIST) {
		return errors.Join(ErrNamespaceCollision, fs.ErrExist, err)
	}
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) {
		return errors.Join(ErrUnsupportedOutput, err)
	}
	return err
}

func renameFileNoReplace(srcDir *Dir, srcName string, dstDir *Dir, dstName string, replace bool) error {
	if err := validName(srcName); err != nil {
		return err
	}
	if err := validName(dstName); err != nil {
		return err
	}
	var err error
	if replace {
		err = unix.Renameat(int(srcDir.handle.Fd()), srcName, int(dstDir.handle.Fd()), dstName)
	} else {
		err = renameNoReplace(int(srcDir.handle.Fd()), srcName, int(dstDir.handle.Fd()), dstName)
	}
	runtime.KeepAlive(srcDir.handle)
	runtime.KeepAlive(dstDir.handle)
	return err
}

func directoryEntryMatchesSpelling(parent *Dir, name string, observed os.FileInfo) (exact bool, resultErr error) {
	fd, err := unix.Openat(int(parent.handle.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	runtime.KeepAlive(parent.handle)
	if err != nil {
		return false, err
	}
	handle := os.NewFile(uintptr(fd), parent.path)
	defer func() { resultErr = errors.Join(resultErr, handle.Close()) }()
	for {
		entries, readErr := handle.ReadDir(nativeSpellingBatchSize)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, readErr
		}
		for _, entry := range entries {
			if !exactEntrySpelling(name, entry.Name()) {
				continue
			}
			candidate, statErr := entry.Info()
			if statErr != nil {
				return false, statErr
			}
			if !os.SameFile(observed, candidate) {
				return false, ErrUnsafeNamespace
			}
			return true, nil
		}
		if errors.Is(readErr, io.EOF) {
			return false, nil
		}
	}
}

func classifyPlatformError(err error) (NativeErrorClass, bool) {
	switch {
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return NativeErrorAccessDenied, true
	case errors.Is(err, unix.ENOENT):
		return NativeErrorNotFound, true
	case errors.Is(err, unix.EBADF):
		return NativeErrorInvalidHandle, true
	case errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS):
		return NativeErrorUnsupported, true
	case errors.Is(err, unix.EIO):
		return NativeErrorIO, true
	default:
		return 0, false
	}
}
