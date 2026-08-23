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
	progress, err := ProjectProgress(value.Progress, false)
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
	if err := projectTransferCapacityLifecycle(value, &spec); err != nil {
		return clievent.TransferLifecycleObserved{}, err
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

func projectTransferCapacityLifecycle(
	value transfer.TransferLifecycleTrace,
	spec *clievent.TransferLifecycleSpec,
) error {
	capacityStage := value.Stage >= transfer.TransferCapacityRetryScheduled &&
		value.Stage <= transfer.TransferCapacityGenerationEnded
	if !capacityStage {
		if !value.CapacityWaitID.IsZero() || !value.CapacityGeneration.IsZero() ||
			!value.CapacityOperationID.IsZero() || value.CapacityAttempt != 0 ||
			value.CapacityHint != 0 || value.CapacityJitter != 0 || value.CapacityDelay != 0 ||
			value.CapacityAccumulated != 0 || value.CapacityActiveWaiters != 0 {
			return ErrInvalidProjection
		}
		return nil
	}
	waitID, err := clievent.NewCapacityWaitID(value.CapacityWaitID.Bytes())
	if err != nil {
		return ErrInvalidProjection
	}
	generation, err := clievent.NewCapacityGenerationID(value.CapacityGeneration.Bytes())
	if err != nil {
		return ErrInvalidProjection
	}
	operation, err := ProtocolOperationID(value.CapacityOperationID)
	if err != nil {
		return ErrInvalidProjection
	}
	spec.CapacityWait = waitID
	spec.CapacityGeneration = generation
	spec.CapacityOperation = operation
	spec.CapacityAttempt = value.CapacityAttempt
	spec.CapacityHint = value.CapacityHint
	spec.CapacityJitter = value.CapacityJitter
	spec.CapacityDelay = value.CapacityDelay
	spec.CapacityAccumulatedWait = value.CapacityAccumulated
	spec.CapacityActiveWaiters = value.CapacityActiveWaiters
	return nil
}
