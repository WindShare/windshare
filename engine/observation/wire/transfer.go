package wire

import appobs "github.com/windshare/windshare/engine/observation"

func (enc *encoder) encodeTransferLifecycleObserved(event appobs.TransferLifecycleObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	selection, err := nameOf(event.FileSelection())
	if err != nil {
		return err
	}
	fileSettlement, err := nameOf(event.FileSettlement())
	if err != nil {
		return err
	}
	treeSettlement, err := nameOf(event.TreeSettlement())
	if err != nil {
		return err
	}
	progress, err := EncodeProgress(event.Progress())
	if err != nil {
		return err
	}
	correlation, err := transferLifecycleCorrelation(event)
	if err != nil {
		return err
	}
	payload := TransferLifecyclePayload{
		ReceiveOperationID: EncodeIdentity(event.ReceiveOperationID().Bytes()),
		TransferJobID:      EncodeIdentity(event.TransferJobID().Bytes()),
		Stage:              stage,
		FileSelection:      selection,
		FileSettlement:     fileSettlement,
		TreeSettlement:     treeSettlement,
		Progress:           progress,
	}
	if capacity, ok := event.CapacityLifecycle(); ok {
		payload.Capacity = &TransferCapacityLifecycle{
			WaitID:              EncodeIdentity(capacity.WaitID().Bytes()),
			GenerationID:        EncodeIdentity(capacity.GenerationID().Bytes()),
			ProtocolOperationID: EncodeIdentity(capacity.OperationID().Bytes()),
			Attempt:             decimal(capacity.Attempt()), HintMS: signedDecimal(capacity.Hint().Milliseconds()),
			JitterMS:          signedDecimal(capacity.Jitter().Milliseconds()),
			DelayMS:           signedDecimal(capacity.Delay().Milliseconds()),
			AccumulatedWaitMS: signedDecimal(capacity.AccumulatedWait().Milliseconds()),
			ActiveWaiters:     capacity.ActiveWaiters(),
		}
	}
	if reason, ok := event.ItemBlock(); ok {
		projected, projectErr := namedPointer(reason)
		if projectErr != nil {
			return projectErr
		}
		payload.ItemBlockReason = projected
	}
	if failure, ok := event.Failure(); ok {
		projected, projectErr := EncodeFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	enc.set("transfer_lifecycle", correlation, payload)
	return nil
}

func transferLifecycleCorrelation(event appobs.TransferLifecycleObserved) (*CorrelationV1, error) {
	if capacity, ok := event.CapacityLifecycle(); ok {
		return projectProtocolCorrelation(
			event.ProtocolSessionID(), capacity.OperationID(), appobs.LaneIdentity{}, false,
		)
	}
	return projectSessionCorrelation(event.ProtocolSessionID(), appobs.LaneIdentity{}, false)
}
