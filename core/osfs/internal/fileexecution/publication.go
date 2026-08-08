package fileexecution

import (
	"context"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func (transaction *Transaction) Commit(
	ctx context.Context,
) (transfer.FileSettlement, outputsession.MutationCut, error) {
	if transaction == nil || transaction.engine == nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange,
			fileContractError(ErrInvalidConfiguration)
	}
	operationID := transaction.engine.nextOperationID()
	transaction.mu.Lock()
	previousPhase := transaction.record.Phase()
	settlement, cut, reconciled, err := transaction.commitLocked(ctx)
	outcome := traceOutcome(reconciled)
	if settlement.Kind() == transfer.FilePublishBlocked {
		outcome = TraceCollision
	}
	if err != nil {
		outcome = TraceNoChange
		if cut == outputsession.MutationAmbiguous {
			outcome = TraceNeedsAttention
		}
	}
	event := transaction.engine.traceEvent(
		transaction.claim.ID(), operationID, TracePublish, outcome,
		previousPhase, transaction.record.Phase(), traceFault(err),
	)
	transaction.mu.Unlock()
	transaction.engine.emit(event)
	return settlement, cut, err
}

func (transaction *Transaction) commitLocked(
	ctx context.Context,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	if err := transaction.validateOpen(ctx); err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, false, err
	}
	checkpoint, cut, checkpointReconciled, err := transaction.checkpointLocked(ctx)
	if err != nil {
		return transfer.FileSettlement{}, cut, checkpointReconciled, err
	}
	if !transfer.RangesCoverFile(transaction.binding.ExactSize(), checkpoint.Ranges()) {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, checkpointReconciled,
			fileContractError(ErrIncompleteFile)
	}
	expectation, err := transaction.finalExpectation()
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, checkpointReconciled,
			fileContractError(err)
	}
	final, observeErr := transaction.destination.ObserveFinal(ctx, expectation)
	if observeErr != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, checkpointReconciled,
			collaboratorError(ctx, observeErr)
	}
	if !final.valid() {
		return transfer.FileSettlement{}, outputsession.MutationNoChange, checkpointReconciled,
			bindingError(ErrInvalidObservation)
	}
	switch final.Condition() {
	case FinalCollision:
		return transaction.settlePublishBlockedLocked(ctx, false)
	case FinalUnsafe:
		return transaction.settleQuarantineLocked(ctx, checkpointmodel.QuarantineFinalUnsafe, false)
	case FinalOwnedExact, FinalOwnedMetadataMismatch:
		return transaction.settleQuarantineLocked(ctx, checkpointmodel.QuarantinePublicationHistory, false)
	case FinalAbsent:
	default:
		return transfer.FileSettlement{}, outputsession.MutationNoChange, checkpointReconciled,
			bindingError(ErrInvalidObservation)
	}

	metadataReconciled, err := transaction.installMetadata(ctx)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange,
			checkpointReconciled || metadataReconciled, err
	}
	publishing, err := publishingRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationNoChange,
			checkpointReconciled || metadataReconciled, fileContractError(err)
	}
	previous := transaction.record
	cut, transitionReconciled, err := transaction.engine.storeRecord(ctx, &previous, publishing)
	reconciled := checkpointReconciled || metadataReconciled || transitionReconciled
	if err != nil {
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = publishing
	return transaction.finishPublishingLocked(ctx, expectation, reconciled)
}

func (transaction *Transaction) installMetadata(ctx context.Context) (bool, error) {
	modified := transaction.claim.File().Descriptor.ModifiedTime()
	setErr := transaction.file.SetModifiedTime(modified)
	if setErr != nil {
		matches, observeErr := transaction.file.MetadataMatches(transaction.binding.ExactSize(), modified)
		if observeErr != nil {
			return false, joinFailures(ctx, collaboratorError(ctx, setErr), collaboratorError(ctx, observeErr))
		}
		if !matches {
			return false, collaboratorError(ctx, setErr)
		}
	}
	if err := transaction.file.Sync(); err != nil {
		return setErr != nil, collaboratorError(ctx, err)
	}
	return setErr != nil, nil
}

func (transaction *Transaction) finishPublishingLocked(
	ctx context.Context,
	expectation FinalExpectation,
	reconciled bool,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	final, publishErr := transaction.destination.PublishNoReplace(ctx, transaction.file, expectation)
	if !final.valid() {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			joinFailures(ctx, collaboratorError(ctx, publishErr), bindingError(ErrInvalidObservation))
	}
	switch final.Condition() {
	case FinalCollision:
		return transaction.settlePublishBlockedLocked(ctx, true)
	case FinalAbsent:
		cause := collaboratorError(ctx, publishErr)
		if cause == nil {
			cause = bindingError(ErrPortContract)
		}
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			publicationAmbiguousError(cause)
	case FinalUnsafe:
		return transaction.settleQuarantineLocked(ctx, checkpointmodel.QuarantineFinalUnsafe, true)
	case FinalOwnedMetadataMismatch:
		return transaction.settleQuarantineLocked(ctx, checkpointmodel.QuarantineMetadataMismatch, true)
	case FinalOwnedExact:
		reconciled = reconciled || publishErr != nil
	default:
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			bindingError(ErrInvalidObservation)
	}
	return transaction.completePublishedLocked(ctx, reconciled, true)
}

func (transaction *Transaction) completePublishedLocked(
	ctx context.Context,
	reconciled bool,
	publicMutation bool,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	if err := transaction.destination.SyncFinalParent(ctx); err != nil {
		// Exact presence proves no replacement, but a failed parent sync leaves the
		// power-loss publication cut unknowable. The Publishing record is retained
		// for restart recovery rather than pretending this was a file-local failure.
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			publicationAmbiguousError(collaboratorError(ctx, err))
	}
	published, err := publishedRecord(transaction.record)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	previous := transaction.record
	cut, checkpointReconciled, err := transaction.engine.storeRecord(ctx, &previous, published)
	reconciled = reconciled || checkpointReconciled
	if err != nil || cut != outputsession.MutationStable {
		if publicMutation {
			cut = outputsession.MutationAmbiguous
		}
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = published
	settlement, err := verifiedSettlement(transfer.FilePublished, transaction.binding, published)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	fileErr := collaboratorError(ctx, closeOwnedFile(transaction.file))
	transaction.file = nil
	if fileErr != nil {
		transaction.state = transactionClosed
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			joinFailures(ctx, fileErr, collaboratorError(ctx, closeDestination(transaction.destination)))
	}
	witnessErr := transaction.engine.retainPublishedWitness(ctx, published.OwnedOutputObject(), transaction.owned)
	if witnessErr != nil {
		transaction.state = transactionClosed
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled,
			joinFailures(ctx, witnessErr, collaboratorError(ctx, closeDestination(transaction.destination)))
	}
	destinationErr := collaboratorError(ctx, closeDestination(transaction.destination))
	transaction.destination = nil
	transaction.state = transactionClosed
	if destinationErr != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, destinationErr
	}
	return settlement, outputsession.MutationStable, reconciled, nil
}

func (transaction *Transaction) settlePublishBlockedLocked(
	ctx context.Context,
	priorMutation bool,
) (transfer.FileSettlement, outputsession.MutationCut, bool, error) {
	paused, err := pauseRecord(transaction.record)
	if err != nil {
		cut := outputsession.MutationNoChange
		if priorMutation {
			cut = outputsession.MutationAmbiguous
		}
		return transfer.FileSettlement{}, cut, false, fileContractError(err)
	}
	previous := transaction.record
	cut, reconciled, err := transaction.engine.storeRecord(ctx, &previous, paused)
	if err != nil {
		if priorMutation {
			cut = outputsession.MutationAmbiguous
		}
		return transfer.FileSettlement{}, cut, reconciled, err
	}
	transaction.record = paused
	settlement, err := verifiedSettlement(transfer.FilePublishBlocked, transaction.binding, paused)
	if err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, fileContractError(err)
	}
	if err := transaction.closeResourcesLocked(ctx); err != nil {
		return transfer.FileSettlement{}, outputsession.MutationAmbiguous, reconciled, err
	}
	return settlement, outputsession.MutationStable, reconciled, nil
}

func (transaction *Transaction) finalExpectation() (FinalExpectation, error) {
	identity, err := outputIdentity(transaction.record.OwnedOutputObject())
	if err != nil {
		return FinalExpectation{}, err
	}
	return NewFinalExpectation(
		identity, transaction.binding.ExactSize(), transaction.claim.File().Descriptor.ModifiedTime(),
	)
}
