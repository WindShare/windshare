//go:build !windows

package osfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/osfs/internal/linkresolve"
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

func resolveLinkedWorkingDirectory(selectedPath, absolute string) (linkedDirectoryResolution, error) {
	return resolveLinkedWorkingDirectoryWith(selectedPath, absolute, linkresolve.POSIXOps{})
}

func resolveLinkedWorkingDirectoryWith(
	selectedPath string,
	absolute string,
	ops linkresolve.POSIXOps,
) (linkedDirectoryResolution, error) {
	resolved, followed, err := linkresolve.ResolvePOSIXDirectory(selectedPath, absolute, ops)
	if err != nil {
		return linkedDirectoryResolution{}, classifyPOSIXLinkResolveError("resolve selected root link", absolute, err)
	}
	return linkedDirectoryResolution{
		disposition: linkedDirectoryResolved,
		openPath:    resolved,
		identity:    sourceObjectIdentity{info: followed},
		kind:        sourceEntryDirectory,
	}, nil
}

// verifyParentDotTraversal ensures a pure ".." traversal whose logical parent
// is a real directory still agrees with the physical directory the POSIX kernel
// opens, preventing silent divergence when $PWD traverses an intermediate link.
func verifyParentDotTraversal(selectedPath, absolute string, expected sourceObjectIdentity) error {
	return verifyParentDotTraversalWith(selectedPath, absolute, expected, os.Stat)
}

func verifyParentDotTraversalWith(
	selectedPath string,
	absolute string,
	expected sourceObjectIdentity,
	follow func(string) (fs.FileInfo, error),
) error {
	if err := linkresolve.VerifyPOSIXDirectoryAgreement(selectedPath, expected.info, follow); err != nil {
		return classifyPOSIXLinkResolveError("follow selected root", absolute, err)
	}
	return nil
}

func classifyPOSIXLinkResolveError(operation, absolute string, err error) error {
	switch {
	case errors.Is(err, linkresolve.ErrSymlinkLoop), errors.Is(err, linkresolve.ErrNotDirectory):
		return fmt.Errorf("osfs: selected root %q is not a stable file or directory: %w", absolute, content.ErrUnsupportedStability)
	case errors.Is(err, linkresolve.ErrDirectoryMismatch):
		return fmt.Errorf("osfs: selected root %q does not resolve to the directory the system opens: %w", absolute, content.ErrUnsupportedStability)
	default:
		return pathfailure.Filesystem(operation, absolute, err)
	}
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
