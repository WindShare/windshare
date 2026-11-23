//go:build !windows

package osfs

import (
	"io/fs"
	"os"
)

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

func inspectSelectedRootKind(absolute string) (fs.FileInfo, sourceEntryKind, sourceEntryAudit, error) {
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	return before, classifyPOSIXFileInfo(before), sourceEntryAudit{}, nil
}

func inspectRootChildKind(
	directory *os.Root,
	_ *os.File,
	entry fs.DirEntry,
) (fs.FileInfo, sourceEntryKind, sourceEntryAudit, error) {
	before, err := directory.Lstat(entry.Name())
	if err != nil {
		return nil, 0, sourceEntryAudit{}, err
	}
	return before, classifyPOSIXFileInfo(before), sourceEntryAudit{}, nil
}

func inspectHandleKind(_ *os.File, info fs.FileInfo) (sourceEntryKind, sourceEntryAudit, error) {
	return classifyPOSIXFileInfo(info), sourceEntryAudit{}, nil
}

func canOpenRootRelativeRegularFile(info fs.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0
}
