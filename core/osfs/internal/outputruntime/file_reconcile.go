package outputruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) openBoundFileRecord(
	shard outputcap.Directory,
	name resumestate.ShardedName,
) (resumestate.BoundFileRecord, error, error) {
	readResult := outputnamespace.ReadRecordWithCleanup(shard, name.Name(), resumestate.MaxFileStateBytes)
	encoded, readErr, closeErr := readResult.Encoded, readResult.ReadError, readResult.CloseError
	if readErr != nil {
		return resumestate.BoundFileRecord{}, closeErr, readErr
	}
	record, err := resumestate.DecodeFileRecord(encoded)
	if err != nil {
		return resumestate.BoundFileRecord{}, closeErr, errors.Join(outputfault.ErrIntentUnsafe, err)
	}
	if record.LocatorDigest() != resumestate.DigestCanonicalLocator(record.CanonicalLocator()) {
		return resumestate.BoundFileRecord{}, closeErr, outputfault.ErrIntentUnsafe
	}
	bound, bindErr := resumestate.BindFileRecord(session.stateSnapshot(), name.Shard(), name.Name(), record)
	return bound, closeErr, bindErr
}

func (session *Session) reconcileFileShardUpdates(
	shardName string,
	shard outputcap.Directory,
	names []string,
) (resultAttention bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, resumestate.LocatorDigest{}, err)
		return false, outputAncestryOperationFault("validate ancestry before file-state recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			resumestate.LocatorDigest{}, "finish file-state recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultAttention = false
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	attention := false
	for _, name := range names {
		classified := resumestate.ClassifyFileShardEntry(shardName, name)
		if classified.Classification() == resumestate.FileShardEntryRecord {
			continue
		}
		entryKind, observeErr := shard.ObserveEntry(name)
		if observeErr != nil {
			entryAttention, err := session.reconcileFileShardObservationFailure(shard, classified, observeErr)
			if err != nil {
				return false, err
			}
			attention = attention || entryAttention
			continue
		}
		entry, targetObservation, bound, err := session.inspectUpdateTemporaryTarget(shard, classified, entryKind)
		if err != nil {
			return false, err
		}
		decision, err := resumestate.ReduceUpdateTemporary(
			session.stateSnapshot().NamespaceAuthority(), classified, entry, targetObservation,
		)
		if err != nil {
			return false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		switch decision.Action() {
		case resumestate.UpdateTemporaryAcceptInstalledTarget:
			continue
		case resumestate.UpdateTemporaryRemoveAndSyncShard:
			entryAttention, err := session.removeAndSyncUpdateTemporary(
				shardName, shard, name, classified, bound, targetObservation, decision,
			)
			if err != nil {
				return false, err
			}
			attention = attention || entryAttention
		case resumestate.UpdateTemporaryInstallFileQuarantine:
			entryAttention, err := session.installUpdateTemporaryQuarantine(shard, classified, bound, decision)
			if err != nil {
				return false, err
			}
			attention = attention || entryAttention
		default:
			attention = true
		}
	}
	return attention, nil
}

func (session *Session) claimFileStart(digest resumestate.LocatorDigest) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.settling || session.poisoned {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrSessionClosed)
	}
	if _, exists := session.active[digest]; exists {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrFileActive)
	}
	if _, exists := session.beginning[digest]; exists {
		return outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, outputfault.ErrFileActive)
	}
	if len(session.active)+len(session.beginning) >= maxFilesystemOutputTransactions {
		return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultOwnership, outputfault.ErrTransactionLimit)
	}
	session.beginning[digest] = struct{}{}
	session.beginWG.Add(1)
	return nil
}

func (session *Session) releaseFileStart(digest resumestate.LocatorDigest) {
	session.mu.Lock()
	delete(session.beginning, digest)
	session.mu.Unlock()
	session.beginWG.Done()
}

func (session *Session) allocateOutputObjectID(
	digest resumestate.LocatorDigest,
) (resumestate.OutputObjectID, error) {
	for range outputnamespace.AllocationAttempts {
		objectID, err := session.owner.objectIDs.NewOutputObjectID()
		if err != nil {
			return resumestate.OutputObjectID{}, err
		}
		if objectID.IsZero() {
			continue
		}
		session.mu.Lock()
		_, claimed := session.objectClaims[objectID]
		if !claimed {
			session.objectClaims[objectID] = digest
		}
		session.mu.Unlock()
		if claimed {
			continue
		}
		occupied, err := session.outputObjectNameOccupied(objectID)
		if err != nil {
			session.releaseOutputObjectClaim(objectID, digest)
			return resumestate.OutputObjectID{}, err
		}
		if !occupied {
			return objectID, nil
		}
		session.releaseOutputObjectClaim(objectID, digest)
	}
	return resumestate.OutputObjectID{}, fmt.Errorf("%w: allocate unique output object", outputcap.ErrUnsafeNamespace)
}

func (session *Session) releaseOutputObjectClaim(
	objectID resumestate.OutputObjectID,
	digest resumestate.LocatorDigest,
) {
	session.mu.Lock()
	if session.objectClaims[objectID] == digest {
		delete(session.objectClaims, objectID)
	}
	session.mu.Unlock()
}

func (session *Session) outputObjectNameOccupied(id resumestate.OutputObjectID) (bool, error) {
	for _, candidate := range []struct {
		parent outputcap.Directory
		name   resumestate.ShardedName
	}{
		{session.anchorsDir, resumestate.AnchorName(id)},
		{session.stagesDir, resumestate.StageName(id)},
	} {
		shard, present, err := openOutputShard(candidate.parent, candidate.name.Shard(), false)
		if err != nil {
			return false, err
		}
		if !present {
			continue
		}
		kind, observeErr := shard.ObserveEntry(candidate.name.Name())
		closeErr := shard.Close()
		if observeErr != nil || closeErr != nil {
			return false, errors.Join(observeErr, closeErr)
		}
		if kind != outputcap.EntryAbsent {
			return true, nil
		}
	}
	return false, nil
}

func (session *Session) resumeFile(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (transfer.FileStart, bool, error) {
	record := bound.Record()
	name := resumestate.FileRecordName(record.LocatorDigest())
	if name.Name() != recordName || record.CanonicalLocator() != file.Path {
		start, quarantineErr := session.quarantinedStart(
			file.Target, resumestate.DigestCanonicalLocator(file.Path), transfer.QuarantineOwnershipMismatch,
		)
		return start, true, quarantineErr
	}
	resumable, err := resumestate.BindResumableFile(bound, file.Descriptor)
	if err != nil {
		if !outputV3RevisionOnlyMismatch(record, file.Descriptor) {
			return transfer.FileStart{}, true, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultOwnership, err)
		}
		return session.replaceInvalidatedRevision(ctx, file, recordDir, recordName, bound)
	}
	start, reduceErr := session.reduceFile(ctx, file, resumable, recordDir, recordName)
	return start, true, reduceErr
}

func outputV3RevisionOnlyMismatch(
	record resumestate.FileRecord,
	descriptor content.FileRevisionDescriptor,
) bool {
	return descriptor.FileRevision() != record.Revision() &&
		descriptor.ShareInstance() == record.ShareInstance() && descriptor.FileID() == record.FileID() &&
		descriptor.ExactSize() == record.ExactSize() &&
		descriptor.Geometry().ChunkSize() == record.ChunkSize() &&
		descriptor.ModifiedTime() == record.ExpectedMetadata().ModifiedTime
}

func (session *Session) replaceInvalidatedRevision(
	ctx context.Context,
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (resultStart transfer.FileStart, resultHandled bool, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, bound.Record().LocatorDigest(), err)
		return transfer.FileStart{}, true,
			outputAncestryOperationFault("validate ancestry before invalidated-revision recovery", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			bound.Record().LocatorDigest(), "finish invalidated-revision recovery ancestry", nil,
		)
		if ancestryErr != nil {
			resultStart = transfer.FileStart{}
			resultHandled = true
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	parentSynced := false
	for {
		if err := ctx.Err(); err != nil {
			return transfer.FileStart{}, true, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultStateIO, err)
		}
		if bound.Record().Phase() == resumestate.FileRetiring {
			return session.finishRetiringInvalidatedRevision(file, recordDir, recordName, bound)
		}

		decision, observationCleanupErr, err := session.inspectInvalidatedRevision(
			validation, bound, parentSynced,
		)
		if err != nil {
			return transfer.FileStart{}, true, err
		}

		step, err := session.applyInvalidatedRevisionDecision(
			file, recordDir, recordName, bound, decision, observationCleanupErr,
		)
		if err != nil {
			return transfer.FileStart{}, true, err
		}
		if step.done {
			return step.start, step.handled, nil
		}
		bound = step.bound
		parentSynced = step.parentSynced
	}
}

type invalidatedRevisionStep struct {
	bound        resumestate.BoundFileRecord
	parentSynced bool
	start        transfer.FileStart
	done         bool
	handled      bool
}

func (session *Session) inspectInvalidatedRevision(
	validation *outputAncestryValidation,
	bound resumestate.BoundFileRecord,
	parentSynced bool,
) (resumestate.RecoveryDecision, error, error) {
	observation, observationCleanupErr, observationErr := session.observeFile(
		validation, bound.Record(), parentSynced,
	)
	if observationErr != nil {
		return resumestate.RecoveryDecision{}, nil, pauseRequiredFileOutputFault(fileOutputFault(
			"observe invalidated revision", errors.Join(observationErr, observationCleanupErr),
		))
	}
	decision, err := resumestate.ReduceFileRecovery(bound, observation)
	if err != nil {
		return resumestate.RecoveryDecision{}, nil, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
		decision.Action() == resumestate.RecoveryHoldQuarantine
	if observationCleanupErr != nil && !quarantineDecision {
		return resumestate.RecoveryDecision{}, nil, pauseRequiredFileOutputFault(fileOutputFault(
			"close invalidated-revision observation", observationCleanupErr,
		))
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceFileRecoveryDecision, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), LocatorDigest: outputLocatorDigestFromState(bound.Record().LocatorDigest()),
		OutputObjectID:   outputObjectIdentityFromState(bound.Record().OutputObject()),
		PreviousPhase:    filesystemOutputFilePhaseFromState(bound.Record().Phase()),
		RecoveryAction:   filesystemOutputRecoveryActionFromState(decision.Action()),
		QuarantineReason: recoveryDecisionQuarantineReason(decision),
	})
	return decision, observationCleanupErr, nil
}

func (session *Session) applyInvalidatedRevisionDecision(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (invalidatedRevisionStep, error) {
	switch decision.Action() {
	case resumestate.RecoveryInstallQuarantine:
		return session.installInvalidatedRevisionQuarantine(
			file, recordDir, recordName, bound, decision, observationCleanupErr,
		)
	case resumestate.RecoveryHoldQuarantine:
		return session.holdInvalidatedRevisionQuarantine(file, bound, observationCleanupErr)
	case resumestate.RecoverySyncFinalParent:
		return session.syncInvalidatedRevisionFinalParent(file, recordDir, recordName, bound)
	case resumestate.RecoveryInstallPublished:
		return session.installInvalidatedRevisionPublished(recordDir, recordName, bound, decision)
	case resumestate.RecoveryHoldPublishedCleanup:
		return invalidatedRevisionStep{}, internalCleanupNeedsAttentionFault(
			"hold invalidated published file with ambiguous internal cleanup evidence",
		)
	case resumestate.RecoveryRemovePublishedStageAndSync, resumestate.RecoverySyncPublishedStageParent:
		return session.retireInvalidatedPublishedRevision(file, recordDir, recordName, bound)
	case resumestate.RecoveryRetryObjectCreation, resumestate.RecoveryInstallWitness,
		resumestate.RecoveryRequireRevisionBinding, resumestate.RecoveryInstallPublishing,
		resumestate.RecoveryHoldPublishBlocked, resumestate.RecoveryLinkFinalNoReplace,
		resumestate.RecoveryInstallRetiring:
		return session.installInvalidatedRevisionRetirementStep(file, recordDir, recordName, bound)
	default:
		return invalidatedRevisionStep{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract,
			fmt.Errorf("unexpected invalidated-revision recovery action %d", decision.Action()),
		)
	}
}

func (session *Session) installInvalidatedRevisionQuarantine(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (invalidatedRevisionStep, error) {
	quarantined, err := resumestate.ApplyRecoveryDecision(bound, decision)
	if err != nil {
		return invalidatedRevisionStep{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
		return invalidatedRevisionStep{}, err
	}
	if observationCleanupErr != nil {
		return invalidatedRevisionStep{}, pauseRequiredFileOutputFault(fileOutputFault(
			"close quarantined invalidated-revision observation", observationCleanupErr,
		))
	}
	start, err := session.quarantinedStart(
		file.Target, quarantined.Record().LocatorDigest(),
		mapQuarantineReason(quarantined.Record().QuarantineReason()),
	)
	return invalidatedRevisionStep{start: start, done: true, handled: true}, err
}

func (session *Session) holdInvalidatedRevisionQuarantine(
	file transfer.OutputFile,
	bound resumestate.BoundFileRecord,
	observationCleanupErr error,
) (invalidatedRevisionStep, error) {
	if observationCleanupErr != nil {
		return invalidatedRevisionStep{}, pauseRequiredFileOutputFault(fileOutputFault(
			"close held invalidated-revision observation", observationCleanupErr,
		))
	}
	start, err := session.quarantinedStart(
		file.Target, bound.Record().LocatorDigest(), mapQuarantineReason(bound.Record().QuarantineReason()),
	)
	return invalidatedRevisionStep{start: start, done: true, handled: true}, err
}

func (session *Session) syncInvalidatedRevisionFinalParent(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (invalidatedRevisionStep, error) {
	start, terminal, err := session.recoverFinalParentSync(
		file, recordDir, recordName, bound, "sync invalidated revision final parent",
	)
	if err != nil {
		return invalidatedRevisionStep{}, err
	}
	if terminal {
		return invalidatedRevisionStep{start: start, done: true, handled: true}, nil
	}
	return invalidatedRevisionStep{bound: bound, parentSynced: true}, nil
}

func (session *Session) installInvalidatedRevisionPublished(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	decision resumestate.RecoveryDecision,
) (invalidatedRevisionStep, error) {
	published, err := resumestate.ApplyRecoveryDecision(bound, decision)
	if err != nil {
		return invalidatedRevisionStep{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, published); err != nil {
		return invalidatedRevisionStep{}, err
	}
	return invalidatedRevisionStep{bound: published}, nil
}

func (session *Session) retireInvalidatedPublishedRevision(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (invalidatedRevisionStep, error) {
	retirement, err := session.authorizePublishedRetirement(recordDir, recordName, bound)
	if err != nil {
		return invalidatedRevisionStep{}, err
	}
	switch retirement.disposition {
	case publishedRetirementAuthorized:
		return session.installInvalidatedRevisionRetirementStep(file, recordDir, recordName, bound)
	case publishedRetirementHoldPreserve:
		return invalidatedRevisionStep{}, internalCleanupNeedsAttentionFault(
			"hold invalidated published file after retirement revalidation",
		)
	case publishedRetirementQuarantineInstalled:
		start, err := session.quarantinedStart(
			file.Target, bound.Record().LocatorDigest(), mapQuarantineReason(retirement.quarantineReason),
		)
		return invalidatedRevisionStep{start: start, done: true, handled: true}, err
	default:
		return invalidatedRevisionStep{}, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		)
	}
}

func (session *Session) installInvalidatedRevisionRetirementStep(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (invalidatedRevisionStep, error) {
	start, handled, err := session.installInvalidatedRevisionRetirement(file, recordDir, recordName, bound)
	return invalidatedRevisionStep{start: start, done: true, handled: handled}, err
}

func (session *Session) finishRetiringInvalidatedRevision(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (transfer.FileStart, bool, error) {
	_, quarantined, err := session.retireBoundFile(
		recordDir, recordName, bound, transfer.OutputFileBinding{},
	)
	if err != nil {
		return transfer.FileStart{}, true, err
	}
	if !quarantined {
		return transfer.FileStart{}, false, nil
	}
	start, quarantineErr := session.quarantinedStart(
		file.Target, bound.Record().LocatorDigest(), transfer.QuarantineRetirementMismatch,
	)
	return start, true, quarantineErr
}

func (session *Session) installInvalidatedRevisionRetirement(
	file transfer.OutputFile,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (transfer.FileStart, bool, error) {
	retiring, err := resumestate.PrepareInvalidatedRevisionRetirement(bound)
	if err != nil {
		return transfer.FileStart{}, true, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, retiring); err != nil {
		return transfer.FileStart{}, true, err
	}
	_, quarantined, err := session.retireBoundFile(
		recordDir, recordName, retiring, transfer.OutputFileBinding{},
	)
	if err != nil {
		return transfer.FileStart{}, true, err
	}
	if quarantined {
		start, quarantineErr := session.quarantinedStart(
			file.Target, retiring.Record().LocatorDigest(), transfer.QuarantineRetirementMismatch,
		)
		return start, true, quarantineErr
	}
	return transfer.FileStart{}, false, nil
}

func fileOutputFault(operation string, cause error) error {
	code := transfer.OutputFaultStateIO
	if errors.Is(cause, outputcap.ErrUnsafeNamespace) {
		code = transfer.OutputFaultOwnership
	}
	return outputfault.New(transfer.OutputFaultFile, code, fmt.Errorf("%s: %w", operation, cause))
}

var _ transfer.FileTransaction = (*FileTransaction)(nil)
