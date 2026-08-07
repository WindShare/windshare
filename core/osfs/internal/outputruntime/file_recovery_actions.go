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
	state fileRecoveryState,
	iteration fileRecoveryIteration,
) (fileRecoveryActionResult, error) {
	switch iteration.decision.Action() {
	case resumestate.RecoveryRetryObjectCreation:
		return session.retryFileWitness(file, state)
	case resumestate.RecoveryInstallWitness, resumestate.RecoveryInstallPublishing,
		resumestate.RecoveryInstallPublished, resumestate.RecoveryInstallPublishBlocked,
		resumestate.RecoveryInstallRetiring, resumestate.RecoveryInstallQuarantine:
		return session.installFileRecoveryDecision(file, state, iteration)
	case resumestate.RecoveryResumeContent:
		return finishFileRecovery(session.transactionStart(file.Descriptor, state.resumable))
	case resumestate.RecoveryLinkFinalNoReplace:
		return session.linkRecoveredFinal(file, state)
	case resumestate.RecoverySyncFinalParent:
		return session.syncRecoveredFinalParent(file, state)
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
		return session.retireRecoveredFile(file, state)
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
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	operationErr, cleanupErr := session.createWitnessObject(state.resumable.BoundState().State())
	if operationErr != nil {
		if classifyNativeRecoveryFailure(operationErr, nativeAuthorizedMutation) == nativeRecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, state.resumable.BoundState(), resumestate.QuarantinePartialObjectCreation,
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
	state fileRecoveryState,
	iteration fileRecoveryIteration,
) (fileRecoveryActionResult, error) {
	next, err := resumestate.ApplyCheckpointRuntimeRecoveryDecision(state.resumable.BoundState(), iteration.decision)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installCheckpointRuntimeState(state.resumable.BoundState(), next); err != nil {
		return fileRecoveryActionResult{}, err
	}
	state.resumable, err = resumestate.BindCheckpointRuntimeDescriptor(next, file.Descriptor)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	return session.finishInstalledFileRecovery(file, state, iteration)
}

func (session *Session) finishInstalledFileRecovery(
	file transfer.OutputFile,
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
			file.Target, state.resumable.BoundState().State().LocatorDigest(),
			mapQuarantineReason(iteration.decision.QuarantineReason()),
		))
	case resumestate.RecoveryInstallRetiring:
		return session.finishInstalledRetirement(file, state, iteration.decision)
	default:
		return continuingFileRecovery(state), nil
	}
}

func (session *Session) finishInstalledRetirement(
	file transfer.OutputFile,
	state fileRecoveryState,
	decision resumestate.RecoveryDecision,
) (fileRecoveryActionResult, error) {
	bound := state.resumable.BoundState()
	binding, err := outputBindingForRuntimeState(session.SessionID(), file.Descriptor, bound.State())
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	settlement, _, cleanupErr := session.retireBoundFile(bound, binding)
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
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	// The retained witness keeps publication anchored to the selected object;
	// the fixed names are revalidated immediately before the no-replace link.
	witness, witnessErr, witnessCleanupErr := session.openPublicationWitness(
		state.resumable.BoundState().State(), anchorWitness{},
	)
	if witnessErr != nil {
		if classifyNativeRecoveryFailure(witnessErr, nativeExistingEntryUnclassified) == nativeRecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, state.resumable.BoundState(), resumestate.QuarantinePublicationHistory,
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
	attempt := session.attemptRecoveredPublication(state.resumable.BoundState(), witness)
	if result, handled, err := session.handleUnclassifiedRecoveredPublication(
		file, state, attempt,
	); handled {
		return result, err
	}
	if attempt.result != resumestate.PublishLinkCreated {
		return session.settleClassifiedRecoveredPublication(file, state, attempt)
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
	bound resumestate.BoundCheckpointRuntimeState,
	witness *publicationWitness,
) recoveryPublicationAttempt {
	result, linkErr, cleanupErr := session.linkFinalNoReplaceResult(bound, witness)
	return recoveryPublicationAttempt{
		result: result, linkErr: linkErr, cleanupErr: errors.Join(cleanupErr, witness.Close()),
	}
}

func (session *Session) handleUnclassifiedRecoveredPublication(
	file transfer.OutputFile,
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
			file, state.resumable.BoundState(), resumestate.QuarantineAnchorUnsafe,
			"close invalidated recovery publication witness", attempt.cleanupErr,
		))
		return result, true, err
	}
	if attempt.linkErr != nil {
		if classifyNativeRecoveryFailure(attempt.linkErr, nativeAuthorizedMutation) == nativeRecoveryAmbiguous {
			result, err := finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, state.resumable.BoundState(), resumestate.QuarantinePublicationHistory,
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
	state fileRecoveryState,
	attempt recoveryPublicationAttempt,
) (fileRecoveryActionResult, error) {
	publishDecision, err := resumestate.ReduceCheckpointRuntimePublishResult(state.resumable.BoundState(), attempt.result)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	next, err := resumestate.ApplyCheckpointRuntimeRecoveryDecision(state.resumable.BoundState(), publishDecision)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installCheckpointRuntimeState(state.resumable.BoundState(), next); err != nil {
		return fileRecoveryActionResult{}, err
	}
	state.resumable, err = resumestate.BindCheckpointRuntimeDescriptor(next, file.Descriptor)
	if err != nil {
		return fileRecoveryActionResult{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if attempt.linkErr != nil {
		if attempt.result != resumestate.PublishExistingAmbiguous &&
			classifyNativeRecoveryFailure(attempt.linkErr, nativeAuthorizedMutation) == nativeRecoveryAmbiguous {
			return finishFileRecovery(session.quarantineRecoveryStartWithCleanup(
				file, next, resumestate.QuarantinePublicationHistory,
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
			file.Target, next.State().LocatorDigest(), mapQuarantineReason(next.State().QuarantineReason()),
		))
	}
	return finishFileRecovery(session.verifiedStart(transfer.FilePublishBlocked, state.resumable))
}

func (session *Session) syncRecoveredFinalParent(
	file transfer.OutputFile,
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	start, terminal, err := session.recoverFinalParentSync(
		file, state.resumable.BoundState(), "sync final parent",
	)
	if terminal {
		return fileRecoveryActionResult{start: start, terminal: true}, err
	}
	state.parentSynced = true
	return continuingFileRecovery(state), nil
}
