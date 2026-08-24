package outputsession

import (
	"context"
	"slices"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func (session *Session) PauseTree(
	ctx context.Context,
	reason transfer.JobPauseReason,
) (transfer.DirectTreeSettlement, error) {
	if session == nil || ctx == nil || reason < transfer.JobPauseInterrupted || reason > transfer.JobPauseDependencyContract {
		return transfer.DirectTreeSettlement{}, executorContractError(transfer.ErrInvalidOutputSettlement)
	}
	owner, drained, settlement, err := session.acquireClose(closePause, reason, 0)
	if !owner {
		return settlement, err
	}
	operationID := session.closeOperationID()
	session.emit(session.closeTrace(operationID, OperationPauseTree, TraceDraining))
	<-drained

	errorsByClaim := session.pauseActiveFiles(ctx, operationID, filePauseReasonForJob(reason))
	session.mu.Lock()
	needsAttention := session.attention || session.hasUncertainClaimsLocked()
	snapshot := session.settlementSnapshotLocked()
	session.mu.Unlock()
	durableKind := transfer.DirectTreeSettlementPaused
	if needsAttention {
		durableKind = transfer.DirectTreeSettlementFailed
	}
	if session.lifecycle != nil {
		if lifecycleErr := session.lifecycle.RecordTreeSettlement(ctx, durableKind, 0, snapshot); lifecycleErr != nil {
			errorsByClaim = append(errorsByClaim, session.closedBoundaryError(ctx, lifecycleErr, ErrSessionResourceRelease))
		}
	}
	releaseErr := session.resources.ReleaseOutputSession(ctx)
	if releaseErr != nil {
		errorsByClaim = append(errorsByClaim, session.closedBoundaryError(ctx, releaseErr, ErrSessionResourceRelease))
	}
	stableErr := joinFailures(ctx, errorsByClaim...)

	session.mu.Lock()
	needsAttention = session.attention || stableErr != nil || session.hasUncertainClaimsLocked()
	kind := transfer.DirectTreeSettlementPaused
	if needsAttention {
		kind = transfer.DirectTreeSettlementFailed
	}
	settlement, constructorErr := transfer.NewDirectTreeSettlement(kind)
	if constructorErr != nil {
		stableErr = joinFailures(ctx, stableErr, executorContractError(constructorErr))
	}
	session.state = sessionPaused
	session.close = closeRecord{
		set: true, kind: closePause, pause: reason, settlement: settlement, err: stableErr,
	}
	event := session.traceLocked(operationID, OperationPauseTree, TraceClosed, 0, 0, 0, 0, session.requiredFault)
	session.releaseLedgerLocked()
	session.mu.Unlock()
	session.gate.finishClose()
	session.emit(event)
	return settlement, stableErr
}

func (session *Session) FinalizeTree(
	ctx context.Context,
	outcome transfer.DirectTreeOutcome,
) (transfer.DirectTreeSettlement, error) {
	if session == nil || ctx == nil ||
		(outcome != transfer.DirectTreeOutcomeSuccess && outcome != transfer.DirectTreeOutcomePartial) {
		return transfer.DirectTreeSettlement{}, executorContractError(transfer.ErrInvalidOutputSettlement)
	}
	owner, drained, settlement, err := session.acquireClose(closeComplete, 0, outcome)
	if !owner {
		return settlement, err
	}
	operationID := session.closeOperationID()
	session.emit(session.closeTrace(operationID, OperationFinalizeTree, TraceDraining))
	<-drained

	session.mu.Lock()
	complete := session.completionReadyLocked()
	if !complete {
		value := fault.DependencyContractFault()
		session.requirePauseLocked(value, true)
	}
	session.mu.Unlock()

	var closeErrors []error
	if !complete {
		closeErrors = append(closeErrors, fault.Wrap(fault.DependencyContractFault(), ErrConflictingSettlement))
		closeErrors = append(closeErrors,
			session.pauseActiveFiles(ctx, operationID, transfer.FilePauseDependencyContract)...,
		)
	}
	session.mu.Lock()
	needsAttention := session.attention || session.hasUncertainClaimsLocked()
	snapshot := session.settlementSnapshotLocked()
	session.mu.Unlock()
	durableKind := transfer.DirectTreeSettlementSuccess
	if outcome == transfer.DirectTreeOutcomePartial {
		durableKind = transfer.DirectTreeSettlementPartial
	}
	if needsAttention {
		durableKind = transfer.DirectTreeSettlementFailed
	}
	if session.lifecycle != nil {
		if lifecycleErr := session.lifecycle.RecordTreeSettlement(ctx, durableKind, outcome, snapshot); lifecycleErr != nil {
			closeErrors = append(closeErrors, session.closedBoundaryError(ctx, lifecycleErr, ErrSessionResourceRelease))
		}
	}
	if releaseErr := session.resources.ReleaseOutputSession(ctx); releaseErr != nil {
		closeErrors = append(closeErrors, session.closedBoundaryError(ctx, releaseErr, ErrSessionResourceRelease))
	}
	stableErr := joinFailures(ctx, closeErrors...)

	session.mu.Lock()
	needsAttention = session.attention || stableErr != nil || session.hasUncertainClaimsLocked()
	kind := transfer.DirectTreeSettlementSuccess
	if outcome == transfer.DirectTreeOutcomePartial {
		kind = transfer.DirectTreeSettlementPartial
	}
	if needsAttention {
		kind = transfer.DirectTreeSettlementFailed
	}
	settlement, constructorErr := transfer.NewDirectTreeSettlement(kind)
	if constructorErr != nil {
		stableErr = joinFailures(ctx, stableErr, executorContractError(constructorErr))
	}
	if kind != transfer.DirectTreeSettlementFailed {
		session.state = sessionCompleted
	} else {
		session.state = sessionPaused
	}
	session.close = closeRecord{
		set: true, kind: closeComplete, outcome: outcome, settlement: settlement, err: stableErr,
	}
	event := session.traceLocked(operationID, OperationFinalizeTree, TraceClosed, 0, 0, 0, 0, session.requiredFault)
	session.releaseLedgerLocked()
	session.mu.Unlock()
	session.gate.finishClose()
	session.emit(event)
	return settlement, stableErr
}

func (session *Session) acquireClose(
	kind closeKind,
	pause transfer.JobPauseReason,
	outcome transfer.DirectTreeOutcome,
) (bool, <-chan struct{}, transfer.DirectTreeSettlement, error) {
	owner, drained := session.gate.requestClose()
	if owner {
		return true, drained, transfer.DirectTreeSettlement{}, nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if !session.close.set {
		return false, nil, transfer.DirectTreeSettlement{}, sessionClosedError()
	}
	exact := session.close.kind == kind &&
		(kind != closePause || session.close.pause == pause) &&
		(kind != closeComplete || session.close.outcome == outcome)
	if !exact {
		return false, nil, transfer.DirectTreeSettlement{}, outputFault(
			fault.ScopeOutputPause, fault.OutputContract, ErrConflictingSettlement,
		)
	}
	return false, nil, session.close.settlement, session.close.err
}

func (session *Session) closeOperationID() uint64 {
	session.mu.Lock()
	defer session.mu.Unlock()
	operationID, err := session.nextOperationLocked()
	if err != nil {
		return 0
	}
	return operationID
}

func (session *Session) pauseActiveFiles(
	ctx context.Context,
	operationID uint64,
	reason transfer.FilePauseReason,
) []error {
	session.mu.Lock()
	claimIDs := make([]ClaimID, 0, session.activeFiles)
	for claimID, entry := range session.fileClaims {
		if entry.state == fileActive {
			claimIDs = append(claimIDs, claimID)
		}
	}
	session.mu.Unlock()
	slices.Sort(claimIDs)

	failures := make([]error, 0)
	for _, claimID := range claimIDs {
		session.mu.Lock()
		entry := session.fileClaims[claimID]
		transaction := entry.transaction
		session.mu.Unlock()
		_, err := transaction.pauseForClose(ctx, operationID, reason)
		if err != nil {
			failures = append(failures, session.closedBoundaryError(ctx, err, ErrSessionRequiresPause))
		}
	}
	return failures
}

func (session *Session) completionReadyLocked() bool {
	if session.state != sessionOpen || session.requiredFault.Valid() ||
		session.activeFiles != 0 || session.fileSlots != 0 {
		return false
	}
	if session.scope.RootExpectation().Kind() == transfer.DirectoryAdmissionNoRoot {
		if session.rootClaim != 0 || len(session.directoryClaims) != 0 {
			return false
		}
	} else {
		root := session.directoryClaims[session.rootClaim]
		if session.rootClaim == 0 || root == nil || root.state != directorySettled || root.uncertain {
			return false
		}
	}
	for _, entry := range session.directoryClaims {
		if entry.state != directorySettled || entry.uncertain || entry.finalizationOperation != nil ||
			entry.activeDescendants != 0 || entry.directUnsettledChildren != 0 {
			return false
		}
	}
	for _, entry := range session.fileClaims {
		if entry.state != fileSettled || entry.uncertain || entry.operation != nil || entry.beginOperation != nil {
			return false
		}
	}
	return true
}

func (session *Session) hasUncertainClaimsLocked() bool {
	for _, entry := range session.directoryClaims {
		if entry.uncertain {
			return true
		}
	}
	for _, entry := range session.fileClaims {
		if entry.uncertain {
			return true
		}
	}
	return false
}

func (session *Session) settlementSnapshotLocked() TreeSettlementSnapshot {
	snapshot := TreeSettlementSnapshot{FileSettlements: make([]transfer.FileSettlement, 0, len(session.fileClaims))}
	for _, entry := range session.fileClaims {
		if entry.state != fileSettled {
			continue
		}
		snapshot.FileSettlements = append(snapshot.FileSettlements, entry.settlement)
		switch entry.settlement.Kind() {
		case transfer.FilePublished:
			snapshot.SuccessCount++
		case transfer.FilePaused:
			// A pause retains restart authority; it is neither a terminal success
			// nor a partial-tree failure.
		default:
			snapshot.FailureCount++
		}
	}
	return snapshot
}

func (session *Session) closedBoundaryError(ctx context.Context, err, sentinel error) error {
	result := fault.NormalizeBoundary(ctx, err)
	if result.Kind() == fault.BoundaryCanceled {
		value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
		session.mu.Lock()
		session.requirePauseLocked(value, true)
		session.mu.Unlock()
		return contextFailure(ctx, err)
	}
	value, ok := result.Fault()
	if !ok {
		value, _ = fault.NewOutput(fault.ScopeOutputPause, fault.OutputStateIO)
	}
	session.mu.Lock()
	session.requirePauseLocked(value, true)
	session.mu.Unlock()
	return fault.Wrap(value, sentinel)
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
	case transfer.JobPauseOutputFailure:
		return transfer.FilePauseOutputFailure
	case transfer.JobPauseResourceBudget:
		return transfer.FilePauseResourceBudget
	default:
		return transfer.FilePauseDependencyContract
	}
}

func (session *Session) closeTrace(
	operationID uint64,
	operation OperationKind,
	decision TraceDecision,
) TraceEvent {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.traceLocked(operationID, operation, decision, 0, 0, 0, 0, session.requiredFault)
}
