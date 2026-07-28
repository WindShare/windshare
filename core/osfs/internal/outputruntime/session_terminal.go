package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *Session) CompleteJob(
	ctx context.Context,
	outcome transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	if session == nil || outcome != transfer.JobSucceeded && outcome != transfer.JobCompletedWithErrors {
		return transfer.JobSettlement{}, outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := session.beginSettlement(); err != nil {
		return transfer.JobSettlement{}, err
	}
	defer session.endSettlement()
	if err := ctx.Err(); err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(err)
	}
	session.beginWG.Wait()
	session.mu.Lock()
	active := len(session.active)
	session.mu.Unlock()
	if active != 0 {
		contractErr := outputfault.New(
			transfer.OutputFaultSession, transfer.OutputFaultContract,
			fmt.Errorf("%w: %d file transactions remain active", transfer.ErrOutputContract, active),
		)
		return transfer.JobSettlement{}, session.failOwnerSettlement(contractErr)
	}
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(
			FilesystemOutputAncestrySessionFinalize, resumestate.LocatorDigest{}, err,
		)
		return transfer.JobSettlement{}, session.failOwnerSettlement(
			outputAncestryOperationFault("validate ancestry before completing session", err),
		)
	}
	validationOwned := true
	finishAncestry := func() error {
		if !validationOwned {
			return nil
		}
		validationOwned = false
		return finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestrySessionFinalize,
			resumestate.LocatorDigest{}, "finish completing session ancestry", nil,
		)
	}
	fail := func(cause error) (transfer.JobSettlement, error) {
		return transfer.JobSettlement{}, session.failOwnerSettlement(errors.Join(cause, finishAncestry()))
	}
	if err := session.installLifecycle(resumestate.SessionCompleting); err != nil {
		return fail(err)
	}

	coordinator, err := session.acquireTerminalLocks()
	if err != nil {
		return fail(err)
	}
	coordinatorOwned := true
	defer func() {
		if coordinatorOwned {
			_ = coordinator.Close()
		}
	}()
	defer func() { _ = session.shutdownOwnerLocked() }()

	// Reopen the terminal layout through the same restart path used after a
	// process crash. One reducer/executor therefore owns every persisted cut,
	// instead of live completion and restart subtly disagreeing on removal order.
	releaseErr := errors.Join(
		closeOutputV3Lock(session.sessionLock),
		closeOutputV3Directory(session.stagesDir),
		closeOutputV3Directory(session.anchorsDir),
		closeOutputV3Directory(session.filesDir),
	)
	session.sessionLock, session.stagesDir, session.anchorsDir, session.filesDir = nil, nil, nil, nil
	if releaseErr != nil {
		return fail(outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, releaseErr))
	}
	state := session.stateSnapshot()
	attention, err := session.owner.recoverTerminalSession(
		session.platform, session.control, session.intentDir, session.sessionDir, state,
		outputSelectionAdmission{
			selection: session.selection, files: session.selectedFiles, dirs: session.selectedDirs,
			ancestry: session.ancestry,
		},
	)
	if err != nil {
		return fail(sessionSettlementFailure(err))
	}
	if ancestryErr := finishAncestry(); ancestryErr != nil {
		coordinatorErr := coordinator.Close()
		coordinatorOwned = false
		return transfer.JobSettlement{}, session.failOwnerSettlement(errors.Join(
			ancestryErr,
			outputAncestryCleanupFault("close terminal coordinator after ancestry failure", coordinatorErr),
		))
	}
	closeErr := errors.Join(coordinator.Close(), session.shutdownOwnerLocked())
	coordinatorOwned = false
	if closeErr != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr)
	}
	settlementKind := transfer.JobClosed
	if attention {
		settlementKind = transfer.JobPausedNeedsAttention
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), JobSettlement: settlementKind,
	})
	return settlement, nil
}

func sessionSettlementFailure(cause error) error {
	if _, found := errors.AsType[*transfer.OutputFault](cause); found {
		return cause
	}
	return outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, cause)
}

func filePauseReasonForJob(reason transfer.JobPauseReason) transfer.FilePauseReason {
	switch reason {
	case transfer.JobPauseInterrupted:
		return transfer.FilePauseInterrupted
	case transfer.JobPauseShutdown:
		return transfer.FilePauseShutdown
	case transfer.JobPauseTransportFailure:
		return transfer.FilePauseTransportFailure
	case transfer.JobPauseSessionFailure:
		return transfer.FilePauseSessionFailure
	default:
		return transfer.FilePauseOutputFailure
	}
}

func (session *Session) acquireTerminalLocks() (outputcap.Lock, error) {
	if err := session.sessionLock.Close(); err != nil {
		return nil, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	coordinator, err := session.owner.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return session.control.Directory().AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: session.resumeIntent, sessionID: session.SessionID(),
			selectionIdentity:    session.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(session.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(session.platform.Certification()),
			scope:                FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		outputnamespace.RootFault("reacquire coordinator lock", outputfault.ErrRootUnsafe),
	)
	if err != nil {
		return nil, err
	}
	lock, err := session.owner.acquireRuntimeNativeLock(
		func() (outputcap.Lock, bool, error) {
			return session.sessionDir.AcquireLock(resumestate.SessionLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: session.resumeIntent, sessionID: session.SessionID(),
			selectionIdentity:    session.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(session.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(session.platform.Certification()),
			scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
		},
		intentOutputFault("reacquire session lock", outputfault.ErrIntentUnsafe),
	)
	if err != nil {
		_ = coordinator.Close()
		return nil, err
	}
	session.sessionLock = lock
	if err := session.revalidateFixedSession(); err != nil {
		_ = lock.Close()
		_ = coordinator.Close()
		return nil, err
	}
	return coordinator, nil
}

func (session *Session) revalidateFixedSession() error {
	intentName := resumestate.ResumeNamespaceName(session.resumeIntent)
	reopenedIntent, err := session.control.Sessions().OpenDirectory(intentName, true)
	if err != nil {
		return intentOutputFault("reopen intent for terminal transition", err)
	}
	sameIntent, compareErr := reopenedIntent.SameDirectory(session.intentDir)
	closeErr := reopenedIntent.Close()
	if compareErr != nil || closeErr != nil || !sameIntent {
		return intentOutputFault(
			"verify intent for terminal transition", errors.Join(outputfault.ErrIntentUnsafe, compareErr, closeErr),
		)
	}
	sessionName := resumestate.SessionDirectoryName(session.sessionID)
	reopenedSession, err := session.intentDir.OpenDirectory(sessionName, true)
	if err != nil {
		return intentOutputFault("reopen session for terminal transition", err)
	}
	sameSession, compareErr := reopenedSession.SameDirectory(session.sessionDir)
	closeErr = reopenedSession.Close()
	if compareErr != nil || closeErr != nil || !sameSession {
		return intentOutputFault(
			"verify session for terminal transition", errors.Join(outputfault.ErrIntentUnsafe, compareErr, closeErr),
		)
	}
	encoded, err := outputnamespace.ReadRecord(session.sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return intentOutputFault("reread terminal session header", err)
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil || header != session.stateSnapshot().Header() {
		return intentOutputFault("verify terminal session header", errors.Join(outputfault.ErrIntentUnsafe, err))
	}
	return nil
}

func (session *Session) completeFileRecords(
	preflight outputV3FileNamespaceSnapshot,
) (bool, error) {
	attention := len(preflight.attention) != 0
	for _, expected := range preflight.shards {
		shardAttention, err := session.completePreflightFileShard(expected)
		if err != nil {
			return false, err
		}
		attention = attention || shardAttention
	}

	// Temporary reduction can advance a target generation. A fresh full scan is
	// therefore the immutable authority used for retirement, not the stale
	// pre-recovery record image.
	settled, err := scanOutputV3FileNamespace(session)
	if err != nil {
		return false, err
	}
	attention = attention || len(settled.attention) != 0
	records := make(map[string]outputV3FileNamespaceRecord, len(settled.records))
	for _, record := range settled.records {
		records[record.shardName+"/"+record.recordName] = record
	}
	for _, expected := range settled.shards {
		shardAttention, err := session.completeSettledFileShard(records, expected)
		if err != nil {
			return false, err
		}
		attention = attention || shardAttention
	}
	return attention, nil
}

// Recovery temporaries are reduced only after the complete namespace has
// passed its selection-derived budget. Comparing this shard with the preflight
// image before mutation prevents a stale enumeration from authorizing cleanup
// in a replaced shard.
func (session *Session) completePreflightFileShard(
	expected outputV3FileNamespaceShard,
) (bool, error) {
	shard, err := session.filesDir.OpenDirectory(expected.name, true)
	if err != nil {
		return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	names, listErr := shard.Names(outputnamespace.FileShardInspectionLimit)
	slices.Sort(names)
	if listErr != nil || !slices.Equal(names, outputV3FileNamespaceEntryNames(expected.entries)) {
		_ = shard.Close()
		return false, transfer.NewOutputSessionError(
			outputfault.New(
				transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
				errors.Join(outputcap.ErrUnsafeNamespace, listErr),
			),
			true,
		)
	}
	shardAttention, recoveryErr := session.reconcileFileShardUpdates(expected.name, shard, names)
	closeErr := shard.Close()
	if recoveryErr != nil || closeErr != nil {
		return false, errors.Join(recoveryErr, outputV3CloseShardFault(closeErr))
	}
	return shardAttention, nil
}

func (session *Session) completeSettledFileShard(
	records map[string]outputV3FileNamespaceRecord,
	expected outputV3FileNamespaceShard,
) (attention bool, resultErr error) {
	shard, err := session.filesDir.OpenDirectory(expected.name, true)
	if err != nil {
		return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	names, listErr := shard.Names(outputnamespace.FileShardInspectionLimit)
	slices.Sort(names)
	if listErr != nil || !slices.Equal(names, outputV3FileNamespaceEntryNames(expected.entries)) {
		_ = shard.Close()
		return false, transfer.NewOutputSessionError(
			outputfault.New(
				transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
				errors.Join(outputcap.ErrUnsafeNamespace, listErr),
			),
			true,
		)
	}
	for _, entry := range expected.entries {
		entryAttention, entryErr := session.completeSettledFileEntry(records, shard, expected.name, entry)
		if entryErr != nil {
			_ = shard.Close()
			return false, entryErr
		}
		attention = attention || entryAttention
	}
	if err := shard.Close(); err != nil {
		return false, outputfault.New(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	return attention, nil
}

func (session *Session) completeSettledFileEntry(
	records map[string]outputV3FileNamespaceRecord,
	shard outputcap.Directory,
	shardName string,
	entry outputV3FileNamespaceEntry,
) (bool, error) {
	if entry.classification.Classification() != resumestate.FileShardEntryRecord {
		return true, nil
	}
	scanned, valid := records[shardName+"/"+entry.name]
	if !valid {
		return true, nil
	}
	bound := scanned.bound
	switch bound.Record().Phase() {
	case resumestate.FilePublished:
		return session.completePublishedFileEntry(shard, entry.name, bound)
	case resumestate.FileRetiring:
		return session.completeRetiringFileEntry(shard, entry.name, bound)
	default:
		return true, nil
	}
}

func (session *Session) completePublishedFileEntry(
	shard outputcap.Directory,
	entryName string,
	bound resumestate.BoundFileRecord,
) (bool, error) {
	retirement, err := session.authorizePublishedRetirement(shard, entryName, bound)
	if err != nil {
		return false, err
	}
	switch retirement.disposition {
	case publishedRetirementAuthorized:
	case publishedRetirementHoldPreserve, publishedRetirementQuarantineInstalled:
		return true, nil
	default:
		return false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		)
	}
	retiring, err := resumestate.PreparePublishedRetirement(bound)
	if err != nil {
		return false, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
	}
	if err := session.installFileRecord(shard, entryName, bound, retiring); err != nil {
		return false, err
	}
	return session.retireSettledFile(shard, entryName, retiring)
}

func (session *Session) completeRetiringFileEntry(
	shard outputcap.Directory,
	entryName string,
	bound resumestate.BoundFileRecord,
) (bool, error) {
	return session.retireSettledFile(shard, entryName, bound)
}

func (session *Session) retireSettledFile(
	shard outputcap.Directory,
	entryName string,
	bound resumestate.BoundFileRecord,
) (bool, error) {
	_, quarantined, err := session.retireBoundFile(
		shard, entryName, bound, transfer.OutputFileBinding{},
	)
	if isInternalCleanupNeedsAttentionFault(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return quarantined, nil
}

type publishedRetirementDisposition uint8

const (
	publishedRetirementAuthorized publishedRetirementDisposition = iota + 1
	publishedRetirementHoldPreserve
	publishedRetirementQuarantineInstalled
)

type publishedRetirementOutcome struct {
	disposition      publishedRetirementDisposition
	quarantineReason resumestate.QuarantineReason
}

// authorizePublishedRetirement keeps final-path evidence in the Published
// state until the published reducer has accepted the current anchor, stage,
// final, and metadata observation. Retiring deliberately stops observing the
// user-visible final, so entering it before this check would turn a removed or
// replaced final into cleanup authority for the last internal witness.
func (session *Session) authorizePublishedRetirement(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (result publishedRetirementOutcome, resultErr error) {
	requirement := outputAncestryRequirement{}
	validation, err := session.validateOutputAncestry(requirement)
	if err != nil {
		session.traceOutputAncestry(FilesystemOutputAncestryRecovery, bound.Record().LocatorDigest(), err)
		return publishedRetirementOutcome{},
			outputAncestryOperationFault("validate ancestry before published retirement", err)
	}
	defer func() {
		ancestryErr := finishOutputAncestryOperation(
			session, validation, requirement, FilesystemOutputAncestryRecovery,
			bound.Record().LocatorDigest(), "finish published-retirement ancestry", nil,
		)
		if ancestryErr != nil {
			result = publishedRetirementOutcome{}
			resultErr = errors.Join(resultErr, ancestryErr)
		}
	}()
	return session.reducePublishedRetirement(validation, recordDir, recordName, bound)
}

func (session *Session) reducePublishedRetirement(
	validation *outputAncestryValidation,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
) (publishedRetirementOutcome, error) {
	stageParentSynced := false
	for {
		observation, observationCleanupErr, observationErr := session.observeFile(
			validation, bound.Record(), false,
		)
		if observationErr != nil {
			return publishedRetirementOutcome{}, pauseRequiredFileOutputFault(fileOutputFault(
				"observe published file before retirement", errors.Join(observationErr, observationCleanupErr),
			))
		}
		decision, err := resumestate.ReduceFileRecovery(bound, observation)
		if err != nil {
			return publishedRetirementOutcome{}, outputfault.New(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		if observationCleanupErr != nil && !publishedRetirementAllowsObservationCleanup(decision.Action()) {
			return publishedRetirementOutcome{}, pauseRequiredFileOutputFault(fileOutputFault(
				"close published-retirement observation", observationCleanupErr,
			))
		}
		session.tracePublishedRetirementDecision(bound, decision)
		outcome, synced, repeat, err := session.applyPublishedRetirementDecision(
			decision, observationCleanupErr, recordDir, recordName, bound, stageParentSynced,
		)
		if err != nil {
			return publishedRetirementOutcome{}, err
		}
		if !repeat {
			return outcome, nil
		}
		stageParentSynced = synced
	}
}

func publishedRetirementAllowsObservationCleanup(action resumestate.RecoveryAction) bool {
	return action == resumestate.RecoveryInstallQuarantine || action == resumestate.RecoveryHoldQuarantine
}

func (session *Session) tracePublishedRetirementDecision(
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

func (session *Session) applyPublishedRetirementDecision(
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	stageParentSynced bool,
) (publishedRetirementOutcome, bool, bool, error) {
	switch decision.Action() {
	case resumestate.RecoveryRemovePublishedStageAndSync:
		return session.removePublishedStageForRetirement(bound)
	case resumestate.RecoverySyncPublishedStageParent:
		return session.syncPublishedStageForRetirement(bound, stageParentSynced)
	case resumestate.RecoveryHoldPublishedCleanup:
		return publishedRetirementOutcome{disposition: publishedRetirementHoldPreserve}, false, false, nil
	case resumestate.RecoveryInstallQuarantine:
		return session.installPublishedRetirementQuarantine(
			recordDir, recordName, bound, decision, observationCleanupErr,
		)
	case resumestate.RecoveryHoldQuarantine:
		return holdPublishedRetirementQuarantine(bound, observationCleanupErr)
	default:
		return publishedRetirementOutcome{}, false, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract,
			fmt.Errorf("unexpected published retirement action %d", decision.Action()),
		)
	}
}

func (session *Session) removePublishedStageForRetirement(
	bound resumestate.BoundFileRecord,
) (publishedRetirementOutcome, bool, bool, error) {
	operationErr, cleanupErr := session.removeStage(bound.Record())
	if operationErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOperationFault(
			"remove published stage before retirement", operationErr, cleanupErr,
		)
	}
	if cleanupErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOperationFault(
			"close removed published stage before retirement", nil, cleanupErr,
		)
	}
	return publishedRetirementOutcome{}, true, true, nil
}

func (session *Session) syncPublishedStageForRetirement(
	bound resumestate.BoundFileRecord,
	stageParentSynced bool,
) (publishedRetirementOutcome, bool, bool, error) {
	if stageParentSynced {
		return publishedRetirementOutcome{disposition: publishedRetirementAuthorized}, true, false, nil
	}
	stage := resumestate.StageName(bound.Record().OutputObject())
	operationErr, cleanupErr := session.syncObjectShard(session.stagesDir, stage)
	if operationErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOperationFault(
			"sync published stage parent before retirement", operationErr, cleanupErr,
		)
	}
	if cleanupErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOperationFault(
			"close synced published-stage parent before retirement", nil, cleanupErr,
		)
	}
	return publishedRetirementOutcome{}, true, true, nil
}

func (session *Session) installPublishedRetirementQuarantine(
	recordDir outputcap.Directory,
	recordName string,
	bound resumestate.BoundFileRecord,
	decision resumestate.RecoveryDecision,
	observationCleanupErr error,
) (publishedRetirementOutcome, bool, bool, error) {
	quarantined, err := resumestate.ApplyRecoveryDecision(bound, decision)
	if err != nil {
		return publishedRetirementOutcome{}, false, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, err,
		)
	}
	if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
		return publishedRetirementOutcome{}, false, false, err
	}
	if observationCleanupErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOutputFault(fileOutputFault(
			"close quarantined published-retirement observation", observationCleanupErr,
		))
	}
	return publishedRetirementOutcome{
		disposition:      publishedRetirementQuarantineInstalled,
		quarantineReason: quarantined.Record().QuarantineReason(),
	}, false, false, nil
}

func holdPublishedRetirementQuarantine(
	bound resumestate.BoundFileRecord,
	observationCleanupErr error,
) (publishedRetirementOutcome, bool, bool, error) {
	if observationCleanupErr != nil {
		return publishedRetirementOutcome{}, false, false, pauseRequiredFileOutputFault(fileOutputFault(
			"close held published-retirement observation", observationCleanupErr,
		))
	}
	if !bound.Record().QuarantineReason().Valid() {
		return publishedRetirementOutcome{}, false, false, outputfault.New(
			transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
		)
	}
	return publishedRetirementOutcome{
		disposition:      publishedRetirementQuarantineInstalled,
		quarantineReason: bound.Record().QuarantineReason(),
	}, false, false, nil
}
