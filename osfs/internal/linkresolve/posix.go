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
	ErrSymlinkLoop    = errors.New("linkresolve: symbolic link loop")
	ErrNotDirectory   = errors.New("linkresolve: target is not a directory")
	ErrTargetMismatch = errors.New("linkresolve: resolved target does not match kernel object")
)

// POSIXOps injects filesystem lookup seams for deterministic unit testing.
// Zero fields fall back to standard os / filepath operations.
type POSIXOps struct {
	Follow       func(string) (fs.FileInfo, error)
	EvalSymlinks func(string) (string, error)
	Lstat        func(string) (fs.FileInfo, error)
}

func (ops POSIXOps) follow(path string) (fs.FileInfo, error) {
	if ops.Follow != nil {
		return ops.Follow(path)
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

// ResolvePOSIXTarget resolves a caller-supplied path through symbolic links
// only when kernel stat(path) (honoring Linux fs.protected_symlinks without
// opening FIFOs), filepath.EvalSymlinks(path), and no-follow lstat(resolved)
// all agree on the exact same non-symlink object.
func ResolvePOSIXTarget(path string, ops POSIXOps) (string, fs.FileInfo, error) {
	return resolvePOSIX(path, false, ops)
}

// ResolvePOSIXDirectory resolves a caller-supplied directory path through
// symbolic links under the same kernel-agreement guarantees as
// ResolvePOSIXTarget, rejecting non-directory targets with ErrNotDirectory.
func ResolvePOSIXDirectory(path string, ops POSIXOps) (string, fs.FileInfo, error) {
	return resolvePOSIX(path, true, ops)
}

func resolvePOSIX(path string, requireDir bool, ops POSIXOps) (string, fs.FileInfo, error) {
	followed, err := ops.follow(path)
	if IsSymlinkLoop(err) {
		return "", nil, fmt.Errorf("%w: %w", ErrSymlinkLoop, err)
	}
	if err != nil {
		return "", nil, err
	}
	if requireDir && !followed.IsDir() {
		return "", nil, ErrNotDirectory
	}
	resolved, err := ops.evalSymlinks(path)
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
	if target.Mode()&fs.ModeSymlink != 0 ||
		target.Mode().Type() != followed.Mode().Type() ||
		!os.SameFile(followed, target) {
		return "", nil, ErrTargetMismatch
	}
	return resolved, target, nil
}
