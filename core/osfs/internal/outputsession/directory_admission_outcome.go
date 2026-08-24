package outputsession

import (
	"context"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func (session *Session) commitDirectoryAdmission(
	operationID uint64,
	entry *directoryEntry,
	disposition DirectoryDisposition,
) (transfer.DirectoryAdmission, error) {
	session.mu.Lock()
	if session.directoryClaims[entry.claim.id] != entry || entry.state != directoryPending ||
		entry.admissionOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return transfer.DirectoryAdmission{}, err
	}
	key := receiptKey(entry.admission)
	if owner, exists := session.receiptClaims[key]; exists && owner != entry.claim.id {
		err := session.completeDirectoryAdmissionAmbiguousLocked(entry, executorContractError(ErrExecutorContract))
		event := session.traceLocked(operationID, OperationAdmitDirectory, TraceAmbiguous, entry.claim.id,
			ClaimDirectory, ClaimPending, ClaimPending, session.requiredFault)
		session.mu.Unlock()
		session.emit(event)
		return transfer.DirectoryAdmission{}, err
	}
	entry.state = directoryAdmitted
	entry.disposition = disposition
	operation := entry.admissionOperation
	entry.admissionOperation = nil
	operation.admission = entry.admission
	session.receiptClaims[key] = entry.claim.id
	if err := session.adjustActiveAncestorsLocked(entry.claim.parent, false); err != nil {
		err = session.markInvariantFailureLocked()
		operation.err = err
		close(operation.done)
		session.mu.Unlock()
		return transfer.DirectoryAdmission{}, err
	}
	if entry.claim.parent != 0 {
		parent := session.directoryClaims[entry.claim.parent]
		parent.directUnsettledChildren++
		parent.notifyLocked()
	}
	close(operation.done)
	event := session.traceLocked(operationID, OperationAdmitDirectory, TraceAdmitted, entry.claim.id,
		ClaimDirectory, ClaimPending, ClaimAdmitted, fault.Fault{})
	session.mu.Unlock()
	session.emit(event)
	return entry.admission, nil
}

func (session *Session) failDirectoryAdmission(
	ctx context.Context,
	operationID uint64,
	entry *directoryEntry,
	cut MutationCut,
	cause error,
) error {
	session.mu.Lock()
	if session.directoryClaims[entry.claim.id] != entry || entry.state != directoryPending ||
		entry.admissionOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return err
	}
	value, normalized := session.normalizeFailureLocked(ctx, cause, cut)
	decision := TraceRolledBack
	if cut == MutationNoChange {
		normalized = session.completeDirectoryAdmissionRollbackLocked(entry, normalized)
	} else {
		normalized = session.completeDirectoryAdmissionAmbiguousLocked(entry, normalized)
		decision = TraceAmbiguous
	}
	event := session.traceLocked(operationID, OperationAdmitDirectory, decision, entry.claim.id,
		ClaimDirectory, ClaimPending, ClaimPending, value)
	session.mu.Unlock()
	session.emit(event)
	return normalized
}

func (session *Session) completeDirectoryAdmissionRollbackLocked(
	entry *directoryEntry,
	err error,
) error {
	operation := entry.admissionOperation
	entry.admissionOperation = nil
	if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
		err = joinCompletedFailures(err, session.markInvariantFailureLocked())
	}
	operation.err = err
	session.removeDirectoryReservationLocked(entry)
	close(operation.done)
	return err
}

func (session *Session) completeDirectoryAdmissionAmbiguousLocked(entry *directoryEntry, err error) error {
	operation := entry.admissionOperation
	entry.admissionOperation = nil
	entry.uncertain = true
	if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
		err = joinCompletedFailures(err, session.markInvariantFailureLocked())
	}
	operation.err = err
	ambiguous, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputMutationAmbiguous)
	session.requirePauseLocked(ambiguous, true)
	close(operation.done)
	return err
}
