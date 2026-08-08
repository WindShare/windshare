package fileexecution

import (
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputsession"
	"github.com/windshare/windshare/core/transfer"
)

func (engine *Engine) transactionStart(
	claim outputsession.FileClaim,
	destination FileDestination,
	file OwnedFile,
	record checkpointmodel.Record,
) (outputsession.FileBeginObservation, error) {
	binding, err := outputBinding(claim.File().Target, record)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, fileContractError(err)
	}
	pending, err := content.NewRangeSet(nil)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, fileContractError(err)
	}
	transaction := &Transaction{
		engine: engine, claim: claim, destination: destination, file: file,
		record: record, binding: binding, owned: OwnedReady, pending: pending, state: transactionOpen,
	}
	return transaction.startObservation()
}

func (transaction *Transaction) startObservation() (outputsession.FileBeginObservation, error) {
	if transaction == nil || transaction.record.Phase() != checkpointmodel.PhaseActive ||
		transaction.record.CommitState() != checkpointmodel.CommitVerified || transaction.file == nil ||
		transaction.file.ObjectID() != transaction.record.OwnedOutputObject() {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous},
			fileContractError(ErrPortContract)
	}
	durable, err := durableRanges(transaction.binding, transaction.record)
	if err != nil {
		return outputsession.FileBeginObservation{Cut: outputsession.MutationAmbiguous}, fileContractError(err)
	}
	return outputsession.FileBeginObservation{
		Cut: outputsession.MutationStable, Transaction: transaction, Durable: durable,
	}, nil
}

func quarantinedSettlement(
	binding transfer.OutputFileBinding,
	record checkpointmodel.Record,
) (transfer.FileSettlement, error) {
	if record.Phase() != checkpointmodel.PhaseQuarantined ||
		record.CommitState() != checkpointmodel.CommitQuarantined {
		return transfer.FileSettlement{}, ErrCheckpointBinding
	}
	reference, err := transfer.NewOutputStateRef(
		binding.OutputSessionID(), binding.Locator().Digest(),
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewTransactionQuarantinedFileSettlement(
		binding, reference, transferQuarantineReason(record.QuarantineReason()),
	)
}

func transferQuarantineReason(reason checkpointmodel.QuarantineReason) transfer.QuarantineReason {
	switch reason {
	case checkpointmodel.QuarantinePublicationHistory,
		checkpointmodel.QuarantineFinalMismatch,
		checkpointmodel.QuarantineFinalUnsafe,
		checkpointmodel.QuarantineMetadataMismatch:
		return transfer.QuarantinePublicationAmbiguous
	case checkpointmodel.QuarantinePartialObjectCreation:
		return transfer.QuarantineRetirementMismatch
	case checkpointmodel.QuarantineUpdateTemporary,
		checkpointmodel.QuarantineOutputObjectDuplicate:
		return transfer.QuarantineStateCorrupt
	default:
		return transfer.QuarantineOwnershipMismatch
	}
}

func verifiedSettlement(
	kind transfer.FileSettlementKind,
	binding transfer.OutputFileBinding,
	record checkpointmodel.Record,
) (transfer.FileSettlement, error) {
	durable, err := durableRanges(binding, record)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return transfer.NewVerifiedFileSettlement(kind, durable)
}

func retiredSettlement(
	binding transfer.OutputFileBinding,
	record checkpointmodel.Record,
) (transfer.FileSettlement, error) {
	if record.Phase() != checkpointmodel.PhaseRetired ||
		record.CommitState() != checkpointmodel.CommitVerified ||
		!record.RetirementReason().Valid() {
		return transfer.FileSettlement{}, ErrCheckpointBinding
	}
	return transfer.NewRetiredFileSettlement(binding)
}
