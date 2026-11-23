package osfs

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

var errSourceCloudPlaceholder = errors.New("osfs: source item is not fully available on this device")

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
