package fileexecution

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type existingFileRecovery struct {
	engine      *Engine
	file        transfer.MaterializationFile
	destination FileDestination
	ownedFile   OwnedFile
	binding     transfer.MaterializedFileBinding
	record      checkpointmodel.Record
}

func (engine *Engine) beginExisting(
	ctx context.Context,
	sequence uint64,
	file transfer.MaterializationFile,
	destination FileDestination,
	record checkpointmodel.Record,
) (transfer.FileStart, error) {
	recovery := existingFileRecovery{
		engine: engine, file: file, destination: destination, record: record,
	}
	binding, err := outputBinding(file.Target, record)
	if err != nil {
		return recovery.failStart(ctx, err)
	}
	recovery.binding = binding
	ownedFile, owned, openErr := engine.platform.OpenOwnedFile(
		ctx, record.OwnedObjectID(), record.ExactSize(), existingRecordWritable(record),
	)
	recovery.ownedFile = ownedFile
	if !ownedFileMatchesObservation(ownedFile, owned, record.OwnedObjectID()) {
		return recovery.failStart(
			ctx, bindingError(ErrInvalidObservation), collaboratorError(ctx, openErr),
		)
	}
	expectation, err := expectationFor(file, record)
	if err != nil {
		return recovery.failStart(ctx, err)
	}
	final, finalErr := destination.ObserveFinal(ctx, expectation)
	if finalErr != nil || !final.valid() {
		return recovery.failStart(ctx, collaboratorError(ctx, finalErr))
	}
	if err := recovery.commitCandidate(ctx, owned, final); err != nil {
		return recovery.failStart(ctx, err)
	}
	decision, err := ReduceRecovery(recovery.record, owned, final)
	if err != nil {
		return recovery.failStart(ctx, err)
	}
	engine.emit(engine.traceEvent(
		sequence, TraceRecoverFile, traceRecoveryOutcome(decision),
		recovery.record.Phase(), recovery.record.Phase(), fault.Fault{},
	))
	return recovery.start(ctx, decision)
}

func existingRecordWritable(record checkpointmodel.Record) bool {
	switch record.Phase() {
	case checkpointmodel.PhaseActive, checkpointmodel.PhasePaused, checkpointmodel.PhasePublishing:
		return true
	default:
		return false
	}
}

func ownedFileMatchesObservation(
	file OwnedFile,
	observation OwnedObservation,
	object checkpointmodel.ObjectID,
) bool {
	if !observation.validFor(object) {
		return false
	}
	if observation.Condition() != OwnedReady {
		return file == nil
	}
	return file != nil && file.ObjectID() == object
}

func (recovery *existingFileRecovery) commitCandidate(
	ctx context.Context,
	owned OwnedObservation,
	final FinalObservation,
) error {
	if recovery.record.CommitState() != checkpointmodel.CommitCandidate {
		return nil
	}
	if owned.Condition() != OwnedReady || final.Condition() != FinalAbsent || recovery.ownedFile == nil {
		return bindingError(ErrTargetOwnershipUnknown)
	}
	if err := recovery.ownedFile.Sync(); err != nil {
		return collaboratorError(ctx, err)
	}
	verified, err := checkpointmodel.Promote(
		recovery.record, recovery.record.Phase(), checkpointmodel.CommitVerified,
	)
	if err != nil {
		return err
	}
	if _, err := recovery.engine.storeRecord(ctx, &recovery.record, verified); err != nil {
		return err
	}
	recovery.record = verified
	return nil
}

func (recovery *existingFileRecovery) start(
	ctx context.Context,
	decision RecoveryDecision,
) (transfer.FileStart, error) {
	switch decision.Action() {
	case RecoveryOpenActive:
		return recovery.engine.transactionStart(
			recovery.file, recovery.destination, recovery.ownedFile, recovery.record,
		)
	case RecoveryActivate:
		return recovery.activate(ctx)
	case RecoveryRetryPublication:
		return recovery.recoverPublication(ctx, true)
	case RecoveryCompletePublication:
		return recovery.recoverPublication(ctx, false)
	case RecoveryPublishBlocked:
		settlement, err := verifiedSettlement(
			transfer.FilePublishBlocked, recovery.binding, recovery.record,
		)
		return recovery.engine.terminalStart(
			ctx, recovery.destination, recovery.ownedFile, settlement, err,
		)
	case RecoveryReturnPublished:
		settlement, err := verifiedSettlement(
			transfer.FilePublished, recovery.binding, recovery.record,
		)
		return recovery.engine.terminalStart(
			ctx, recovery.destination, recovery.ownedFile, settlement, err,
		)
	case RecoveryReturnRetired:
		settlement, err := transfer.NewRetiredFileSettlement(recovery.binding)
		return recovery.engine.terminalStart(
			ctx, recovery.destination, recovery.ownedFile, settlement, err,
		)
	case RecoveryReturnQuarantined:
		settlement, err := quarantinedSettlement(recovery.binding, recovery.record)
		return recovery.engine.terminalStart(
			ctx, recovery.destination, recovery.ownedFile, settlement, err,
		)
	case RecoveryInstallQuarantine:
		return recovery.installQuarantine(ctx, decision.QuarantineReason())
	case RecoveryNeedsAttention:
		return recovery.failStart(ctx, bindingError(ErrTargetOwnershipUnknown))
	default:
		return recovery.failStart(ctx, bindingError(ErrPortContract))
	}
}

func (recovery *existingFileRecovery) activate(ctx context.Context) (transfer.FileStart, error) {
	next, err := activateRecord(recovery.record)
	if err != nil {
		return recovery.failStart(ctx, err)
	}
	if _, err := recovery.engine.storeRecord(ctx, &recovery.record, next); err != nil {
		return recovery.failStart(ctx, err)
	}
	recovery.record = next
	return recovery.engine.transactionStart(
		recovery.file, recovery.destination, recovery.ownedFile, recovery.record,
	)
}

func (recovery *existingFileRecovery) recoverPublication(
	ctx context.Context,
	retry bool,
) (transfer.FileStart, error) {
	transaction := recovery.engine.newTransaction(
		recovery.file, recovery.destination, recovery.ownedFile, recovery.binding, recovery.record,
	)
	return transaction.recoverPublication(ctx, retry)
}

func (recovery *existingFileRecovery) installQuarantine(
	ctx context.Context,
	reason checkpointmodel.QuarantineReason,
) (transfer.FileStart, error) {
	next, err := quarantineRecord(recovery.record, reason)
	if err != nil {
		return recovery.failStart(ctx, err)
	}
	if _, err := recovery.engine.storeRecord(ctx, &recovery.record, next); err != nil {
		return recovery.failStart(ctx, err)
	}
	recovery.record = next
	settlement, err := quarantinedSettlement(recovery.binding, recovery.record)
	return recovery.engine.terminalStart(
		ctx, recovery.destination, recovery.ownedFile, settlement, err,
	)
}

func (recovery *existingFileRecovery) failStart(
	ctx context.Context,
	causes ...error,
) (transfer.FileStart, error) {
	causes = append(causes, closeOwnedFile(recovery.ownedFile), closeDestination(recovery.destination))
	return transfer.FileStart{}, joinFailures(ctx, causes...)
}
