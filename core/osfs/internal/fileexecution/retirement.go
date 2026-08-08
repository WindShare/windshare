package fileexecution

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func (transaction *Transaction) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	return transaction.runTerminalTransition(ctx, TraceRetire, func(ctx context.Context) (
		transfer.FileSettlement,
		outputsession.MutationCut,
		bool,
		error,
	) {
		return transaction.retireLocked(ctx, reason)
	})
}

func (transaction *Transaction) retireLocked(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, err
	}
	retirementReason, err := checkpointRetirementReason(reason)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	expectation, err := transaction.finalExpectation()
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	final, observeErr := transaction.destination.ObserveFinal(ctx, expectation)
	if observeErr != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, collaboratorError(ctx, observeErr)
	}
	if !final.valid() {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, bindingError(ErrInvalidObservation)
	}
	if final.Condition() != FinalAbsent {
		quarantineReason := checkpointmodel.QuarantinePublicationHistory
		if final.Condition() == FinalUnsafe {
			quarantineReason = checkpointmodel.QuarantineFinalUnsafe
		}
		return transaction.settleQuarantineLocked(ctx, quarantineReason, false)
	}
	next, err := retiredRecord(transaction.record, retirementReason)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	previous := transaction.record
	cut, reconciled, err := transaction.engine.storeRecord(ctx, &previous, next)
	if err != nil {
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = next
	fileErr := collaboratorError(ctx, closeOwnedFile(transaction.file))
	transaction.file = nil
	if fileErr != nil {
		transaction.state = transactionClosed
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			joinFailures(ctx, fileErr, collaboratorError(ctx, closeDestination(transaction.destination)))
	}
	cleanupErr := transaction.engine.cleanupOwned(ctx, next.OwnedOutputObject(), OwnedReady)
	if cleanupErr != nil {
		transaction.state = transactionClosed
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			joinFailures(ctx, cleanupErr, collaboratorError(ctx, closeDestination(transaction.destination)))
	}
	settlement, err := retiredSettlement(transaction.binding, next)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	destinationErr := collaboratorError(ctx, closeDestination(transaction.destination))
	transaction.destination = nil
	transaction.state = transactionClosed
	if destinationErr != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, destinationErr
	}
	return settlement, outputsession.MutationStable, reconciled, nil
}

func checkpointRetirementReason(
	reason transfer.FileRetireReason,
) (checkpointmodel.RetirementReason, error) {
	switch reason {
	case transfer.FileRetireIsolatedPermanentSourceFailure:
		return checkpointmodel.RetirementIsolatedFailure, nil
	case transfer.FileRetireInvalidatedRevision:
		return checkpointmodel.RetirementInvalidatedRevision, nil
	default:
		// Explicit policy skip is not a normalized permanent source fault and
		// therefore cannot acquire durable deletion authority.
		return 0, ErrRetirementUnauthorized
	}
}

func (transaction *Transaction) settleQuarantineLocked(
	ctx context.Context,
	reason checkpointmodel.QuarantineReason,
	publicMutation bool,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	next, err := quarantineRecord(transaction.record, reason)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, fileContractError(err)
	}
	previous := transaction.record
	cut, reconciled, err := transaction.engine.storeRecord(ctx, &previous, next)
	if err != nil {
		if publicMutation {
			cut = outputsession.MutationAmbiguous
		}
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = next
	settlement, err := quarantinedSettlement(transaction.binding, next)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	if err := transaction.closeResourcesLocked(ctx); err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, err
	}
	return settlement, outputsession.MutationStable, reconciled, nil
}

func (engine *Engine) cleanupOwned(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	condition OwnedCondition,
) error {
	if engine == nil || ctx == nil || object.IsZero() || !condition.valid() {
		return fileContractError(ErrInvalidConfiguration)
	}
	start := RetirementRemoveStage
	if condition == OwnedStageMissing || condition == OwnedAbsent {
		// Name absence cannot prove that either directory entry survived power
		// loss. Re-sync both namespaces before treating cleanup as durable.
		start = RetirementSyncStageNamespace
	} else if condition != OwnedReady {
		return bindingError(errors.Join(ErrRetirementAmbiguous, ErrInvalidObservation))
	}
	for step := start; step <= RetirementSyncAnchorNamespace; step++ {
		observation, operationErr := engine.platform.ApplyRetirement(ctx, object, step)
		if !observation.validFor(object) || !retirementStepReached(step, observation.Condition()) {
			return joinFailures(ctx,
				bindingError(ErrRetirementAmbiguous), collaboratorError(ctx, operationErr),
			)
		}
		// A fresh exact observation is stronger than the operation's diagnostic
		// error. Continuing from it preserves idempotence across every crash cut.
	}
	return nil
}

func (engine *Engine) retainPublishedWitness(
	ctx context.Context,
	object checkpointmodel.ObjectID,
	condition OwnedCondition,
) error {
	if engine == nil || ctx == nil || object.IsZero() || !condition.valid() {
		return fileContractError(ErrInvalidConfiguration)
	}
	start := RetirementRemoveStage
	if condition == OwnedStageMissing {
		start = RetirementSyncStageNamespace
	} else if condition != OwnedReady {
		return bindingError(errors.Join(ErrRetirementAmbiguous, ErrInvalidObservation))
	}
	for step := start; step <= RetirementSyncStageNamespace; step++ {
		observation, operationErr := engine.platform.ApplyRetirement(ctx, object, step)
		if !observation.validFor(object) || observation.Condition() != OwnedStageMissing {
			return joinFailures(ctx,
				bindingError(ErrRetirementAmbiguous), collaboratorError(ctx, operationErr),
			)
		}
		// The published checkpoint needs the private anchor as its durable identity
		// witness; only the writable stage is retired at this lifecycle boundary.
	}
	return nil
}

func retirementStepReached(step RetirementStep, condition OwnedCondition) bool {
	switch step {
	case RetirementRemoveStage, RetirementSyncStageNamespace:
		return condition == OwnedStageMissing || condition == OwnedAbsent
	case RetirementRemoveAnchor, RetirementSyncAnchorNamespace:
		return condition == OwnedAbsent
	default:
		return false
	}
}
