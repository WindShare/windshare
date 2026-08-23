package clievent

import "time"

type TransferLifecycleSpec struct {
	ReceiveOperation        ReceiveOperationID
	ProtocolSession         ProtocolSessionID
	TransferJob             TransferJobID
	Stage                   TransferLifecycleStage
	Progress                ProgressSnapshot
	FileSelection           FileSelectionDecision
	FileSettlement          FileSettlement
	ItemBlockReason         ItemBlockReason
	TreeSettlement          TreeSettlement
	CapacityWait            CapacityWaitID
	CapacityGeneration      CapacityGenerationID
	CapacityOperation       ProtocolOperationID
	CapacityAttempt         uint64
	CapacityHint            time.Duration
	CapacityJitter          time.Duration
	CapacityDelay           time.Duration
	CapacityAccumulatedWait time.Duration
	CapacityActiveWaiters   uint32
	Failure                 Failure
}

type TransferLifecycleObserved struct{ spec TransferLifecycleSpec }

func NewTransferLifecycleObserved(spec TransferLifecycleSpec) (TransferLifecycleObserved, error) {
	if !validTransferLifecycleSpec(spec) {
		return TransferLifecycleObserved{}, ErrInvalidEvent
	}
	return TransferLifecycleObserved{spec: spec}, nil
}

func validTransferLifecycleSpec(spec TransferLifecycleSpec) bool {
	_, stageOK := spec.Stage.Name()
	_, selectionOK := spec.FileSelection.Name()
	_, fileSettlementOK := spec.FileSettlement.Name()
	_, itemBlockReasonOK := spec.ItemBlockReason.Name()
	_, treeSettlementOK := spec.TreeSettlement.Name()
	return spec.ReceiveOperation.Valid() && spec.ProtocolSession.Valid() && spec.TransferJob.Valid() &&
		stageOK && spec.Progress.Valid() && selectionOK && fileSettlementOK && itemBlockReasonOK &&
		treeSettlementOK && (spec.ItemBlockReason == ItemBlockNone || spec.FileSettlement == FileItemBlocked) &&
		validTransferCapacityLifecycle(spec)
}

func validTransferCapacityLifecycle(spec TransferLifecycleSpec) bool {
	capacityStage := spec.Stage >= TransferCapacityRetryScheduled && spec.Stage <= TransferCapacityGenerationEnded
	if !capacityStage {
		return !spec.CapacityWait.Valid() && !spec.CapacityGeneration.Valid() && !spec.CapacityOperation.Valid() &&
			spec.CapacityAttempt == 0 && spec.CapacityHint == 0 && spec.CapacityJitter == 0 &&
			spec.CapacityDelay == 0 && spec.CapacityAccumulatedWait == 0 && spec.CapacityActiveWaiters == 0
	}
	if !spec.CapacityWait.Valid() || !spec.CapacityGeneration.Valid() || !spec.CapacityOperation.Valid() ||
		spec.CapacityAttempt == 0 || spec.CapacityHint <= 0 || spec.CapacityJitter < 0 ||
		spec.CapacityDelay < 0 || spec.CapacityAccumulatedWait < 0 {
		return false
	}
	terminal := spec.Stage >= TransferCapacityBudgetPaused
	if terminal != spec.Failure.Valid() {
		return false
	}
	if spec.Stage == TransferCapacityRetryScheduled {
		return spec.CapacityDelay > 0
	}
	return spec.CapacityJitter == 0 && spec.CapacityDelay == 0
}

type TransferCapacityLifecycle struct {
	wait            CapacityWaitID
	generation      CapacityGenerationID
	operation       ProtocolOperationID
	attempt         uint64
	hint            time.Duration
	jitter          time.Duration
	delay           time.Duration
	accumulatedWait time.Duration
	activeWaiters   uint32
}

func (value TransferCapacityLifecycle) WaitID() CapacityWaitID             { return value.wait }
func (value TransferCapacityLifecycle) GenerationID() CapacityGenerationID { return value.generation }
func (value TransferCapacityLifecycle) OperationID() ProtocolOperationID   { return value.operation }
func (value TransferCapacityLifecycle) Attempt() uint64                    { return value.attempt }
func (value TransferCapacityLifecycle) Hint() time.Duration                { return value.hint }
func (value TransferCapacityLifecycle) Jitter() time.Duration              { return value.jitter }
func (value TransferCapacityLifecycle) Delay() time.Duration               { return value.delay }
func (value TransferCapacityLifecycle) AccumulatedWait() time.Duration     { return value.accumulatedWait }
func (value TransferCapacityLifecycle) ActiveWaiters() uint32              { return value.activeWaiters }

func (TransferLifecycleObserved) event()           {}
func (TransferLifecycleObserved) Command() Command { return CommandGet }
func (TransferLifecycleObserved) Level() Level     { return LevelDebug }
func (value TransferLifecycleObserved) ReceiveOperationID() ReceiveOperationID {
	return value.spec.ReceiveOperation
}
func (value TransferLifecycleObserved) ProtocolSessionID() ProtocolSessionID {
	return value.spec.ProtocolSession
}
func (value TransferLifecycleObserved) TransferJobID() TransferJobID  { return value.spec.TransferJob }
func (value TransferLifecycleObserved) Stage() TransferLifecycleStage { return value.spec.Stage }
func (value TransferLifecycleObserved) Progress() ProgressSnapshot    { return value.spec.Progress }
func (value TransferLifecycleObserved) FileSelection() FileSelectionDecision {
	return value.spec.FileSelection
}
func (value TransferLifecycleObserved) FileSettlement() FileSettlement {
	return value.spec.FileSettlement
}
func (value TransferLifecycleObserved) ItemBlock() (ItemBlockReason, bool) {
	return value.spec.ItemBlockReason, value.spec.ItemBlockReason != ItemBlockNone
}
func (value TransferLifecycleObserved) TreeSettlement() TreeSettlement {
	return value.spec.TreeSettlement
}
func (value TransferLifecycleObserved) CapacityLifecycle() (TransferCapacityLifecycle, bool) {
	if value.spec.Stage < TransferCapacityRetryScheduled || value.spec.Stage > TransferCapacityGenerationEnded {
		return TransferCapacityLifecycle{}, false
	}
	return TransferCapacityLifecycle{
		wait: value.spec.CapacityWait, generation: value.spec.CapacityGeneration,
		operation: value.spec.CapacityOperation, attempt: value.spec.CapacityAttempt,
		hint: value.spec.CapacityHint, jitter: value.spec.CapacityJitter, delay: value.spec.CapacityDelay,
		accumulatedWait: value.spec.CapacityAccumulatedWait, activeWaiters: value.spec.CapacityActiveWaiters,
	}, true
}
func (value TransferLifecycleObserved) Failure() (Failure, bool) {
	return value.spec.Failure, value.spec.Failure.Valid()
}
func (value TransferLifecycleObserved) Accept(visitor Visitor) error {
	return acceptTransferLifecycleObserved(visitor, value)
}
