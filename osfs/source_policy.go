package osfs

import (
	"errors"

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

type selectedLinkDisposition uint8

const (
	selectedLinkResolved selectedLinkDisposition = iota + 1
	selectedLinkRejected
)

// selectedLinkResolution records the explicit outcome of resolving a caller-
// selected top-level symbolic link or Windows directory junction once through
// the kernel before pinning its canonical target with os.Root.
type selectedLinkResolution struct {
	disposition selectedLinkDisposition
	openPath    string
	identity    sourceObjectIdentity
	kind        sourceEntryKind
	audit       sourceEntryAudit
}
