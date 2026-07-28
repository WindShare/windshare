package outputruntime

import (
	"bytes"
	"context"
	"errors"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) retireBoundFile(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
) (resultSettlement transfer.FileSettlement, resultQuarantined bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, bound.Record().LocatorDigest(), err)
		return transfer.FileSettlement{}, false,
			outputAncestryOperationFault("validate ancestry before file retirement", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			bound.Record().LocatorDigest(), "finish file retirement ancestry", nil,
		)
		if ancestryErr != nil {
			resultSettlement = transfer.FileSettlement{}
			resultQuarantined = false
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	for {
		decision, observationCleanupErr, err := session.decideFileRetirement(validation, bound)
		if err != nil {
			return transfer.FileSettlement{}, false, err
		}
		session.traceFileRetirementDecision(bound, decision)
		if err := fileRetirementObservationCleanupFault(decision, observationCleanupErr); err != nil {
			return transfer.FileSettlement{}, false, err
		}
		step, err := session.applyFileRetirementDecision(
			recordDir, recordName, bound, binding, decision, observationCleanupErr,
		)
		if err != nil || step.complete {
			return step.settlement, step.quarantined, err
		}
	}
}

func removeBoundFileRecord(
	directory outputcap.Directory,
	name string,
	bound resumestate.BoundFileRecord,
) (error, error) {
	expected, err := resumestate.EncodeFileRecord(bound)
	if err != nil {
		return err, nil
	}
	file, err := directory.OpenFile(name, true, false)
	if err != nil {
		return err, closeOutputV3File(file)
	}
	actual, err := outputnamespace.ReadFile(file, resumestate.MaxFileStateBytes)
	if err != nil || !bytes.Equal(actual, expected) {
		return errors.Join(outputcap.ErrUnsafeNamespace, err), file.Close()
	}
	operationErr := directory.RemoveFile(name, file)
	if operationErr == nil {
		operationErr = directory.Sync()
	}
	return operationErr, file.Close()
}

func quarantinedSettlement(
	binding transfer.OutputFileBinding,
	record resumestate.FileRecord,
) (transfer.FileSettlement, error) {
	reference, err := transfer.NewOutputStateRef(binding.OutputSessionID(), record.LocatorDigest().OutputLocatorDigest())
	if err != nil {
		return transfer.FileSettlement{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, err := transfer.NewTransactionQuarantinedFileSettlement(
		binding, reference, mapQuarantineReason(record.QuarantineReason()),
	)
	if err != nil {
		return transfer.FileSettlement{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return settlement, nil
}

func mapQuarantineReason(reason resumestate.QuarantineReason) transfer.QuarantineReason {
	switch reason {
	case resumestate.QuarantinePublicationHistory, resumestate.QuarantineFinalMismatch,
		resumestate.QuarantineFinalUnsafe, resumestate.QuarantineMetadataMismatch:
		return transfer.QuarantinePublicationAmbiguous
	case resumestate.QuarantinePartialObjectCreation:
		return transfer.QuarantineRetirementMismatch
	case resumestate.QuarantineUpdateTemporary, resumestate.QuarantineOutputObjectDuplicate:
		return transfer.QuarantineStateCorrupt
	default:
		return transfer.QuarantineOwnershipMismatch
	}
}

func openOutputShard(
	parent outputcap.Directory,
	name string,
	create bool,
) (outputcap.Directory, bool, error) {
	if !validStateShard(name) {
		return nil, false, outputcap.ErrUnsafeNamespace
	}
	if create {
		result, err := outputnamespace.EnsureDirectory(parent, name, true)
		return result.Directory, err == nil, err
	}
	result, err := outputnamespace.OpenOptionalDirectory(parent, name, true)
	return result.Directory, result.Disposition != outputnamespace.DirectoryAbsent, err
}

func (transaction *FileTransaction) Pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (settlement transfer.FileSettlement, resultErr error) {
	if transaction == nil || reason < transfer.FilePauseInterrupted || reason > transfer.FilePauseOutputFailure {
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer transaction.session.endOperation()
	return transaction.pause(ctx, reason, true, FilesystemOutputSettlementPause)
}

func (transaction *FileTransaction) pauseForSessionSettlement(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	return transaction.pause(ctx, reason, false, FilesystemOutputSettlementJobPause)
}

func (transaction *FileTransaction) pauseForBeginFileCleanup(
	ctx context.Context,
	reason transfer.FilePauseReason,
) (transfer.FileSettlement, error) {
	return transaction.pause(ctx, reason, false, FilesystemOutputSettlementBeginFileCleanup)
}

func (transaction *FileTransaction) pause(
	ctx context.Context,
	reason transfer.FilePauseReason,
	requireActiveSession bool,
	boundary FilesystemOutputFileSettlementBoundary,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := transaction.claimTerminalSettlement(requireActiveSession); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer func() {
		transaction.session.traceReturnedFileSettlement(filesystemOutputFileSettlementTraceContext{
			boundary: boundary, pauseReason: reason,
		}, settlement, resultErr)
	}()
	defer transaction.finishTerminalResult(&resultErr, "close paused output")
	return transaction.pauseSettling(ctx)
}

func (transaction *FileTransaction) pauseSettling(
	ctx context.Context,
) (transfer.FileSettlement, error) {
	settleErr := ctx.Err()
	transaction.mu.Lock()
	if transaction.lifecycle != FileTransactionSettling {
		transaction.mu.Unlock()
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	var checkpoint transfer.VerifiedDurableRanges
	checkpointErr := settleErr
	if checkpointErr == nil {
		checkpoint, checkpointErr = transaction.checkpointLocked()
	}
	if checkpointErr != nil {
		record := transaction.resumable.Bound().Record()
		checkpoint, _ = transfer.VerifyDurableRanges(
			transaction.binding, transfer.CheckpointGeneration(record.CheckpointGeneration()), record.DurableRanges(),
		)
	}
	settlement, err := transfer.NewVerifiedFileSettlement(transfer.FilePaused, checkpoint)
	transaction.mu.Unlock()
	if err != nil {
		return transfer.FileSettlement{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract,
			errors.Join(err, checkpointErr))
	}
	if checkpointErr != nil {
		return settlement, fileSettlementFailure(checkpointErr)
	}
	return settlement, nil
}

func (transaction *FileTransaction) Retire(
	ctx context.Context,
	reason transfer.FileRetireReason,
) (settlement transfer.FileSettlement, resultErr error) {
	if err := ctx.Err(); err != nil {
		return transfer.FileSettlement{}, fileSettlementFailure(err)
	}
	if transaction == nil || reason < transfer.FileRetireIsolatedPermanentSourceFailure ||
		reason > transfer.FileRetireExplicitPolicySkip {
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := transaction.session.beginOperation(); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer transaction.session.endOperation()
	if err := transaction.claimTerminalSettlement(true); err != nil {
		return transfer.FileSettlement{}, err
	}
	defer func() {
		transaction.session.traceReturnedFileSettlement(filesystemOutputFileSettlementTraceContext{
			boundary: FilesystemOutputSettlementRetire, retireReason: reason,
		}, settlement, resultErr)
	}()
	var validation *outputAncestryValidation
	defer func() {
		transaction.finishTerminalResult(&resultErr, "close retired output")
		if validation == nil {
			return
		}
		ancestryErr := finishOutputAncestryOperation(
			transaction.session, validation, outputAncestryRequirement{},
			FilesystemOutputAncestryRecovery,
			transaction.resumable.Bound().Record().LocatorDigest(),
			"finish retired output ancestry",
			nil,
		)
		if ancestryErr != nil {
			settlement = transfer.FileSettlement{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()

	retiring, err := transaction.prepareRetirement(reason)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	requirement := outputAncestryRequirement{}
	validation, err = transaction.session.validateOutputAncestry(requirement)
	if err != nil {
		transaction.session.traceOutputAncestry(
			FilesystemOutputAncestryRecovery,
			transaction.resumable.Bound().Record().LocatorDigest(),
			err,
		)
		return transfer.FileSettlement{}, outputAncestryOperationFault(
			"validate ancestry before retiring output", err,
		)
	}
	return transaction.retireSettling(retiring)
}

func (transaction *FileTransaction) prepareRetirement(
	reason transfer.FileRetireReason,
) (resumestate.BoundFileRecord, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != FileTransactionSettling || transaction.session.operationDisabled() {
		return resumestate.BoundFileRecord{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed,
		)
	}
	var retiring resumestate.BoundFileRecord
	var err error
	if reason == transfer.FileRetireInvalidatedRevision {
		retiring, err = resumestate.PrepareInvalidatedRevisionRetirement(transaction.resumable.Bound())
	} else {
		retiring, err = resumestate.PrepareIsolatedRetirement(transaction.resumable.Bound())
	}
	if err != nil {
		return resumestate.BoundFileRecord{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := transaction.session.installFileRecord(
		transaction.recordDir, transaction.recordName, transaction.resumable.Bound(), retiring,
	); err != nil {
		return resumestate.BoundFileRecord{}, err
	}
	return retiring, nil
}

func (transaction *FileTransaction) retireSettling(
	retiring resumestate.BoundFileRecord,
) (transfer.FileSettlement, error) {
	transaction.mu.Lock()
	closeErr := errors.Join(transaction.data.Close(), transaction.anchor.Close())
	transaction.data, transaction.anchor = stagedData{}, anchorWitness{}
	transaction.mu.Unlock()
	if closeErr != nil {
		return transfer.FileSettlement{}, fileOutputFault("close retiring output", closeErr)
	}
	settlement, _, err := transaction.session.retireBoundFile(
		transaction.recordDir, transaction.recordName, retiring, transaction.binding,
	)
	if err != nil {
		return transfer.FileSettlement{}, err
	}
	return settlement, nil
}

func (transaction *FileTransaction) claimTerminalSettlement(requireActiveSession bool) error {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.lifecycle != FileTransactionOpen ||
		requireActiveSession && transaction.session.operationDisabled() {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	transaction.lifecycle = FileTransactionSettling
	return nil
}

func (transaction *FileTransaction) finishTerminalResult(
	resultErr *error,
	operation string,
) {
	closeErr := transaction.finishTerminalSettlement()
	if closeErr != nil {
		*resultErr = errors.Join(
			*resultErr,
			pauseRequiredFileOperationFault(operation, nil, closeErr),
		)
	}
}

func (transaction *FileTransaction) finishTerminalSettlement() error {
	transaction.mu.Lock()
	if transaction.lifecycle != FileTransactionSettling {
		transaction.mu.Unlock()
		return outputcap.ErrUnsafeNamespace
	}
	transaction.lifecycle = FileTransactionClosed
	digest := transaction.resumable.Bound().Record().LocatorDigest()
	closeErr := transaction.closeHandlesLocked()
	transaction.mu.Unlock()
	transaction.session.finishFile(digest, transaction)
	return closeErr
}

func (transaction *FileTransaction) closeHandles() error {
	if transaction == nil {
		return nil
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	return transaction.closeHandlesLocked()
}

func (transaction *FileTransaction) closeHandlesLocked() error {
	var result error
	if transaction.data.valid() {
		result = errors.Join(result, transaction.data.Close())
		transaction.data = stagedData{}
	}
	if transaction.anchor.valid() {
		result = errors.Join(result, transaction.anchor.Close())
		transaction.anchor = anchorWitness{}
	}
	if transaction.stageDir != nil {
		result = errors.Join(result, transaction.stageDir.Close())
		transaction.stageDir = nil
	}
	if transaction.anchorDir != nil {
		result = errors.Join(result, transaction.anchorDir.Close())
		transaction.anchorDir = nil
	}
	if transaction.recordDir != nil {
		result = errors.Join(result, transaction.recordDir.Close())
		transaction.recordDir = nil
	}
	return result
}

func (session *Session) finishFile(
	digest resumestate.LocatorDigest,
	transaction *FileTransaction,
) {
	session.mu.Lock()
	if session.active[digest] == transaction {
		delete(session.active, digest)
	}
	session.mu.Unlock()
}

func outputDirectoryOperationFault(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, errOutputAncestryUnsafe) ||
		(errors.Is(cause, outputcap.ErrUnsafeNamespace) && !errors.Is(cause, errOutputAncestryAuthorityDenied)) ||
		(errors.Is(cause, outputnamespace.ErrPositiveEntryEvidence) && errors.Is(cause, outputcap.ErrNamespaceCollision)) {
		return outputAncestryPauseFault(operation, cause)
	}
	return transfer.NewOutputSessionError(directoryOutputFault(operation, cause), true)
}

func fileSettlementFailure(cause error) error {
	if _, found := errors.AsType[*transfer.OutputFault](cause); found {
		return cause
	}
	return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultStateIO, cause)
}
