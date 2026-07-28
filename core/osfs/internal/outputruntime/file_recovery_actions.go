package outputruntime

import (
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) applyFileRecoveryAction(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	iteration fileRecoveryIteration,
) (fileRecoveryActionResult, error) {
	switch iteration.decision.Action() {
	case resumestate.RecoveryRetryObjectCreation:
		return session.retryFileWitness(file, recordDir, recordName, state)
	case resumestate.RecoveryInstallWitness, resumestate.RecoveryInstallPublishing,
		resumestate.RecoveryInstallPublished, resumestate.RecoveryInstallPublishBlocked,
		resumestate.RecoveryInstallRetiring, resumestate.RecoveryInstallQuarantine:
		return session.installFileRecoveryDecision(file, recordDir, recordName, state, iteration)
	case resumestate.RecoveryResumeContent:
		return finishFileRecovery(session.transactionStart(
			file.Descriptor, state.resumable, recordDir, recordName,
		))
	case resumestate.RecoveryLinkFinalNoReplace:
		return session.linkRecoveredFinal(file, recordDir, recordName, state)
	case resumestate.RecoverySyncFinalParent:
		return session.syncRecoveredFinalParent(file, recordDir, recordName, state)
	case resumestate.RecoveryHoldPublishBlocked:
		return finishFileRecovery(session.verifiedStart(transfer.FilePublishBlocked, state.resumable))
	case resumestate.RecoveryRemovePublishedStageAndSync:
		return session.removeRecoveredPublishedStage(state)
	case resumestate.RecoverySyncPublishedStageParent:
		return session.syncRecoveredPublishedStage(state)
	case resumestate.RecoveryHoldPublishedCleanup:
		return fileRecoveryActionResult{}, internalCleanupNeedsAttentionFault(
			"hold published file with ambiguous internal cleanup evidence",
		)
	case resumestate.RecoveryHoldQuarantine:
		return session.holdRecoveredQuarantine(file, state, iteration.observationCleanupErr)
	case resumestate.RecoveryHoldRetiringCleanup:
		return fileRecoveryActionResult{}, internalCleanupNeedsAttentionFault(
			"hold retiring file with ambiguous internal cleanup evidence",
		)
	case resumestate.RecoveryRemoveRetiringStageAndSync,
		resumestate.RecoverySyncStageRemoveAnchorAndSync,
		resumestate.RecoverySyncParentsRemoveRecordAndSync:
		return session.retireRecoveredFile(file, recordDir, recordName, state)
	default:
		return fileRecoveryActionResult{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract,
			fmt.Errorf("unsupported recovery action %d", iteration.decision.Action()),
		)
	}
}

func continuingFileRecovery(state fileRecoveryState) fileRecoveryActionResult {
	return fileRecoveryActionResult{state: state}
}

func finishFileRecovery(
	start transfer.FileStart,
	err error,
) (fileRecoveryActionResult, error) {
	return fileRecoveryActionResult{start: start, terminal: true}, err
}

func (session *Session) retryFileWitness(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	operationErr, cleanupErr := session.createWitnessObject(state.resumable.Bound().Record())
	if operationErr != nil {
		if classifyOutputV3RecoveryFailure(operationErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, recordDir, recordName, state.resumable.Bound(), resumestate.QuarantinePartialObjectCreation,
				"close quarantined witnessed output object", cleanupErr,
			))
		}
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"create witnessed output object", operationErr, cleanupErr,
		)
	}
	if cleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"close created witnessed output object", nil, cleanupErr,
		)
	}
	return continuingFileRecovery(state), nil
}

func (session *Session) installFileRecoveryDecision(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	iteration fileRecoveryIteration,
) (fileRecoveryActionResult, error) {
	next, err := resumestate.ApplyRecoveryDecision(state.resumable.Bound(), iteration.decision)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, state.resumable.Bound(), next); err != nil {
		return fileRecoveryActionResult{}, err
	}
	state.resumable, err = resumestate.BindResumableFile(next, file.Descriptor)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return session.finishInstalledFileRecovery(file, recordDir, recordName, state, iteration)
}

func (session *Session) finishInstalledFileRecovery(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	iteration fileRecoveryIteration,
) (fileRecoveryActionResult, error) {
	switch iteration.decision.Action() {
	case resumestate.RecoveryInstallPublishBlocked:
		return finishFileRecovery(session.verifiedStart(transfer.FilePublishBlocked, state.resumable))
	case resumestate.RecoveryInstallQuarantine:
		if iteration.observationCleanupErr != nil {
			return fileRecoveryActionResult{}, pauseRequiredFileOutputFault(fileOutputFault(
				"close quarantined file recovery observation", iteration.observationCleanupErr,
			))
		}
		return finishFileRecovery(session.quarantinedStart(
			file.Target, state.resumable.Bound().Record().LocatorDigest(),
			mapQuarantineReason(iteration.decision.QuarantineReason()),
		))
	case resumestate.RecoveryInstallRetiring:
		return session.finishInstalledRetirement(file, recordDir, recordName, state, iteration.decision)
	default:
		return continuingFileRecovery(state), nil
	}
}

func (session *Session) finishInstalledRetirement(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	decision resumestate.RecoveryDecision,
) (fileRecoveryActionResult, error) {
	bound := state.resumable.Bound()
	binding, err := outputBindingForRecord(session.SessionID(), file.Descriptor, bound.Record())
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, _, cleanupErr := session.retireBoundFile(recordDir, recordName, bound, binding)
	if cleanupErr != nil {
		return fileRecoveryActionResult{}, cleanupErr
	}
	if decision.Settlement() == resumestate.RecoveryCollision {
		return finishFileRecovery(session.collisionStart(file))
	}
	return finishFileRecovery(transfer.NewFileSettlementStart(settlement))
}

type recoveryPublicationAttempt struct {
	result     resumestate.PublishResult
	linkErr    error
	cleanupErr error
}

func (session *Session) linkRecoveredFinal(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	// The retained witness keeps publication anchored to the selected object;
	// the fixed names are revalidated immediately before the no-replace link.
	witness, witnessErr, witnessCleanupErr := session.openPublicationWitness(
		state.resumable.Bound().Record(), anchorWitness{},
	)
	if witnessErr != nil {
		if classifyOutputV3RecoveryFailure(witnessErr, outputV3ExistingEntryUnclassified) == outputV3RecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, recordDir, recordName, state.resumable.Bound(), resumestate.QuarantinePublicationHistory,
				"close quarantined recovery publication witness", witnessCleanupErr,
			))
		}
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"retain recovery publication witness", witnessErr, witnessCleanupErr,
		)
	}
	if witnessCleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"close retained recovery publication witness", nil, witnessCleanupErr,
		)
	}
	attempt := session.attemptRecoveredPublication(state.resumable.Bound(), witness)
	if result, handled, err := session.handleUnclassifiedRecoveredPublication(
		file, recordDir, recordName, state, attempt,
	); handled {
		return result, err
	}
	if attempt.result != resumestate.PublishLinkCreated {
		return session.settleClassifiedRecoveredPublication(file, recordDir, recordName, state, attempt)
	}
	if attempt.linkErr != nil || attempt.cleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"finish final publication", attempt.linkErr, attempt.cleanupErr,
		)
	}
	state.parentSynced = true
	return continuingFileRecovery(state), nil
}

func (session *Session) attemptRecoveredPublication(
	bound resumestate.BoundFileRecord,
	witness *publicationWitness,
) recoveryPublicationAttempt {
	result, linkErr, cleanupErr := session.linkFinalNoReplaceResult(bound, witness)
	return recoveryPublicationAttempt{
		result: result, linkErr: linkErr, cleanupErr: errors.Join(cleanupErr, witness.Close()),
	}
}

func (session *Session) handleUnclassifiedRecoveredPublication(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	attempt recoveryPublicationAttempt,
) (fileRecoveryActionResult, bool, error) {
	if attempt.result != 0 {
		return fileRecoveryActionResult{}, false, nil
	}
	if errors.Is(attempt.linkErr, errOutputAncestryUnsafe) {
		return fileRecoveryActionResult{}, true,
			outputAncestryPauseFault("revalidate recovery final publication", attempt.linkErr)
	}
	if errors.Is(attempt.linkErr, outputcap.ErrFixedLinkSourceChanged) {
		result, err := finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
			file, recordDir, recordName, state.resumable.Bound(), resumestate.QuarantineAnchorUnsafe,
			"close invalidated recovery publication witness", attempt.cleanupErr,
		))
		return result, true, err
	}
	if attempt.linkErr != nil {
		if classifyOutputV3RecoveryFailure(attempt.linkErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
			result, err := finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, recordDir, recordName, state.resumable.Bound(), resumestate.QuarantinePublicationHistory,
				"close quarantined final-link evidence", attempt.cleanupErr,
			))
			return result, true, err
		}
		return fileRecoveryActionResult{}, true, pauseRequiredFileOperationFault(
			"publish final link", attempt.linkErr, attempt.cleanupErr,
		)
	}
	return fileRecoveryActionResult{}, true, pauseRequiredFileOperationFault(
		"close unclassified final-link evidence", nil, attempt.cleanupErr,
	)
}

func (session *Session) settleClassifiedRecoveredPublication(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
	attempt recoveryPublicationAttempt,
) (fileRecoveryActionResult, error) {
	publishDecision, err := resumestate.ReducePublishResult(state.resumable.Bound(), attempt.result)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	next, err := resumestate.ApplyRecoveryDecision(state.resumable.Bound(), publishDecision)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, state.resumable.Bound(), next); err != nil {
		return fileRecoveryActionResult{}, err
	}
	state.resumable, err = resumestate.BindResumableFile(next, file.Descriptor)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if attempt.linkErr != nil {
		if attempt.result != resumestate.PublishExistingAmbiguous &&
			classifyOutputV3RecoveryFailure(attempt.linkErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, recordDir, recordName, next, resumestate.QuarantinePublicationHistory,
				"close quarantined classified publication evidence", attempt.cleanupErr,
			))
		}
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"settle classified publication evidence", attempt.linkErr, attempt.cleanupErr,
		)
	}
	if attempt.cleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"close classified publication evidence", nil, attempt.cleanupErr,
		)
	}
	if attempt.result == resumestate.PublishExistingAmbiguous {
		return finishFileRecovery(session.quarantinedStart(
			file.Target, next.Record().LocatorDigest(), mapQuarantineReason(next.Record().QuarantineReason()),
		))
	}
	return finishFileRecovery(session.verifiedStart(transfer.FilePublishBlocked, state.resumable))
}

func (session *Session) syncRecoveredFinalParent(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	start, terminal, err := session.recoverFinalParentSync(
		file, recordDir, recordName, state.resumable.Bound(), "sync final parent",
	)
	if terminal {
		return fileRecoveryActionResult{start: start, terminal: true}, err
	}
	state.parentSynced = true
	return continuingFileRecovery(state), nil
}

func (session *Session) removeRecoveredPublishedStage(
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	operationErr, cleanupErr := session.removeStage(state.resumable.Bound().Record())
	if operationErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"remove published stage", operationErr, cleanupErr,
		)
	}
	if cleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"close removed published stage", nil, cleanupErr,
		)
	}
	return continuingFileRecovery(state), nil
}

func (session *Session) syncRecoveredPublishedStage(
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	operationErr, cleanupErr := session.syncObjectShard(
		session.stagesDir, resumestate.StageName(state.resumable.Bound().Record().OutputObject()),
	)
	if operationErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"sync published stage shard", operationErr, cleanupErr,
		)
	}
	if cleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOperationFault(
			"close synced published-stage shard", nil, cleanupErr,
		)
	}
	return finishFileRecovery(session.verifiedStart(transfer.FilePublished, state.resumable))
}

func (session *Session) holdRecoveredQuarantine(
	file transfer.OutputFile,
	state fileRecoveryState,
	observationCleanupErr error,
) (fileRecoveryActionResult, error) {
	if observationCleanupErr != nil {
		return fileRecoveryActionResult{}, pauseRequiredFileOutputFault(fileOutputFault(
			"close held file recovery observation", observationCleanupErr,
		))
	}
	record := state.resumable.Bound().Record()
	return finishFileRecovery(session.quarantinedStart(
		file.Target, record.LocatorDigest(), mapQuarantineReason(record.QuarantineReason()),
	))
}

func (session *Session) retireRecoveredFile(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	bound := state.resumable.Bound()
	binding, err := outputBindingForRecord(session.SessionID(), file.Descriptor, bound.Record())
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, _, cleanupErr := session.retireBoundFile(recordDir, recordName, bound, binding)
	if cleanupErr != nil {
		return fileRecoveryActionResult{}, cleanupErr
	}
	return finishFileRecovery(transfer.NewFileSettlementStart(settlement))
}

// A failed temporary observation is recoverable only when its corresponding
// record still proves which locator owns the namespace. An unclassifiable race
// is retained for later attention instead of turning a pathname into authority.
func (session *Session) reconcileFileShardObservationFailure(
	shard outputcap.Directory,
	classified resumestate.ClassifiedFileShardEntry,
	observeErr error,
) (bool, error) {
	if classified.Classification() != resumestate.FileShardEntryUpdateTemporary || classified.Locator().IsZero() {
		return false, fileOutputFault("observe file-shard recovery entry", observeErr)
	}
	targetName := resumestate.FileRecordName(classified.Locator())
	targetKind, targetErr := shard.ObserveEntry(targetName.Name())
	if targetErr != nil || targetKind != outputcap.EntryRegularFile {
		return true, nil
	}
	bound, recordCloseErr, bindErr := session.openBoundFileRecord(shard, targetName)
	if bindErr != nil {
		return true, nil
	}
	_, quarantineErr := session.installUnsafeNamespaceQuarantine(
		shard, targetName.Name(), bound, resumestate.QuarantineUpdateTemporary,
	)
	if quarantineErr != nil {
		if recordCloseErr != nil {
			return false, pauseRequiredFileOutputFault(fileOutputFault(
				"close update target after failed entry-race quarantine",
				errors.Join(quarantineErr, recordCloseErr),
			))
		}
		return false, quarantineErr
	}
	if recordCloseErr != nil {
		return false, pauseRequiredFileOutputFault(fileOutputFault(
			"close quarantined update target after entry race", recordCloseErr,
		))
	}
	return true, nil
}

func (session *Session) inspectUpdateTemporaryTarget(
	shard outputcap.Directory,
	classified resumestate.ClassifiedFileShardEntry,
	entryKind outputcap.EntryKind,
) (
	resumestate.UpdateTemporaryEntryObservation,
	resumestate.UpdateTargetObservation,
	resumestate.BoundFileRecord,
	error,
) {
	entry := resumestate.UpdateTemporaryEntryUnsafe
	switch entryKind {
	case outputcap.EntryAbsent:
		entry = resumestate.UpdateTemporaryEntryMissing
	case outputcap.EntryRegularFile:
		entry = resumestate.UpdateTemporaryEntryRegular
	}
	if classified.Locator().IsZero() {
		return entry, resumestate.UpdateTargetMissing, resumestate.BoundFileRecord{}, nil
	}
	targetName := resumestate.FileRecordName(classified.Locator())
	targetKind, err := shard.ObserveEntry(targetName.Name())
	if err != nil {
		return entry, resumestate.UpdateTargetMissing, resumestate.BoundFileRecord{}, fileOutputFault(
			"observe update target", err,
		)
	}
	if targetKind == outputcap.EntryAbsent {
		return entry, resumestate.UpdateTargetMissing, resumestate.BoundFileRecord{}, nil
	}
	if targetKind != outputcap.EntryRegularFile {
		return entry, resumestate.UpdateTargetInvalid, resumestate.BoundFileRecord{}, nil
	}
	bound, recordCloseErr, bindErr := session.openBoundFileRecord(shard, targetName)
	if bindErr != nil {
		return entry, resumestate.UpdateTargetInvalid, resumestate.BoundFileRecord{}, nil
	}
	if recordCloseErr != nil {
		return entry, resumestate.UpdateTargetValid, bound, pauseRequiredFileOutputFault(fileOutputFault(
			"close recovered file-state record", recordCloseErr,
		))
	}
	return entry, resumestate.UpdateTargetValid, bound, nil
}

func (session *Session) installUpdateTemporaryQuarantine(
	shard outputcap.Directory,
	classified resumestate.ClassifiedFileShardEntry,
	bound resumestate.BoundFileRecord,
	decision resumestate.UpdateTemporaryDecision,
) (bool, error) {
	quarantined, err := resumestate.ApplyUpdateTemporaryQuarantine(bound, decision)
	if err != nil {
		return false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if quarantined.Record().StateGeneration() == bound.Record().StateGeneration() {
		return true, nil
	}
	targetName := resumestate.FileRecordName(classified.Locator())
	if err := session.installFileRecord(shard, targetName.Name(), bound, quarantined); err != nil {
		return false, err
	}
	return true, nil
}

type updateTemporaryRecoveryContext struct {
	shard             outputcap.Directory
	classified        resumestate.ClassifiedFileShardEntry
	bound             resumestate.BoundFileRecord
	targetObservation resumestate.UpdateTargetObservation
}

func (session *Session) removeAndSyncUpdateTemporary(
	shardName string,
	shard outputcap.Directory,
	name string,
	classified resumestate.ClassifiedFileShardEntry,
	bound resumestate.BoundFileRecord,
	targetObservation resumestate.UpdateTargetObservation,
	decision resumestate.UpdateTemporaryDecision,
) (bool, error) {
	recovery := updateTemporaryRecoveryContext{shard, classified, bound, targetObservation}
	temporary, err := shard.OpenFile(name, true, false)
	if err != nil {
		return session.handleUpdateTemporaryOpenFailure(recovery, temporary, err)
	}
	if err := decision.AuthorizeRemoval(
		bound, shardName, name, resumestate.UpdateTemporaryEntryRegular,
	); err != nil {
		closeErr := temporary.Close()
		if closeErr != nil {
			return false, pauseRequiredFileOutputFault(fileOutputFault("close unauthorized recoverable update temporary", errors.Join(err, closeErr)))
		}
		return false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if removeErr := shard.RemoveFile(name, temporary); removeErr != nil {
		closeErr := temporary.Close()
		return session.handleUpdateTemporaryMutationFailure(
			recovery, removeErr, closeErr,
			"remove recoverable update temporary", "close quarantined recovered update removal",
		)
	}
	return session.finishUpdateTemporaryRemoval(recovery, temporary)
}

func (session *Session) handleUpdateTemporaryOpenFailure(
	recovery updateTemporaryRecoveryContext,
	temporary outputcap.File,
	openErr error,
) (bool, error) {
	closeErr := closeOutputV3File(temporary)
	if recovery.targetObservation != resumestate.UpdateTargetValid {
		fault := fileOutputFault("open recoverable update temporary", errors.Join(openErr, closeErr))
		if closeErr != nil {
			fault = pauseRequiredFileOutputFault(fault)
		}
		return false, fault
	}
	targetName := resumestate.FileRecordName(recovery.classified.Locator())
	_, quarantineErr := session.installUnsafeNamespaceQuarantine(
		recovery.shard, targetName.Name(), recovery.bound, resumestate.QuarantineUpdateTemporary,
	)
	if quarantineErr != nil {
		if closeErr != nil {
			return false, pauseRequiredFileOutputFault(fileOutputFault(
				"close ambiguous recovered update temporary", errors.Join(quarantineErr, openErr, closeErr),
			))
		}
		return false, quarantineErr
	}
	if closeErr != nil {
		return false, pauseRequiredFileOutputFault(fileOutputFault("close quarantined recovered update temporary", errors.Join(openErr, closeErr)))
	}
	return true, nil
}

func (session *Session) finishUpdateTemporaryRemoval(
	recovery updateTemporaryRecoveryContext,
	temporary outputcap.File,
) (bool, error) {
	syncErr := recovery.shard.Sync()
	closeErr := temporary.Close()
	if syncErr != nil {
		return session.handleUpdateTemporaryMutationFailure(
			recovery, syncErr, closeErr,
			"sync recoverable update temporary", "close quarantined recovered update sync",
		)
	}
	if closeErr != nil {
		return false, pauseRequiredFileOperationFault("close synced recoverable update temporary", nil, closeErr)
	}
	return false, nil
}

func (session *Session) handleUpdateTemporaryMutationFailure(
	recovery updateTemporaryRecoveryContext,
	operationErr error,
	closeErr error,
	operation string,
	closeOperation string,
) (bool, error) {
	if recovery.targetObservation == resumestate.UpdateTargetValid &&
		classifyOutputV3RecoveryFailure(operationErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
		return session.quarantineAmbiguousUpdateTemporary(recovery, closeOperation, closeErr)
	}
	return false, pauseRequiredFileOperationFault(operation, operationErr, closeErr)
}

func (session *Session) quarantineAmbiguousUpdateTemporary(
	recovery updateTemporaryRecoveryContext,
	closeOperation string,
	closeErr error,
) (bool, error) {
	targetName := resumestate.FileRecordName(recovery.classified.Locator())
	_, quarantineErr := session.installUnsafeNamespaceQuarantine(
		recovery.shard, targetName.Name(), recovery.bound, resumestate.QuarantineUpdateTemporary,
	)
	if quarantineErr != nil {
		if closeErr != nil {
			return false, errors.Join(
				quarantineErr,
				pauseRequiredFileOperationFault(closeOperation, nil, closeErr),
			)
		}
		return false, quarantineErr
	}
	if closeErr != nil {
		return false, pauseRequiredFileOperationFault(closeOperation, nil, closeErr)
	}
	return true, nil
}
