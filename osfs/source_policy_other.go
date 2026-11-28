//go:build !windows

package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/osfs/internal/pathfailure"
)

type sourceObjectIdentity struct {
	info fs.FileInfo
}

func (identity sourceObjectIdentity) sameObject(other sourceObjectIdentity) bool {
	if identity.info == nil || other.info == nil {
		return false
	}
	return identity.info.IsDir() == other.info.IsDir() && os.SameFile(identity.info, other.info)
}

type rootInspector struct{}

func newRootInspector([]*os.Root) (rootInspector, error) { return rootInspector{}, nil }
func (rootInspector) Close() error                       { return nil }

func (rootInspector) inspectRelativeRegularFile(
	root *os.Root,
	_ int,
	relative string,
) (sourceObjectIdentity, bool, error) {
	info, err := root.Lstat(relative)
	if err != nil {
		return sourceObjectIdentity{}, false, err
	}
	return sourceObjectIdentity{info: info}, classifyPOSIXFileInfo(info) == sourceEntryRegularFile, nil
}

func classifyPOSIXFileInfo(info fs.FileInfo) sourceEntryKind {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return sourceEntrySymbolicLink
	case info.IsDir():
		return sourceEntryDirectory
	case info.Mode().IsRegular():
		return sourceEntryRegularFile
	default:
		return sourceEntrySpecialFile
	}
}

func openRootObjectNoRecall(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

func inspectSelectedRootKind(absolute string) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	before, err := os.Lstat(absolute)
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, err
	}
	return sourceObjectIdentity{info: before}, classifyPOSIXFileInfo(before), sourceEntryAudit{}, nil
}

func isLinkedDirectoryLeaf(kind sourceEntryKind, _ sourceEntryAudit) bool {
	return kind == sourceEntrySymbolicLink
}

func resolveLinkedWorkingDirectory(selectedPath, absolute string) (resolvedLinkedDirectory, error) {
	return resolveLinkedWorkingDirectoryWith(selectedPath, absolute, os.Stat)
}

func isPOSIXSymlinkLoop(err error) bool {
	return errors.Is(err, syscall.ELOOP) || (err != nil && strings.Contains(err.Error(), "too many links"))
}

// resolveLinkedWorkingDirectoryWith resolves a "." / ".." traversal whose
// logical working directory ends in a symlink. The kernel stat of selectedPath
// honors fs.protected_symlinks and physical ".." traversal, while kernel stat
// of absolute plus EvalSymlinks, Lstat, and SameFile confirm that the logical
// path reaches the exact same real directory before opening os.Root.
func resolveLinkedWorkingDirectoryWith(
	selectedPath string,
	absolute string,
	follow func(string) (fs.FileInfo, error),
) (resolvedLinkedDirectory, error) {
	followed, err := follow(selectedPath)
	if isPOSIXSymlinkLoop(err) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	if err != nil {
		return resolvedLinkedDirectory{}, pathfailure.Filesystem("follow selected root", absolute, err)
	}
	if !followed.IsDir() {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	logicalTarget, err := os.Stat(absolute)
	if isPOSIXSymlinkLoop(err) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	if err != nil {
		return resolvedLinkedDirectory{}, pathfailure.Filesystem("resolve selected root link", absolute, err)
	}
	if !logicalTarget.IsDir() || !os.SameFile(followed, logicalTarget) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q does not resolve to the directory the system opens: %w", absolute, content.ErrUnsupportedStability)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if isPOSIXSymlinkLoop(err) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	}
	if err != nil {
		return resolvedLinkedDirectory{}, pathfailure.Filesystem("resolve selected root link", absolute, err)
	}
	target, err := os.Lstat(resolved)
	if err != nil {
		return resolvedLinkedDirectory{}, pathfailure.Filesystem("inspect selected root link target", absolute, err)
	}
	if !target.IsDir() || target.Mode()&fs.ModeSymlink != 0 || !os.SameFile(followed, target) {
		return resolvedLinkedDirectory{}, fmt.Errorf("osfs: selected root %q does not resolve to the directory the system opens: %w", absolute, content.ErrUnsupportedStability)
	}
	return resolvedLinkedDirectory{
		openPath: resolved,
		identity: sourceObjectIdentity{info: followed},
		kind:     sourceEntryDirectory,
	}, nil
}

func inspectRootChildKind(
	directory *os.Root,
	_ *os.File,
	entry fs.DirEntry,
) (sourceObjectIdentity, sourceEntryKind, sourceEntryAudit, error) {
	before, err := directory.Lstat(entry.Name())
	if err != nil {
		return sourceObjectIdentity{}, 0, sourceEntryAudit{}, err
	}
	return sourceObjectIdentity{info: before}, classifyPOSIXFileInfo(before), sourceEntryAudit{}, nil
}

func inspectOpenedSourceHandle(handle *os.File) (openedSourceHandle, error) {
	if handle == nil {
		return openedSourceHandle{}, os.ErrInvalid
	}
	info, err := handle.Stat()
	if err != nil {
		return openedSourceHandle{}, err
	}
	modified, err := catalogModifiedTime(info)
	if err != nil {
		return openedSourceHandle{}, err
	}
	kind := classifyPOSIXFileInfo(info)
	var size uint64
	if kind == sourceEntryRegularFile && info.Size() > 0 {
		size = uint64(info.Size())
	}
	return openedSourceHandle{
		identity: sourceObjectIdentity{info: info},
		kind:     kind,
		size:     size,
		modified: modified,
	}, nil
}

func classifyRootRelativeOpenError(operation, relative string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return content.WithRevisionComparison(errors.Join(content.ErrRevisionStale, err), content.RevisionComparisonUnavailable)
	}
	return content.WithRevisionComparison(
		pathfailure.Filesystem(operation, relative, err), content.RevisionComparisonUnavailable,
	)
}

func isTransientRevisionOpenError(error) bool { return false }
