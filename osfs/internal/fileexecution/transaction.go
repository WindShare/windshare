package fileexecution

import (
	"sync"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
)

type transactionState uint8

const (
	transactionOpen transactionState = iota + 1
	transactionClosed
)

// PartialFileTransaction owns one restart-durable native file lifecycle.
type PartialFileTransaction struct {
	engine          *Engine
	materialization transfer.MaterializationFile
	destination     FileDestination
	file            OwnedFile
	binding         transfer.MaterializedFileBinding

	mu                 sync.Mutex
	record             checkpointmodel.Record
	pending            content.RangeSet
	state              transactionState
	resumed            bool
	warnings           []MetadataWarning
	acceptedGeneration transfer.CheckpointGeneration
	lastCheckpoint     time.Time
	flushTimer         CheckpointTimer
	flushFailure       error
	receiptCleanupErr  error
}

var _ transfer.FileTransaction = (*PartialFileTransaction)(nil)

type MetadataWarningKind uint8

const MetadataModifiedTimeWarning MetadataWarningKind = iota + 1

type MetadataWarning struct{ kind MetadataWarningKind }

func (warning MetadataWarning) Kind() MetadataWarningKind { return warning.kind }

func settlementMetadataWarnings(warnings []MetadataWarning) []transfer.FileMetadataWarning {
	result := make([]transfer.FileMetadataWarning, 0, len(warnings))
	for _, warning := range warnings {
		if warning.kind == MetadataModifiedTimeWarning {
			result = append(result, transfer.FileMetadataModifiedTime)
		}
	}
	return result
}
