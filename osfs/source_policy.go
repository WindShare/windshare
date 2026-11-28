package osfs

import (
	"errors"
	"os"

	"github.com/windshare/windshare/core/catalog"
)

var errSourceCloudPlaceholder = errors.New("osfs: source item is not fully available on this device")

// openedSourceHandle captures the classified kind, audit metadata, object
// identity, size, and modification time from one already-open handle so callers
// do not repeat redundant stat/handle syscalls.
type openedSourceHandle struct {
	identity sourceObjectIdentity
	kind     sourceEntryKind
	audit    sourceEntryAudit
	size     uint64
	modified catalog.ModifiedTime
}

type sourceEntryKind uint8

const (
	sourceEntryDirectory sourceEntryKind = iota + 1
	sourceEntryRegularFile
	sourceEntrySymbolicLink
	sourceEntryReparsePoint
	sourceEntryCloudPlaceholder
	sourceEntrySpecialFile
)

// sourceEntryAudit carries platform metadata for structured diagnostic logs on
// cloud entries and rejected reparse points. Ordinary local files keep the zero value.
type sourceEntryAudit struct {
	attributes       uint32
	reparseTag       uint32
	placeholderState uint32
	hydratedCloud    bool
	hasCloudState    bool
}

func (a sourceEntryAudit) present() bool {
	return a.hydratedCloud || a.reparseTag != 0 || a.attributes != 0
}

func omissionForEntryKind(kind sourceEntryKind) catalog.SourceOmissionReason {
	switch kind {
	case sourceEntryDirectory, sourceEntryRegularFile:
		return catalog.SourceOmissionNone
	case sourceEntrySymbolicLink:
		return catalog.SourceOmissionSymbolicLink
	case sourceEntryReparsePoint:
		return catalog.SourceOmissionReparsePoint
	case sourceEntryCloudPlaceholder:
		return catalog.SourceOmissionCloudPlaceholder
	default:
		return catalog.SourceOmissionSpecialFile
	}
}

type linkedDirectoryDisposition uint8

const (
	linkedDirectoryResolved linkedDirectoryDisposition = iota + 1
	linkedDirectoryRejected
)

// linkedDirectoryResolution makes the outcome of linked working-directory
// resolution explicit instead of using an empty openPath as an implicit sentinel.
type linkedDirectoryResolution struct {
	disposition linkedDirectoryDisposition
	openPath    string
	identity    sourceObjectIdentity
	kind        sourceEntryKind
	audit       sourceEntryAudit
}

// classifyPureDotTraversal reports whether selectedPath consists solely of "."
// and ".." relative components separated by path separators, and whether any
// component is "..". Only pure dot spellings ask for the working directory or
// its ancestors without naming a link component; named links (including
// "link/." or glob-expanded "dir/*/.") stay outside this allowance.
func classifyPureDotTraversal(selectedPath string) (pure bool, hasParent bool) {
	if selectedPath == "" || os.IsPathSeparator(selectedPath[0]) {
		return false, false
	}
	start := 0
	for index := 0; index <= len(selectedPath); index++ {
		if index < len(selectedPath) && !os.IsPathSeparator(selectedPath[index]) {
			continue
		}
		component := selectedPath[start:index]
		start = index + 1
		if component == "" {
			continue
		}
		switch component {
		case ".":
			pure = true
		case "..":
			pure = true
			hasParent = true
		default:
			return false, false
		}
	}
	return pure, hasParent
}
