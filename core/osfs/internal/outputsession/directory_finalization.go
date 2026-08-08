package outputsession

import (
	"context"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func (session *Session) FinalizeDirectory(
	ctx context.Context,
	admission transfer.DirectoryAdmission,
) (transfer.DirectorySettlement, error) {
	lease, operationID, err := session.beginOperation()
	if err != nil {
		return transfer.DirectorySettlement{}, err
	}
	defer lease.release()
	if ctx == nil {
		return transfer.DirectorySettlement{}, executorContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectorySettlement{}, err
	}

	entry, pending, cached, event, err := session.reserveDirectoryFinalization(operationID, admission)
	session.emit(event)
	if err != nil {
		return transfer.DirectorySettlement{}, err
	}
	if cached {
		return entry.settlement, nil
	}
	if pending != nil {
		return waitDirectoryFinalization(ctx, lease.closing(), pending)
	}
	if err := session.waitForDirectoryDescendants(ctx, lease.closing(), entry); err != nil {
		return transfer.DirectorySettlement{}, session.finishDirectoryFinalizationWithoutMutation(
			operationID, entry, err,
		)
	}

	observation, executeErr := session.directories.FinalizeDirectory(ctx, entry.claim)
	if executeErr != nil {
		cut := observation.Cut
		if !cut.valid() || cut == MutationStable {
			cut = MutationAmbiguous
			executeErr = executorContractError(executeErr)
		}
		return transfer.DirectorySettlement{}, session.failDirectoryFinalization(
			ctx, operationID, entry, cut, executeErr,
		)
	}
	settlement, err := session.directorySettlement(entry.admission, observation)
	if err != nil {
		return transfer.DirectorySettlement{}, session.failDirectoryFinalization(
			ctx, operationID, entry, MutationAmbiguous, err,
		)
	}
	return session.commitDirectoryFinalization(operationID, entry, settlement)
}

func (session *Session) reserveDirectoryFinalization(
	operationID uint64,
	admission transfer.DirectoryAdmission,
) (*directoryEntry, *directoryFinalizationOperation, bool, TraceEvent, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	claimID, ok := session.receiptClaims[receiptKey(admission)]
	entry := session.directoryClaims[claimID]
	if !ok || entry == nil || !sameAdmission(entry.admission, admission) {
		return nil, nil, false, session.bindingTraceLocked(operationID, claimID, OperationFinalizeDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if entry.state == directorySettled {
		value, _ := entry.settlement.IsolatedFault()
		return entry, nil, true,
			session.traceLocked(operationID, OperationFinalizeDirectory, TraceSettled, claimID, ClaimDirectory,
				ClaimSettled, ClaimSettled, value), nil
	}
	if entry.finalizationOperation != nil {
		return entry, entry.finalizationOperation, false,
			session.traceLocked(operationID, OperationFinalizeDirectory, TraceCoalesced, claimID, ClaimDirectory,
				ClaimSettling, ClaimSettling, fault.Fault{}), nil
	}
	if entry.state != directoryAdmitted && entry.state != directorySettling || entry.uncertain {
		return nil, nil, false, session.bindingTraceLocked(operationID, claimID, OperationFinalizeDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if err := session.operationRejectionLocked(); err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, claimID, OperationFinalizeDirectory), err
	}
	from := ClaimAdmitted
	if entry.state == directorySettling {
		from = ClaimSettling
	}
	entry.state = directorySettling
	entry.finalizationOperation = &directoryFinalizationOperation{done: make(chan struct{})}
	entry.notifyLocked()
	if err := session.adjustActiveAncestorsLocked(entry.claim.parent, true); err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, claimID, OperationFinalizeDirectory),
			session.markInvariantFailureLocked()
	}
	return entry, nil, false,
		session.traceLocked(operationID, OperationFinalizeDirectory, TraceSealed, claimID, ClaimDirectory,
			from, ClaimSettling, fault.Fault{}), nil
}

func (session *Session) waitForDirectoryDescendants(
	ctx context.Context,
	closing <-chan struct{},
	entry *directoryEntry,
) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		session.mu.Lock()
		if session.directoryClaims[entry.claim.id] != entry || entry.state != directorySettling ||
			entry.finalizationOperation == nil {
			session.mu.Unlock()
			return ErrExecutorContract
		}
		if session.requiredFault.Valid() {
			err := session.operationRejectionLocked()
			session.mu.Unlock()
			return err
		}
		if entry.activeDescendants == 0 {
			children := entry.directUnsettledChildren
			session.mu.Unlock()
			if children != 0 {
				return ErrDirectoryChildrenUnsettled
			}
			return nil
		}
		changed := entry.changed
		session.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-closing:
			return sessionClosedError()
		}
	}
}

func (session *Session) directorySettlement(
	admission transfer.DirectoryAdmission,
	observation DirectoryFinalization,
) (transfer.DirectorySettlement, error) {
	if observation.Cut != MutationStable {
		return transfer.DirectorySettlement{}, ErrExecutorContract
	}
	switch observation.Kind {
	case DirectoryFinalizationFinalized:
		if !observation.Failure.IsZero() {
			return transfer.DirectorySettlement{}, ErrExecutorContract
		}
		return transfer.NewFinalizedDirectorySettlement(admission)
	case DirectoryFinalizationIsolatedFailure:
		return transfer.NewIsolatedDirectorySettlement(admission, observation.Failure)
	default:
		return transfer.DirectorySettlement{}, ErrExecutorContract
	}
}

func (session *Session) commitDirectoryFinalization(
	operationID uint64,
	entry *directoryEntry,
	settlement transfer.DirectorySettlement,
) (transfer.DirectorySettlement, error) {
	session.mu.Lock()
	if session.directoryClaims[entry.claim.id] != entry || entry.state != directorySettling ||
		entry.finalizationOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return transfer.DirectorySettlement{}, err
	}
	operation := entry.finalizationOperation
	entry.finalizationOperation = nil
	entry.state = directorySettled
	entry.settlement = settlement
	operation.settlement = settlement
	if err := session.adjustActiveAncestorsLocked(entry.claim.parent, false); err != nil {
		err = session.markInvariantFailureLocked()
		operation.err = err
		close(operation.done)
		session.mu.Unlock()
		return transfer.DirectorySettlement{}, err
	}
	if entry.claim.parent != 0 {
		parent := session.directoryClaims[entry.claim.parent]
		if parent == nil || parent.directUnsettledChildren == 0 {
			err := session.markInvariantFailureLocked()
			operation.err = err
			close(operation.done)
			session.mu.Unlock()
			return transfer.DirectorySettlement{}, err
		}
		parent.directUnsettledChildren--
		parent.notifyLocked()
	}
	close(operation.done)
	value, _ := settlement.IsolatedFault()
	event := session.traceLocked(operationID, OperationFinalizeDirectory, TraceSettled, entry.claim.id,
		ClaimDirectory, ClaimSettling, ClaimSettled, value)
	session.mu.Unlock()
	session.emit(event)
	return settlement, nil
}

func (session *Session) failDirectoryFinalization(
	ctx context.Context,
	operationID uint64,
	entry *directoryEntry,
	cut MutationCut,
	cause error,
) error {
	session.mu.Lock()
	if session.directoryClaims[entry.claim.id] != entry || entry.state != directorySettling ||
		entry.finalizationOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return err
	}
	value, normalized := session.normalizeFailureLocked(ctx, cause, cut)
	operation := entry.finalizationOperation
	entry.finalizationOperation = nil
	if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
		normalized = joinFailures(ctx, normalized, session.markInvariantFailureLocked())
		value = fault.Join(value, session.requiredFault)
	}
	operation.err = normalized
	decision := TraceRolledBack
	if cut != MutationNoChange {
		entry.uncertain = true
		session.attention = true
		decision = TraceAmbiguous
	}
	close(operation.done)
	event := session.traceLocked(operationID, OperationFinalizeDirectory, decision, entry.claim.id,
		ClaimDirectory, ClaimSettling, ClaimSettling, value)
	session.mu.Unlock()
	session.emit(event)
	return normalized
}

func (session *Session) finishDirectoryFinalizationWithoutMutation(
	operationID uint64,
	entry *directoryEntry,
	cause error,
) error {
	session.mu.Lock()
	if session.directoryClaims[entry.claim.id] != entry || entry.finalizationOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return err
	}
	operation := entry.finalizationOperation
	entry.finalizationOperation = nil
	var value fault.Fault
	if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
		cause = joinCompletedFailures(cause, session.markInvariantFailureLocked())
		value = session.requiredFault
	}
	operation.err = cause
	close(operation.done)
	event := session.traceLocked(operationID, OperationFinalizeDirectory, TraceRolledBack, entry.claim.id,
		ClaimDirectory, ClaimSettling, ClaimSettling, value)
	session.mu.Unlock()
	session.emit(event)
	return cause
}

func waitDirectoryFinalization(
	ctx context.Context,
	closing <-chan struct{},
	operation *directoryFinalizationOperation,
) (transfer.DirectorySettlement, error) {
	select {
	case <-operation.done:
		return operation.settlement, operation.err
	case <-ctx.Done():
		return transfer.DirectorySettlement{}, ctx.Err()
	case <-closing:
		return transfer.DirectorySettlement{}, sessionClosedError()
	}
}
