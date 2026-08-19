package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/transfer"
)

func ProjectTransferLifecycle(value transfer.TransferLifecycleTrace) (clievent.TransferLifecycleObserved, error) {
	if value.Interruption != 0 && !value.Interruption.Valid() {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	receiveID, err := ReceiveOperationID(value.ReceiveOperationID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	sessionID, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	jobID, err := TransferJobID(value.TransferJobID)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	stage, ok := projectTransferStage(value.Stage)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	progress, err := ProjectProgress(value.Progress)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	selection, ok := projectFileSelection(value.FileSelection)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	fileSettlement, ok := projectFileSettlement(value.FileSettlement)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	itemBlockReason, ok := projectItemBlockReason(value.ItemBlockReason)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	treeSettlement, ok := projectTreeSettlement(value.DirectTreeSettlement)
	if !ok {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	spec := clievent.TransferLifecycleSpec{
		ReceiveOperation: receiveID, ProtocolSession: sessionID, TransferJob: jobID,
		Stage: stage, Progress: progress, FileSelection: selection,
		FileSettlement: fileSettlement, ItemBlockReason: itemBlockReason,
		TreeSettlement: treeSettlement,
	}
	if value.Failed {
		if spec.Failure, ok = ProjectFault(value.Fault); !ok {
			if spec.Failure, ok = ProjectTransferInterruption(value.Interruption); !ok {
				spec.Failure = mustFailure(clievent.FailureUnexpected)
			}
		}
	} else if value.Fault.Valid() || value.Interruption != 0 {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewTransferLifecycleObserved(spec)
	if err != nil {
		return clievent.TransferLifecycleObserved{}, ErrInvalidProjection
	}
	return event, nil
}
