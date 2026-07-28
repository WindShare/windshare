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
	bound resumestate.BoundFileRecord,
) (resumestate.RecoveryDecision, error, error) {
	observation, cleanupErr, err := session.observeFile(validation, bound.Record(), false)
	if err != nil {
		return resumestate.RecoveryDecision{}, nil, pauseRequiredFileOutputFault(fileOutputFault(
			"observe file retirement", errors.Join(err, cleanupErr),
		))
	}
	decision, err := resumestate.ReduceFileRecovery(bound, observation)
	if err != nil {
		return resumestate.RecoveryDecision{}, cleanupErr, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	return decision, cleanupErr, nil
}

func (session *Session) traceFileRetirementDecision(
	bound resumestate.BoundFileRecord,
	decision resumestate.RecoveryDecision,
) {
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(bound.Record().LocatorDigest()),
		OutputObjectID:   outputObjectIdentityFromState(bound.Record().OutputObject()),
		PreviousPhase:    filesystemOutputFilePhaseFromState(bound.Record().Phase()),
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
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (fileRetirementStep, error) {
	switch decision.Action() {
	case resumestate.RecoveryRemoveRetiringStageAndSync:
		return fileRetirementStep{}, session.removeRetiringStage(bound.Record())
	case resumestate.RecoverySyncStageRemoveAnchorAndSync:
		return fileRetirementStep{}, session.syncRetiringStageAndRemoveAnchor(bound.Record())
	case resumestate.RecoverySyncParentsRemoveRecordAndSync:
		return session.finishRetiringRecord(recordDir, recordName, bound, binding, decision)
	case resumestate.RecoveryHoldRetiringCleanup:
		return fileRetirementStep{complete: true}, internalCleanupNeedsAttentionFault(
			"hold retiring file with ambiguous internal cleanup evidence",
		)
	case resumestate.RecoveryInstallQuarantine:
		return session.installRetirementQuarantine(
			recordDir, recordName, bound, binding, decision, observationCleanupErr,
		)
	case resumestate.RecoveryHoldQuarantine:
		return heldRetirementQuarantine(binding, bound.Record(), observationCleanupErr)
	default:
		return fileRetirementStep{complete: true}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract,
			fmt.Errorf("unexpected retirement action %d", decision.Action()),
		)
	}
}

func (session *Session) removeRetiringStage(record resumestate.FileRecord) error {
	operationErr, cleanupErr := session.removeStage(record)
	if operationErr != nil {
		return pauseRequiredFileOperationFault("remove retiring stage", operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault("close removed retiring stage", nil, cleanupErr)
	}
	return nil
}

func (session *Session) syncRetiringStageAndRemoveAnchor(record resumestate.FileRecord) error {
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

func (session *Session) removeRetiringAnchor(record resumestate.FileRecord) error {
	anchorName := resumestate.AnchorName(record.OutputObject())
	anchorDir, present, openErr := openOutputShard(session.anchorsDir, anchorName.Shard(), false)
	if openErr != nil || !present {
		operationErr := openErr
		if !present {
			operationErr = errors.Join(outputcap.ErrUnsafeNamespace, fs.ErrNotExist)
		}
		return pauseRequiredFileOperationFault(
			"open retiring anchor shard", operationErr, closeOutputV3Directory(anchorDir),
		)
	}
	anchor, openErr := anchorDir.OpenFile(anchorName.Name(), true, false)
	if openErr != nil {
		return pauseRequiredFileOperationFault(
			"open retiring anchor", openErr,
			errors.Join(closeOutputV3File(anchor), anchorDir.Close()),
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
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
) (fileRetirementStep, error) {
	if err := session.syncRetirementParent(
		session.stagesDir, resumestate.StageName(bound.Record().OutputObject()),
		"sync retiring stage parent", "close synced retiring-stage parent",
	); err != nil {
		return fileRetirementStep{}, err
	}
	if err := session.syncRetirementParent(
		session.anchorsDir, resumestate.AnchorName(bound.Record().OutputObject()),
		"sync retiring anchor parent", "close synced retiring-anchor parent",
	); err != nil {
		return fileRetirementStep{}, err
	}
	if err := session.removeRetiringFileRecord(recordDir, recordName, bound); err != nil {
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

func (session *Session) removeRetiringFileRecord(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) error {
	operationErr, cleanupErr := removeBoundFileRecord(recordDir, recordName, bound)
	if operationErr != nil {
		operation := "remove retiring file state"
		if classifyOutputV3RecoveryFailure(operationErr, outputV3AuthorizedMutation) == outputV3RecoveryAmbiguous {
			session.poisonState()
			operation = "remove retiring file state with uncertain authority"
		}
		return pauseRequiredFileOperationFault(operation, operationErr, cleanupErr)
	}
	if cleanupErr != nil {
		return pauseRequiredFileOperationFault("close removed retiring file state", nil, cleanupErr)
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
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	binding transfer.OutputFileBinding,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (fileRetirementStep, error) {
	step := fileRetirementStep{quarantined: true, complete: true}
	next, err := resumestate.ApplyRecoveryDecision(bound, decision)
	if err != nil {
		return fileRetirementStep{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, next); err != nil {
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
	step.settlement, err = quarantinedSettlement(binding, next.Record())
	return step, err
}

func heldRetirementQuarantine(
	binding transfer.OutputFileBinding,
	record resumestate.FileRecord,
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
