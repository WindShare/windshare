package outputruntime

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

type fileRetirementStep struct {
	settlement  transfer.FileSettlement
	quarantined bool
	complete    bool
}

func (session *Session) decideFileRetirement(
	validation *outputAncestryValidation,
	bound resumestate.BoundCheckpointRuntimeState,
) (resumestate.RecoveryDecision, error, error) {
	observation, cleanupErr, err := session.observeFile(validation, bound.State(), false)
	if err != nil {
		return resumestate.RecoveryDecision{}, nil, pauseRequiredFileOutputFault(fileOutputFault(
			"observe file retirement", errors.Join(err, cleanupErr),
		))
	}
	decision, err := resumestate.ReduceCheckpointRuntimeStateRecovery(bound, observation)
	if err != nil {
		return resumestate.RecoveryDecision{}, cleanupErr, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	return decision, cleanupErr, nil
}

func (session *Session) traceFileRetirementDecision(
	bound resumestate.BoundCheckpointRuntimeState,
	decision resumestate.RecoveryDecision,
) {
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFileRecoveryDecision, IntentDigest: session.intentDigest,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(bound.State().LocatorDigest()),
		OutputObjectID:   outputObjectIdentityFromState(bound.State().OutputObject()),
		PreviousPhase:    filesystemOutputFilePhaseFromState(bound.State().Phase()),
		RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
		QuarantineReason: recoveryDecisionQuarantineReason(decision),
	})
}

func fileRetirementObservationCleanupFault(
	decision resumestate.RecoveryDecision,
	cleanupErr error,
) error {
	quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
		decision.Action() == resumestate.RecoveryHoldQuarantine
	if cleanupErr == nil || quarantineDecision {
		return nil
	}
	return pauseRequiredFileOutputFault(fileOutputFault(
		"close retiring-file observation", cleanupErr,
	))
}

func (session *Session) applyFileRetirementDecision(
	bound resumestate.BoundCheckpointRuntimeState,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (fileRetirementStep, error) {
	switch decision.Action() {
	case resumestate.RecoveryRemoveRetiringStageAndSync:
		return fileRetirementStep{}, session.removeRetiringStage(bound.State())
	case resumestate.RecoverySyncStageRemoveAnchorAndSync:
		return fileRetirementStep{}, session.syncRetiringStageAndRemoveAnchor(bound.State())
	case resumestate.RecoverySyncParentsRemoveRecordAndSync:
		return session.finishRetiringRecord(bound, binding, decision)
	case resumestate.RecoveryHoldRetiringCleanup:
		return fileRetirementStep{complete: true}, internalCleanupNeedsAttentionFault(
			"hold retiring file with ambiguous internal cleanup evidence",
		)
	case resumestate.RecoveryInstallQuarantine:
		return session.installRetirementQuarantine(
			bound, binding, decision, observationCleanupErr,
		)
	case resumestate.RecoveryHoldQuarantine:
		return heldRetirementQuarantine(binding, bound.State(), observationCleanupErr)
	default:
		return fileRetirementStep{complete: true}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract,
			fmt.Errorf("unexpected retirement action %d", decision.Action()),
		)
	}
}

func (session *Session) removeRetiringStage(record resumestate.CheckpointRuntimeState) error {
	operationErr, cleanupErr := session.removeStage(record)
	if operationErr != nil {
		return pauseRequiredFileOperationFault("remove retiring stage", operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault("close removed retiring stage", nil, cleanupErr)
	}
	return nil
}

func (session *Session) syncRetiringStageAndRemoveAnchor(record resumestate.CheckpointRuntimeState) error {
	stageName := resumestate.StageName(record.OutputObject())
	operationErr, cleanupErr := session.syncObjectShard(session.stagesDir, stageName)
	if operationErr != nil {
		return pauseRequiredFileOperationFault("sync retiring stage shard", operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault("close synced retiring-stage shard", nil, cleanupErr)
	}
	return session.removeRetiringAnchor(record)
}

func (session *Session) removeRetiringAnchor(record resumestate.CheckpointRuntimeState) error {
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchorDir, present, openErr := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if openErr != nil || !present {
		operationErr := openErr
		if !present {
			operationErr = errors.Join(outputcap.ErrUnsafeNamespace, fs.ErrNotExist)
		}
		return pauseRequiredFileOperationFault(
			"open retiring anchor shard", operationErr, closeOutputDirectory(anchorDir),
		)
	}
	anchor, openErr := anchorDir.OpenFile(anchorName.Name(), true, false)
	if openErr != nil {
		return pauseRequiredFileOperationFault(
			"open retiring anchor", openErr,
			errors.Join(closeOutputFile(anchor), anchorDir.Close()),
		)
	}
	removeErr := anchorDir.RemoveFile(anchorName.Name(), anchor)
	if removeErr == nil {
		removeErr = anchorDir.Sync()
	}
	closeErr := errors.Join(anchor.Close(), anchorDir.Close())
	if removeErr != nil {
		return pauseRequiredFileOperationFault("remove retiring anchor", removeErr, closeErr)
	}
	if closeErr != nil {
		return pauseRequiredFileOperationFault("close removed retiring anchor", nil, closeErr)
	}
	return nil
}

func (session *Session) finishRetiringRecord(
	bound resumestate.BoundCheckpointRuntimeState,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
) (fileRetirementStep, error) {
	if err := session.syncRetirementParent(
		session.stagesDir, resumestate.StageName(bound.State().OutputObject()),
		"sync retiring stage parent", "close synced retiring-stage parent",
	); err != nil {
		return fileRetirementStep{}, err
	}
	if err := session.syncRetirementParent(
		session.anchorsDir, resumestate.AnchorName(bound.State().OutputObject()),
		"sync retiring anchor parent", "close synced retiring-anchor parent",
	); err != nil {
		return fileRetirementStep{}, err
	}
	if err := session.removeRetiringFileCheckpoint(bound); err != nil {
		return fileRetirementStep{}, err
	}
	settlement, err := retiredFileSettlement(binding, decision)
	return fileRetirementStep{settlement: settlement, complete: true}, err
}

func (session *Session) syncRetirementParent(
	parent outputcap.Directory,
	name resumestate.ShardedName,
	operation string,
	cleanupOperation string,
) error {
	operationErr, cleanupErr := session.syncObjectShard(parent, name)
	if operationErr != nil {
		return pauseRequiredFileOperationFault(operation, operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault(cleanupOperation, nil, cleanupErr)
	}
	return nil
}

func (session *Session) removeRetiringFileCheckpoint(
	bound resumestate.BoundCheckpointRuntimeState,
) error {
	operationErr, cleanupErr := session.removeIncrementalCheckpoint(bound.State())
	if operationErr != nil {
		operation := "remove retiring FileCheckpointV1"
		if classifyNativeRecoveryFailure(operationErr, nativeAuthorizedMutation) == nativeRecoveryAmbiguous {
			session.poisonState()
			operation = "remove retiring FileCheckpointV1 with uncertain authority"
		}
		return pauseRequiredFileOperationFault(operation, operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault("close removed retiring FileCheckpointV1", nil, cleanupErr)
	}
	return nil
}

func retiredFileSettlement(
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
) (transfer.FileSettlement, error) {
	if binding.BackendID() == "" {
		return transfer.FileSettlement{}, nil
	}
	var settlement transfer.FileSettlement
	var err error
	if decision.Settlement() == resumestate.RecoveryCollision {
		settlement, err = transfer.NewCollisionFileSettlement(binding.Target())
	} else {
		settlement, err = transfer.NewRetiredFileSettlement(binding)
	}
	if err != nil {
		return transfer.FileSettlement{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	return settlement, nil
}

func (session *Session) installRetirementQuarantine(
	bound resumestate.BoundCheckpointRuntimeState,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (fileRetirementStep, error) {
	step := fileRetirementStep{quarantined: true, complete: true}
	next, err := resumestate.ApplyCheckpointRuntimeRecoveryDecision(bound, decision)
	if err != nil {
		return fileRetirementStep{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	if err := session.installCheckpointRuntimeState(bound, next); err != nil {
		return fileRetirementStep{}, err
	}
	if observationCleanupErr != nil {
		return step, pauseRequiredFileOutputFault(fileOutputFault(
			"close quarantined retirement observation", observationCleanupErr,
		))
	}
	if binding.BackendID() == "" {
		return step, nil
	}
	step.settlement, err = quarantinedSettlement(binding, next.State())
	return step, err
}

func heldRetirementQuarantine(
	binding transfer.OutputFileBinding,
	record resumestate.CheckpointRuntimeState,
	observationCleanupErr error,
) (fileRetirementStep, error) {
	step := fileRetirementStep{quarantined: true, complete: true}
	if observationCleanupErr != nil {
		return step, pauseRequiredFileOutputFault(fileOutputFault(
			"close held retirement observation", observationCleanupErr,
		))
	}
	if binding.BackendID() == "" {
		return step, nil
	}
	settlement, err := quarantinedSettlement(binding, record)
	step.settlement = settlement
	return step, err
}

func (session *Session) removeRecoveredPublishedStage(
	state fileRecoveryState,
) (fileRecoveryActionResult, error) {
	operationErr, cleanupErr := session.removeStage(state.resumable.BoundState().State())
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
		session.stagesDir, resumestate.StageName(state.resumable.BoundState().State().OutputObject()),
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
	record := state.resumable.BoundState().State()
	return finishFileRecovery(session.quarantinedStart(
		file.Target, record.LocatorDigest(), mapQuarantineReason(record.QuarantineReason()),
	))
}

func (session *Session) retireRecoveredFile(
	file transfer.OutputFile,
	state fileRecoveryState,
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
	return finishFileRecovery(transfer.NewFileSettlementStart(settlement))
}
