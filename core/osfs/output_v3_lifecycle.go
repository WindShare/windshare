package osfs

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func (session *filesystemOutputSession) PauseJob(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.JobSettlement, error) {
	if session == nil || reason < transfer.JobPauseInterrupted || reason > transfer.JobPauseOutputFailure {
		return transfer.JobSettlement{}, outputFault(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputSettlement,
		)
	}
	if err := session.beginSettlement(); err != nil {
		return transfer.JobSettlement{}, err
	}
	defer session.endSettlement()
	session.beginWG.Wait()
	if err := session.installLifecycle(resumestate.SessionPausing); err != nil {
		return transfer.JobSettlement{}, session.failOwnerSettlement(err)
	}
	session.mu.Lock()
	transactions := make([]*filesystemFileTransaction, 0, len(session.active))
	for _, transaction := range session.active {
		transactions = append(transactions, transaction)
	}
	attention := len(session.attention) != 0
	session.mu.Unlock()
	fileReason := filePauseReasonForJob(reason)
	var settleErr error
	for _, transaction := range transactions {
		settlement, err := transaction.pauseForSessionSettlement(ctx, fileReason)
		if err != nil {
			settleErr = errors.Join(settleErr, err)
			attention = true
			continue
		}
		if settlement.Kind() == transfer.FileQuarantined {
			attention = true
		}
	}

	next := resumestate.SessionPaused
	settlementKind := transfer.JobPaused
	if attention || settleErr != nil {
		next = resumestate.SessionPausedNeedsAttention
		settlementKind = transfer.JobPausedNeedsAttention
	}
	stateErr := session.installLifecycle(next)
	closeErr := session.shutdownOwnerLocked()
	if err := errors.Join(settleErr, stateErr, closeErr); err != nil {
		return transfer.JobSettlement{}, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), JobSettlement: settlementKind,
	})
	return settlement, nil
}

func (session *filesystemOutputSession) CompleteJob(
	ctx context.Context,
	outcome transfer.JobOutcome,
) (transfer.JobSettlement, error) {
	if session == nil || outcome != transfer.JobSucceeded && outcome != transfer.JobCompletedWithErrors {
		return transfer.JobSettlement{}, outputFault(
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
		contractErr := outputFault(
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
		return fail(outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, releaseErr))
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
		return transfer.JobSettlement{}, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr)
	}
	settlementKind := transfer.JobClosed
	if attention {
		settlementKind = transfer.JobPausedNeedsAttention
	}
	settlement, err := transfer.NewJobSettlement(settlementKind)
	if err != nil {
		return transfer.JobSettlement{}, outputFault(transfer.OutputFaultSession, transfer.OutputFaultContract, err)
	}
	session.owner.trace(FilesystemOutputTrace{
		Operation: TraceSessionSettlement, ResumeIntent: session.resumeIntent,
		SessionID: session.SessionID(), JobSettlement: settlementKind,
	})
	return settlement, nil
}

func (session *filesystemOutputSession) beginSettlement() error {
	if session == nil {
		return outputFault(
			transfer.OutputFaultSession, transfer.OutputFaultContract, transfer.ErrInvalidOutputBinding,
		)
	}
	session.operationGate.Lock()
	session.mu.Lock()
	if session.closed || session.settling || session.poisoned {
		session.mu.Unlock()
		session.operationGate.Unlock()
		return outputFault(transfer.OutputFaultSession, transfer.OutputFaultOwnership, errOutputSessionClosed)
	}
	session.settling = true
	session.mu.Unlock()
	return nil
}

func (session *filesystemOutputSession) endSettlement() {
	session.operationGate.Unlock()
}

func (session *filesystemOutputSession) failOwnerSettlement(cause error) error {
	cause = sessionSettlementFailure(cause)
	closeErr := session.shutdownOwnerLocked()
	if closeErr == nil {
		return cause
	}
	return errors.Join(
		cause,
		outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, closeErr),
	)
}

func sessionSettlementFailure(cause error) error {
	var fault *transfer.OutputFault
	if errors.As(cause, &fault) {
		return cause
	}
	return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, cause)
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

func (session *filesystemOutputSession) acquireTerminalLocks() (outputV3Lock, error) {
	if err := session.sessionLock.Close(); err != nil {
		return nil, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
	}
	coordinator, err := session.owner.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return session.control.directory.AcquireLock(resumestate.CoordinatorLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: session.resumeIntent, sessionID: session.SessionID(),
			selectionIdentity:    session.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(session.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(session.platform.Certification()),
			scope:                FilesystemOutputNativeLockCoordinator, failureScope: transfer.OutputFaultRoot,
		},
		rootOutputFault("reacquire coordinator lock", errOutputRootUnsafe),
	)
	if err != nil {
		return nil, err
	}
	lock, err := session.owner.acquireRuntimeNativeLock(
		func() (outputV3Lock, bool, error) {
			return session.sessionDir.AcquireLock(resumestate.SessionLockName, true)
		},
		filesystemOutputNativeLockContext{
			resumeIntent: session.resumeIntent, sessionID: session.SessionID(),
			selectionIdentity:    session.selection.Identity(),
			outputAncestryDigest: filesystemOutputAncestryDigestFromState(session.ancestry.binding),
			certification:        filesystemOutputCertificationFromState(session.platform.Certification()),
			scope:                FilesystemOutputNativeLockSession, failureScope: transfer.OutputFaultSession,
		},
		intentOutputFault("reacquire session lock", errOutputIntentUnsafe),
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

func (session *filesystemOutputSession) revalidateFixedSession() error {
	intentName := resumestate.ResumeNamespaceName(session.resumeIntent)
	reopenedIntent, err := session.control.sessions.OpenDirectory(intentName, true)
	if err != nil {
		return intentOutputFault("reopen intent for terminal transition", err)
	}
	sameIntent, compareErr := reopenedIntent.SameDirectory(session.intentDir)
	closeErr := reopenedIntent.Close()
	if compareErr != nil || closeErr != nil || !sameIntent {
		return intentOutputFault(
			"verify intent for terminal transition", errors.Join(errOutputIntentUnsafe, compareErr, closeErr),
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
			"verify session for terminal transition", errors.Join(errOutputIntentUnsafe, compareErr, closeErr),
		)
	}
	encoded, err := readStateRecord(session.sessionDir, resumestate.HeaderRecordName, resumestate.MaxSessionHeaderBytes)
	if err != nil {
		return intentOutputFault("reread terminal session header", err)
	}
	header, err := resumestate.DecodeHeader(encoded)
	if err != nil || header != session.stateSnapshot().Header() {
		return intentOutputFault("verify terminal session header", errors.Join(errOutputIntentUnsafe, err))
	}
	return nil
}

func (session *filesystemOutputSession) completeFileRecords(
	preflight outputV3FileNamespaceSnapshot,
) (bool, error) {
	attention := len(preflight.attention) != 0

	// Recovery temporaries are reduced only after the complete namespace has
	// passed its selection-derived budget. Every shard is also compared with the
	// preflight image before its first mutation, preventing a stale enumeration
	// from authorizing cleanup in a replaced shard.
	for _, expected := range preflight.shards {
		shard, err := session.filesDir.OpenDirectory(expected.name, true)
		if err != nil {
			return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		names, listErr := shard.Names(outputFileShardInspectionLimit)
		slices.Sort(names)
		expectedNames := outputV3FileNamespaceEntryNames(expected.entries)
		if listErr != nil || !slices.Equal(names, expectedNames) {
			_ = shard.Close()
			return false, transfer.NewOutputSessionError(
				outputFault(
					transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
					errors.Join(errOutputV3Unsafe, listErr),
				),
				true,
			)
		}
		shardAttention, recoveryErr := session.reconcileFileShardUpdates(expected.name, shard, names)
		closeErr := shard.Close()
		if recoveryErr != nil || closeErr != nil {
			return false, errors.Join(recoveryErr, outputV3CloseShardFault(closeErr))
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
		shard, err := session.filesDir.OpenDirectory(expected.name, true)
		if err != nil {
			return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		names, listErr := shard.Names(outputFileShardInspectionLimit)
		slices.Sort(names)
		if listErr != nil || !slices.Equal(names, outputV3FileNamespaceEntryNames(expected.entries)) {
			_ = shard.Close()
			return false, transfer.NewOutputSessionError(
				outputFault(
					transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
					errors.Join(errOutputV3Unsafe, listErr),
				),
				true,
			)
		}
		for _, entry := range expected.entries {
			if entry.classification.Classification() != resumestate.FileShardEntryRecord {
				attention = true
				continue
			}
			scanned, valid := records[expected.name+"/"+entry.name]
			if !valid {
				attention = true
				continue
			}
			bound := scanned.bound
			switch bound.Record().Phase() {
			case resumestate.FilePublished:
				retirement, verifyErr := session.authorizePublishedRetirement(
					shard, entry.name, bound,
				)
				if verifyErr != nil {
					_ = shard.Close()
					return false, verifyErr
				}
				switch retirement.disposition {
				case publishedRetirementAuthorized:
				case publishedRetirementHoldPreserve, publishedRetirementQuarantineInstalled:
					attention = true
					continue
				default:
					_ = shard.Close()
					return false, outputFault(
						transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
					)
				}
				retiring, prepareErr := resumestate.PreparePublishedRetirement(bound)
				if prepareErr != nil {
					_ = shard.Close()
					return false, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, prepareErr)
				}
				if err := session.installFileRecord(shard, entry.name, bound, retiring); err != nil {
					_ = shard.Close()
					return false, err
				}
				_, quarantined, err := session.retireBoundFile(
					shard, entry.name, retiring, transfer.OutputFileBinding{},
				)
				if isInternalCleanupNeedsAttentionFault(err) {
					attention = true
					continue
				}
				if err != nil {
					_ = shard.Close()
					return false, err
				}
				attention = attention || quarantined
			case resumestate.FileRetiring:
				_, quarantined, err := session.retireBoundFile(
					shard, entry.name, bound, transfer.OutputFileBinding{},
				)
				if isInternalCleanupNeedsAttentionFault(err) {
					attention = true
					continue
				}
				if err != nil {
					_ = shard.Close()
					return false, err
				}
				attention = attention || quarantined
			default:
				attention = true
			}
		}
		if err := shard.Close(); err != nil {
			return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
	}
	return attention, nil
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
func (session *filesystemOutputSession) authorizePublishedRetirement(
	recordDir outputV3Directory,
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
			return publishedRetirementOutcome{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
		}
		quarantineDecision := decision.Action() == resumestate.RecoveryInstallQuarantine ||
			decision.Action() == resumestate.RecoveryHoldQuarantine
		if observationCleanupErr != nil && !quarantineDecision {
			return publishedRetirementOutcome{}, pauseRequiredFileOutputFault(fileOutputFault(
				"close published-retirement observation", observationCleanupErr,
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

		switch decision.Action() {
		case resumestate.RecoveryRemovePublishedStageAndSync:
			operationErr, cleanupErr := session.removeStage(bound.Record())
			if operationErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOperationFault(
					"remove published stage before retirement", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOperationFault(
					"close removed published stage before retirement", nil, cleanupErr,
				)
			}
			stageParentSynced = true
		case resumestate.RecoverySyncPublishedStageParent:
			if stageParentSynced {
				return publishedRetirementOutcome{disposition: publishedRetirementAuthorized}, nil
			}
			stage := resumestate.StageName(bound.Record().OutputObject())
			operationErr, cleanupErr := session.syncObjectShard(session.stagesDir, stage)
			if operationErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOperationFault(
					"sync published stage parent before retirement", operationErr, cleanupErr,
				)
			}
			if cleanupErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOperationFault(
					"close synced published-stage parent before retirement", nil, cleanupErr,
				)
			}
			stageParentSynced = true
		case resumestate.RecoveryHoldPublishedCleanup:
			return publishedRetirementOutcome{disposition: publishedRetirementHoldPreserve}, nil
		case resumestate.RecoveryInstallQuarantine:
			quarantined, err := resumestate.ApplyRecoveryDecision(bound, decision)
			if err != nil {
				return publishedRetirementOutcome{}, outputFault(transfer.OutputFaultFile, transfer.OutputFaultContract, err)
			}
			if err := session.installFileRecord(recordDir, recordName, bound, quarantined); err != nil {
				return publishedRetirementOutcome{}, err
			}
			if observationCleanupErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOutputFault(fileOutputFault(
					"close quarantined published-retirement observation", observationCleanupErr,
				))
			}
			return publishedRetirementOutcome{
				disposition:      publishedRetirementQuarantineInstalled,
				quarantineReason: quarantined.Record().QuarantineReason(),
			}, nil
		case resumestate.RecoveryHoldQuarantine:
			if observationCleanupErr != nil {
				return publishedRetirementOutcome{}, pauseRequiredFileOutputFault(fileOutputFault(
					"close held published-retirement observation", observationCleanupErr,
				))
			}
			if !bound.Record().QuarantineReason().Valid() {
				return publishedRetirementOutcome{}, outputFault(
					transfer.OutputFaultFile, transfer.OutputFaultContract, resumestate.ErrInvalidState,
				)
			}
			return publishedRetirementOutcome{
				disposition:      publishedRetirementQuarantineInstalled,
				quarantineReason: bound.Record().QuarantineReason(),
			}, nil
		default:
			return publishedRetirementOutcome{}, outputFault(
				transfer.OutputFaultFile, transfer.OutputFaultContract,
				fmt.Errorf("unexpected published retirement action %d", decision.Action()),
			)
		}
	}
}

func outputV3FileNamespaceEntryNames(entries []outputV3FileNamespaceEntry) []string {
	names := make([]string, len(entries))
	for index := range entries {
		names[index] = entries[index].name
	}
	return names
}

func outputV3CloseShardFault(err error) error {
	if err == nil {
		return nil
	}
	return outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
}

func (session *filesystemOutputSession) inspectAndRemoveEmptyShards() (bool, error) {
	attention := false
	for _, parent := range []outputV3Directory{session.stagesDir, session.anchorsDir, session.filesDir} {
		names, err := parent.Names(resumestate.MaxFileStateShardDirectories + 1)
		if err != nil {
			return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
		}
		for _, name := range names {
			if !validStateShard(name) {
				attention = true
				continue
			}
			shard, err := parent.OpenDirectory(name, true)
			if err != nil {
				return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
			}
			children, listErr := shard.Names(1)
			if listErr != nil {
				_ = shard.Close()
				return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, listErr)
			}
			if len(children) != 0 {
				attention = true
				_ = shard.Close()
				continue
			}
			removeErr := parent.RemoveDirectory(name, shard)
			syncErr := parent.Sync()
			closeErr := shard.Close()
			if err := errors.Join(removeErr, syncErr, closeErr); err != nil {
				return false, outputFault(transfer.OutputFaultSession, transfer.OutputFaultStateIO, err)
			}
		}
	}
	return attention, nil
}
