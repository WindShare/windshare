package liveshare

import (
	"errors"

	"github.com/windshare/windshare/core/catalog"
)

// SelectedRootKind is deliberately narrower than catalog.NodeKind: a prepared
// sender can expose only a file or directory as a user-selected root.
type SelectedRootKind uint8

const (
	SelectedRootKindUnknown SelectedRootKind = iota
	SelectedRootKindDirectory
	SelectedRootKindFile
)

// SelectedRootDisplay is a display-only value. Its name is intentionally
// available to human output and must not be copied into diagnostic traces.
type SelectedRootDisplay struct {
	name     string
	kind     SelectedRootKind
	fileSize uint64
}

func (root SelectedRootDisplay) Name() string           { return root.name }
func (root SelectedRootDisplay) Kind() SelectedRootKind { return root.kind }

// FileSize distinguishes a zero-byte file from a directory. Directory sizes
// would imply descendant discovery, which sender preparation never performs.
func (root SelectedRootDisplay) FileSize() (uint64, bool) {
	return root.fileSize, root.kind == SelectedRootKindFile
}

// SelectedRootSummary is an immutable projection of the root records already
// opened by osfs. Multiple selections expose only their count so constructing
// the summary cannot accidentally turn into per-root or descendant work.
type SelectedRootSummary struct {
	selectedCount uint64
	single        SelectedRootDisplay
}

func (summary SelectedRootSummary) SelectedCount() uint64 { return summary.selectedCount }

func (summary SelectedRootSummary) SingleRoot() (SelectedRootDisplay, bool) {
	return summary.single, summary.selectedCount == 1
}

func newSelectedRootSummary(selected []catalog.NodeRecord) (SelectedRootSummary, error) {
	count := uint64(len(selected))
	if count == 0 {
		return SelectedRootSummary{}, errors.New("live share selected-root summary requires a selected root")
	}
	summary := SelectedRootSummary{selectedCount: count}
	if count != 1 {
		return summary, nil
	}

	entry := selected[0].Entry()
	switch selected[0].Kind() {
	case catalog.NodeKindDirectory:
		summary.single = SelectedRootDisplay{
			name: entry.Name(),
			kind: SelectedRootKindDirectory,
		}
	case catalog.NodeKindFile:
		summary.single = SelectedRootDisplay{
			name:     entry.Name(),
			kind:     SelectedRootKindFile,
			fileSize: entry.ExpectedSize(),
		}
	default:
		return SelectedRootSummary{}, errors.New("live share selected root has an unsupported kind")
	}
	if summary.single.name == "" {
		return SelectedRootSummary{}, errors.New("live share selected root has no display name")
	}
	return summary, nil
}
