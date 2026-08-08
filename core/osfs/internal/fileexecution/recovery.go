package fileexecution

import (
	"context"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

type recoveryObservation struct {
	file        OwnedFile
	owned       OwnedObservation
	final       FinalObservation
	expectation FinalExpectation
	reconciled  bool
}

type recoveryExecution struct {
	observation outputsession.FileBeginObservation
	cut         outputsession.MutationCut
	reconciled  bool
}

func (engine *Engine) beginExisting(
	ctx context.Context,
	operationID uint64,
	claim outputsession.FileClaim,
	destination FileDestination,
	record checkpointmodel.Record,
) (outputsession.FileBeginObservation, error) {
	binding, err := outputBinding(claim.File().Target, record)
	if err != nil {
		err = joinFailures(ctx, fileContractError(err), collaboratorError(ctx, destination.Close()))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	if record.Phase() == checkpointmodel.PhaseQuarantined {
		return engine.returnQuarantinedRecovery(ctx, operationID, claim.ID(), destination, binding, record)
	}
	observed, err := engine.observeRecovery(ctx, claim, destination, record)
	if err != nil {
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	transaction := newRecoveryTransaction(engine, claim, destination, binding, record, observed)
	if record.CommitState() == checkpointmodel.CommitCandidate {
		return engine.executeInitialCandidateRecovery(ctx, operationID, transaction, observed)
	}
	decision, err := ReduceRecovery(record, observed.owned, observed.final)
	if err != nil {
		err = joinFailures(ctx, checkpointBindingError(err), transaction.closeForFailedBegin(ctx))
		return engine.failedBegin(claim.ID(), operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	execution, err := transaction.executeRecovery(
		ctx, decision, observed.owned, observed.expectation, observed.reconciled,
	)
	if err != nil {
		err = joinFailures(ctx, err, transaction.closeForFailedBegin(ctx))
		return engine.failedBegin(claim.ID(), operationID, execution.cut, TraceNeedsAttention, err)
	}
	engine.emit(engine.traceEvent(
		claim.ID(), operationID, TraceRecoverFile, traceOutcome(execution.reconciled),
		record.Phase(), transaction.record.Phase(), fault.Fault{},
	))
	return execution.observation, nil
}

func (engine *Engine) returnQuarantinedRecovery(
	ctx context.Context,
	operationID uint64,
	claimID outputsession.ClaimID,
	destination FileDestination,
	binding transfer.OutputFileBinding,
	record checkpointmodel.Record,
) (outputsession.FileBeginObservation, error) {
	settlement, settlementErr := quarantinedSettlement(binding, record)
	closeErr := collaboratorError(ctx, destination.Close())
	if settlementErr != nil || closeErr != nil {
		err := joinFailures(ctx, fileContractError(settlementErr), closeErr)
		return engine.failedBegin(claimID, operationID, outputsession.MutationNoChange, TraceNoChange, err)
	}
	engine.emit(engine.traceEvent(
		claimID, operationID, TraceRecoverFile, TraceSucceeded,
		record.Phase(), record.Phase(), fault.Fault{},
	))
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Settlement: settlement,
	}, nil
}

func (engine *Engine) observeRecovery(
	ctx context.Context,
	claim outputsession.FileClaim,
	destination FileDestination,
	record checkpointmodel.Record,
) (recoveryObservation, error) {
	writable := record.Phase() == checkpointmodel.PhaseActive ||
		record.Phase() == checkpointmodel.PhasePaused || record.Phase() == checkpointmodel.PhasePublishing
	file, owned, openErr := engine.platform.OpenOwnedFile(
		ctx, record.OwnedOutputObject(), record.ExactSize(), writable,
	)
	if err := validateRecoveryOwnedObservation(record, file, owned); err != nil {
		return recoveryObservation{}, joinFailures(ctx,
			bindingError(err), collaboratorError(ctx, openErr),
			collaboratorError(ctx, closeOwnedFile(file)), collaboratorError(ctx, destination.Close()),
		)
	}
	expectation, err := expectationForRecord(claim, record)
	if err != nil {
		return recoveryObservation{}, joinFailures(ctx,
			fileContractError(err), collaboratorError(ctx, closeOwnedFile(file)),
			collaboratorError(ctx, destination.Close()),
		)
	}
	final, finalErr := destination.ObserveFinal(ctx, expectation)
	if finalErr != nil || !final.valid() {
		if finalErr == nil {
			finalErr = bindingError(ErrInvalidObservation)
		} else {
			finalErr = collaboratorError(ctx, finalErr)
		}
		return recoveryObservation{}, joinFailures(ctx,
			finalErr, collaboratorError(ctx, closeOwnedFile(file)),
			collaboratorError(ctx, destination.Close()),
		)
	}
	return recoveryObservation{
		file: file, owned: owned, final: final, expectation: expectation,
		// A fresh exact observation outranks the operation diagnostic; the trace
		// still records that recovery reconciled a counterintuitive boundary result.
		reconciled: openErr != nil,
	}, nil
}

func validateRecoveryOwnedObservation(
	record checkpointmodel.Record,
	file OwnedFile,
	owned OwnedObservation,
) error {
	if !owned.validFor(record.OwnedOutputObject()) {
		return ErrInvalidObservation
	}
	if owned.Condition() == OwnedReady {
		if file == nil || file.ObjectID() != record.OwnedOutputObject() {
			return ErrInvalidObservation
		}
		return nil
	}
	if file != nil {
		return ErrInvalidObservation
	}
	return nil
}

func newRecoveryTransaction(
	engine *Engine,
	claim outputsession.FileClaim,
	destination FileDestination,
	binding transfer.OutputFileBinding,
	record checkpointmodel.Record,
	observed recoveryObservation,
) *Transaction {
	pending, _ := content.NewRangeSet(nil)
	return &Transaction{
		engine: engine, claim: claim, destination: destination, file: observed.file,
		record: record, binding: binding, owned: observed.owned.Condition(), pending: pending,
		state: transactionOpen,
	}
}

func (engine *Engine) executeInitialCandidateRecovery(
	ctx context.Context,
	operationID uint64,
	transaction *Transaction,
	observed recoveryObservation,
) (outputsession.FileBeginObservation, error) {
	previousPhase := transaction.record.Phase()
	result, reconciled, err := transaction.recoverInitialCandidateLocked(
		ctx, observed.owned, observed.final, observed.reconciled,
	)
	if err != nil {
		err = joinFailures(ctx, err, transaction.closeForFailedBegin(ctx))
		return engine.failedBegin(
			transaction.claim.ID(), operationID, result.Cut, TraceNeedsAttention, err,
		)
	}
	engine.emit(engine.traceEvent(
		transaction.claim.ID(), operationID, TraceRecoverFile, traceOutcome(reconciled),
		previousPhase, transaction.record.Phase(), fault.Fault{},
	))
	return result, nil
}

func (transaction *Transaction) executeRecovery(
	ctx context.Context,
	decision RecoveryDecision,
	owned OwnedObservation,
	expectation FinalExpectation,
	reconciled bool,
) (recoveryExecution, error) {
	switch decision.Action() {
	case RecoveryOpenActive:
		result, err := transaction.engine.transactionStart(
			transaction.claim, transaction.destination, transaction.file, transaction.record,
		)
		return recoveryExecution{observation: result, cut: result.Cut, reconciled: reconciled}, err
	case RecoveryActivate:
		result, err := transaction.activateRecovered(ctx)
		return recoveryExecution{observation: result, cut: result.Cut, reconciled: reconciled}, err
	case RecoveryRetryPublication:
		settlement, cut, nextReconciled, err := transaction.finishPublishingLocked(
			ctx, expectation, reconciled,
		)
		return settledRecoveryExecution(settlement, cut, nextReconciled, err)
	case RecoveryCompletePublication:
		settlement, cut, nextReconciled, err := transaction.completePublishedLocked(ctx, reconciled, false)
		return settledRecoveryExecution(settlement, cut, nextReconciled, err)
	case RecoveryPublishBlocked:
		if transaction.record.Phase() == checkpointmodel.PhasePaused {
			result, err := transaction.returnPublishBlocked(ctx)
			return recoveryExecution{observation: result, cut: result.Cut, reconciled: reconciled}, err
		}
		settlement, cut, nextReconciled, err := transaction.settlePublishBlockedLocked(ctx, false)
		return settledRecoveryExecution(settlement, cut, nextReconciled, err)
	case RecoveryReturnPublished:
		result, err := transaction.returnTerminalCheckpoint(ctx, owned, transfer.FilePublished)
		return recoveryExecution{observation: result, cut: result.Cut, reconciled: reconciled}, err
	case RecoveryReturnRetired:
		result, err := transaction.returnTerminalCheckpoint(ctx, owned, transfer.FileRetired)
		return recoveryExecution{observation: result, cut: result.Cut, reconciled: reconciled}, err
	case RecoveryInstallQuarantine:
		settlement, cut, nextReconciled, err := transaction.settleQuarantineLocked(
			ctx, decision.QuarantineReason(), false,
		)
		return settledRecoveryExecution(settlement, cut, nextReconciled, err)
	case RecoveryNeedsAttention:
		return recoveryExecution{cut: outputsession.MutationNoChange, reconciled: reconciled},
			bindingError(ErrCheckpointBinding)
	default:
		return recoveryExecution{cut: outputsession.MutationNoChange, reconciled: reconciled},
			bindingError(ErrPortContract)
	}
}

func settledRecoveryExecution(
	settlement transfer.FileSettlement,
	cut outputsession.MutationCut,
	reconciled bool,
	err error,
) (recoveryExecution, error) {
	execution := recoveryExecution{cut: cut, reconciled: reconciled}
	if err == nil {
		execution.observation = outputsession.FileBeginObservation{
			Cut:        outputsession.MutationStable,
			Settlement: settlement,
		}
	}
	return execution, err
}

func (transaction *Transaction) recoverInitialCandidateLocked(
	ctx context.Context,
	owned OwnedObservation,
	final FinalObservation,
	reconciled bool,
) (outputsession.FileBeginObservation, bool, error) {
	if transaction.record.Phase() != checkpointmodel.PhaseActive ||
		transaction.record.CommitState() != checkpointmodel.CommitCandidate {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			reconciled, checkpointBindingError(ErrCheckpointBinding)
	}
	if final.Condition() != FinalAbsent || owned.Condition() != OwnedReady {
		// A candidate cannot transition directly to quarantine. Promoting its empty
		// range set grants no bytes, but it gives the canonical lifecycle reducer a
		// committed state from which the broken topology can be quarantined.
		verified, err := checkpointmodel.PromoteInitialCandidate(transaction.record)
		if err != nil {
			return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
				reconciled, checkpointBindingError(err)
		}
		previous := transaction.record
		cut, storedReconciled, err := transaction.engine.storeRecord(ctx, &previous, verified)
		reconciled = reconciled || storedReconciled
		if err != nil {
			return outputsession.FileBeginObservation{Cut: cut}, reconciled, err
		}
		transaction.record = verified
		reason := checkpointmodel.QuarantinePublicationHistory
		switch {
		case final.Condition() == FinalUnsafe:
			reason = checkpointmodel.QuarantineFinalUnsafe
		case final.Condition() == FinalAbsent:
			reason = ownedQuarantineReason(owned.Condition())
		}
		settlement, cut, quarantineReconciled, err := transaction.settleQuarantineLocked(ctx, reason, true)
		return outputsession.FileBeginObservation{Cut: cut, Settlement: settlement},
			reconciled || quarantineReconciled, err
	}
	if transaction.file == nil || transaction.file.ObjectID() != transaction.record.OwnedOutputObject() {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			reconciled, bindingError(ErrInvalidObservation)
	}
	if err := transaction.file.Sync(); err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			reconciled, collaboratorError(ctx, err)
	}
	verified, err := checkpointmodel.PromoteInitialCandidate(transaction.record)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange},
			reconciled, checkpointBindingError(err)
	}
	previous := transaction.record
	cut, storedReconciled, err := transaction.engine.storeRecord(ctx, &previous, verified)
	reconciled = reconciled || storedReconciled
	if err != nil {
		return outputsession.FileBeginObservation{Cut: cut}, reconciled, err
	}
	transaction.record = verified
	result, err := transaction.startObservation()
	return result, reconciled, err
}

func (transaction *Transaction) activateRecovered(
	ctx context.Context,
) (outputsession.FileBeginObservation, error) {
	next, err := activateRecord(transaction.record)
	if err != nil {
		return outputsession.FileBeginObservation{}, fileContractError(err)
	}
	previous := transaction.record
	cut, _, err := transaction.engine.storeRecord(ctx, &previous, next)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: cut}, err
	}
	transaction.record = next
	return transaction.startObservation()
}

func (transaction *Transaction) returnPublishBlocked(
	ctx context.Context,
) (outputsession.FileBeginObservation, error) {
	settlement, err := verifiedSettlement(transfer.FilePublishBlocked, transaction.binding, transaction.record)
	if err == nil {
		err = transaction.closeResourcesLocked(ctx)
	}
	if err != nil {
		return outputsession.FileBeginObservation{}, err
	}
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Settlement: settlement,
	}, nil
}

func (transaction *Transaction) returnTerminalCheckpoint(
	ctx context.Context,
	owned OwnedObservation,
	kind transfer.FileSettlementKind,
) (outputsession.FileBeginObservation, error) {
	if err := collaboratorError(ctx, closeOwnedFile(transaction.file)); err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationNoChange}, err
	}
	transaction.file = nil
	var cleanupErr error
	if kind == transfer.FilePublished {
		cleanupErr = transaction.engine.retainPublishedWitness(
			ctx, transaction.record.OwnedOutputObject(), owned.Condition(),
		)
	} else {
		cleanupErr = transaction.engine.cleanupOwned(
			ctx, transaction.record.OwnedOutputObject(), owned.Condition(),
		)
	}
	if cleanupErr != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, cleanupErr
	}
	var settlement transfer.FileSettlement
	var err error
	if kind == transfer.FilePublished {
		settlement, err = verifiedSettlement(kind, transaction.binding, transaction.record)
	} else {
		settlement, err = retiredSettlement(transaction.binding, transaction.record)
	}
	if err == nil {
		err = collaboratorError(ctx, closeDestination(transaction.destination))
	}
	transaction.destination = nil
	transaction.state = transactionClosed
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, err
	}
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Settlement: settlement,
	}, nil
}

func (transaction *Transaction) closeForFailedBegin(ctx context.Context) error {
	if transaction == nil || transaction.state == transactionClosed {
		return nil
	}
	return transaction.closeResourcesLocked(ctx)
}

func expectationForRecord(
	claim outputsession.FileClaim,
	record checkpointmodel.Record,
) (FinalExpectation, error) {
	identity, err := outputIdentity(record.OwnedOutputObject())
	if err != nil {
		return FinalExpectation{}, err
	}
	return NewFinalExpectation(
		identity, record.ExactSize(), claim.File().Descriptor.ModifiedTime(),
	)
}
