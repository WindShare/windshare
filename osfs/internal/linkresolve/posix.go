//go:build !windows

package linkresolve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	ErrSymlinkLoop       = errors.New("linkresolve: symbolic link loop")
	ErrNotDirectory      = errors.New("linkresolve: target is not a directory")
	ErrDirectoryMismatch = errors.New("linkresolve: resolved target does not match kernel directory")
)

// POSIXOps injects filesystem lookup seams for deterministic unit testing.
// Zero fields fall back to standard os / filepath operations.
type POSIXOps struct {
	FollowSelected func(string) (fs.FileInfo, error)
	StatLogical    func(string) (fs.FileInfo, error)
	EvalSymlinks   func(string) (string, error)
	Lstat          func(string) (fs.FileInfo, error)
}

func (ops POSIXOps) followSelected(path string) (fs.FileInfo, error) {
	if ops.FollowSelected != nil {
		return ops.FollowSelected(path)
	}
	return os.Stat(path)
}

func (ops POSIXOps) statLogical(path string) (fs.FileInfo, error) {
	if ops.StatLogical != nil {
		return ops.StatLogical(path)
	}
	return os.Stat(path)
}

func (ops POSIXOps) evalSymlinks(path string) (string, error) {
	if ops.EvalSymlinks != nil {
		return ops.EvalSymlinks(path)
	}
	return filepath.EvalSymlinks(path)
}

func (ops POSIXOps) lstat(path string) (fs.FileInfo, error) {
	if ops.Lstat != nil {
		return ops.Lstat(path)
	}
	return os.Lstat(path)
}

// IsSymlinkLoop reports whether err indicates a symbolic link cycle from either
// the kernel (ELOOP) or Go's filepath.EvalSymlinks ("too many links").
func IsSymlinkLoop(err error) bool {
	return errors.Is(err, syscall.ELOOP) || (err != nil && strings.Contains(err.Error(), "too many links"))
}

// ResolvePOSIXDirectory resolves a caller-supplied directory path through a
// final symbolic link only when kernel stat(selectedPath) (honoring Linux
// fs.protected_symlinks and physical ".." traversal), kernel stat(logicalPath),
// filepath.EvalSymlinks(logicalPath), and no-follow lstat(resolved) all agree
// on the exact same real directory.
func ResolvePOSIXDirectory(selectedPath, logicalPath string, ops POSIXOps) (string, fs.FileInfo, error) {
	followed, err := ops.followSelected(selectedPath)
	if IsSymlinkLoop(err) {
		return "", nil, fmt.Errorf("%w: %w", ErrSymlinkLoop, err)
	}
	if err != nil {
		return "", nil, err
	}
	if !followed.IsDir() {
		return "", nil, ErrNotDirectory
	}
	if selectedPath != logicalPath || ops.StatLogical != nil {
		logicalTarget, statErr := ops.statLogical(logicalPath)
		if IsSymlinkLoop(statErr) {
			return "", nil, fmt.Errorf("%w: %w", ErrSymlinkLoop, statErr)
		}
		if statErr != nil {
			return "", nil, statErr
		}
		if !logicalTarget.IsDir() || !os.SameFile(followed, logicalTarget) {
			return "", nil, ErrDirectoryMismatch
		}
	}
	resolved, err := ops.evalSymlinks(logicalPath)
	if IsSymlinkLoop(err) {
		return "", nil, fmt.Errorf("%w: %w", ErrSymlinkLoop, err)
	}
	if err != nil {
		return "", nil, err
	}
	target, err := ops.lstat(resolved)
	if err != nil {
		return "", nil, err
	}
	if !target.IsDir() || target.Mode()&fs.ModeSymlink != 0 || !os.SameFile(followed, target) {
		return "", nil, ErrDirectoryMismatch
	}
	return resolved, followed, nil
}

// VerifyPOSIXDirectoryAgreement confirms that kernel stat(selectedPath) opens
// the exact same directory as expected (for example when lexical ".." from $PWD
// may diverge from physical ".." inside a symlinked working directory).
func VerifyPOSIXDirectoryAgreement(
	selectedPath string,
	expected fs.FileInfo,
	follow func(string) (fs.FileInfo, error),
) error {
	if follow == nil {
		follow = os.Stat
	}
	followed, err := follow(selectedPath)
	if IsSymlinkLoop(err) {
		return fmt.Errorf("%w: %w", ErrSymlinkLoop, err)
	}
	if err != nil {
		return err
	}
	if !followed.IsDir() || !os.SameFile(followed, expected) {
		return ErrDirectoryMismatch
	}
	return nil
}
