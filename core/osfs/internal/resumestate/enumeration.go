package resumestate

import (
	"errors"
	"fmt"
)

const (
	MaxFileStateShardDirectories           = 1 << (ShardHexCharacters * 4)
	MaxUpdateTemporariesPerSession         = MaxFilesPerSession
	MaxFileStateAuxiliaryEntriesPerSession = MaxUpdateTemporariesPerSession
	MaxFileStateEntriesPerSession          = MaxFilesPerSession + MaxFileStateAuxiliaryEntriesPerSession
)

var ErrFileStateNamespaceLimit = errors.New("osfs resumestate file-state namespace exceeds its bound")

// FileStateNamespaceBudget counts names before their contents grant any
// authority. Malformed and opaque entries consume the same finite budget as
// update temporaries, so an attacker cannot bypass the session bound by
// choosing a spelling that recovery will retain rather than remove.
type FileStateNamespaceBudget struct {
	selectedFiles uint32
	shards        uint32
	records       uint32
	auxiliary     uint32
	initialized   bool
}

func NewFileStateNamespaceBudget(selectedFileCount uint32) (FileStateNamespaceBudget, error) {
	if uint64(selectedFileCount) > uint64(MaxFilesPerSession) {
		return FileStateNamespaceBudget{}, fmt.Errorf("%w: selected file count", ErrInvalidState)
	}
	return FileStateNamespaceBudget{selectedFiles: selectedFileCount, initialized: true}, nil
}

// ObserveShard counts every direct child of the files directory, including an
// entry whose name or type later proves invalid. The two-hex namespace has only
// 256 canonical buckets; extra names must not create unbounded scan work.
func (budget *FileStateNamespaceBudget) ObserveShard() error {
	if budget == nil || !budget.initialized {
		return fmt.Errorf("%w: file-state namespace budget", ErrInvalidState)
	}
	if budget.shards >= MaxFileStateShardDirectories {
		return ErrFileStateNamespaceLimit
	}
	budget.shards++
	return nil
}

// ObserveEntry counts a classified shard child before decoding a record or
// reconciling a temporary. A valid crash cut needs at most one authoritative
// record and one in-flight temporary per selected file. Invalid names consume
// the temporary half of that budget instead of receiving unbounded slack.
func (budget *FileStateNamespaceBudget) ObserveEntry(classified ClassifiedFileShardEntry) error {
	if budget == nil || !budget.initialized {
		return fmt.Errorf("%w: file-state namespace budget", ErrInvalidState)
	}
	switch classified.classification {
	case FileShardEntryRecord:
		if budget.records >= budget.selectedFiles {
			return ErrFileStateNamespaceLimit
		}
		budget.records++
	case FileShardEntryUpdateTemporary, FileShardEntryMalformedForLocator, FileShardEntryOpaque:
		if budget.auxiliary >= budget.selectedFiles {
			return ErrFileStateNamespaceLimit
		}
		budget.auxiliary++
	default:
		return fmt.Errorf("%w: file shard entry classification", ErrInvalidState)
	}
	return nil
}

func (budget FileStateNamespaceBudget) SelectedFiles() uint32    { return budget.selectedFiles }
func (budget FileStateNamespaceBudget) Shards() uint32           { return budget.shards }
func (budget FileStateNamespaceBudget) Records() uint32          { return budget.records }
func (budget FileStateNamespaceBudget) AuxiliaryEntries() uint32 { return budget.auxiliary }
func (budget FileStateNamespaceBudget) TotalEntries() uint32 {
	return budget.records + budget.auxiliary
}
