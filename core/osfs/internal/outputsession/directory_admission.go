package outputsession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func (session *Session) AdmitDirectory(
	ctx context.Context,
	directory transfer.MaterializationDirectory,
) (transfer.DirectoryAdmission, error) {
	lease, operationID, err := session.beginOperation()
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	defer lease.release()
	if ctx == nil {
		return transfer.DirectoryAdmission{}, executorContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectoryAdmission{}, err
	}

	admission, err := transfer.NewDirectoryAdmissionWithSecret(session.secret[:], session.scope, directory)
	if err != nil {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, err)
	}
	locatorKey, canonicalErr := session.locator.CanonicalLocatorKey(directory.Path)
	if canonicalErr != nil || !validCanonicalLocatorKey(directory.Path, locatorKey) {
		if canonicalErr == nil {
			canonicalErr = ErrExecutorContract
		}
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, canonicalErr)
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectoryAdmission{}, err
	}

	entry, pending, cached, event, err := session.reserveDirectory(
		operationID, directory, admission, locatorKey,
	)
	session.emit(event)
	if err != nil {
		return transfer.DirectoryAdmission{}, err
	}
	if cached {
		return entry.admission, nil
	}
	if pending != nil {
		return waitDirectoryAdmission(ctx, lease.closing(), pending)
	}

	observation, executeErr := session.directories.MaterializeDirectory(ctx, entry.claim)
	if executeErr != nil {
		cut := observation.Cut
		if !cut.valid() || cut == MutationStable {
			cut = MutationAmbiguous
			executeErr = executorContractError(executeErr)
		}
		return transfer.DirectoryAdmission{}, session.failDirectoryAdmission(
			ctx, operationID, entry, cut, executeErr,
		)
	}
	if observation.Cut != MutationStable || !observation.Disposition.validFor(entry.claim.IsRoot()) {
		return transfer.DirectoryAdmission{}, session.failDirectoryAdmission(
			ctx, operationID, entry, MutationAmbiguous, ErrExecutorContract,
		)
	}
	return session.commitDirectoryAdmission(operationID, entry, observation.Disposition)
}

func (session *Session) reserveDirectory(
	operationID uint64,
	directory transfer.MaterializationDirectory,
	admission transfer.DirectoryAdmission,
	locatorKey string,
) (*directoryEntry, *directoryAdmissionOperation, bool, TraceEvent, error) {
	session.mu.Lock()
	defer session.mu.Unlock()

	parent, err := session.directoryParentLocked(directory)
	if err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, 0, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(err)
	}
	node := catalog.NodeID(directory.DirectoryID)
	nameKey := parentNameKey{parent: parent, name: claimName(directory.Path)}
	reference, exists, consistent := session.existingClaimLocked(node, directory.Path, nameKey)
	if !consistent {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.markInvariantFailureLocked()
	}
	if exists {
		return session.existingDirectoryReservationLocked(operationID, reference, directory, admission, locatorKey)
	}
	if err := session.operationRejectionLocked(); err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, 0, OperationAdmitDirectory), err
	}
	if parent != 0 {
		parentEntry := session.directoryClaims[parent]
		if parentEntry == nil || parentEntry.state != directoryAdmitted || parentEntry.uncertain {
			return nil, nil, false, session.bindingTraceLocked(operationID, parent, OperationAdmitDirectory),
				session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
		}
	}
	if owner, exists := session.locatorClaims[locatorKey]; exists {
		return nil, nil, false, session.bindingTraceLocked(operationID, owner.id, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	charge := directoryMetadataBytes(directory, locatorKey)
	overMetadataBudget := session.metadataBytes > session.limits.DirectoryMetadataBytes ||
		charge > session.limits.DirectoryMetadataBytes-session.metadataBytes
	if uint64(len(session.directoryClaims)) >= session.limits.DirectoryClaims ||
		uint64(len(session.nodeClaims)) >= session.limits.NodeClaims || overMetadataBudget {
		value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputResourceBudget)
		session.requirePauseLocked(value, false)
		return nil, nil, false,
			session.traceLocked(operationID, OperationAdmitDirectory, TraceRejected, 0, ClaimDirectory,
				ClaimPending, ClaimPending, value), resourceBudgetError()
	}
	claimID, err := session.nextClaimLocked()
	if err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, 0, OperationAdmitDirectory), err
	}
	operation := &directoryAdmissionOperation{done: make(chan struct{})}
	entry := &directoryEntry{
		claim: DirectoryClaim{
			id: claimID, directory: directory, admission: admission, locatorKey: locatorKey, parent: parent,
		},
		admission: admission, state: directoryPending, metadataBytes: charge,
		changed: make(chan struct{}), admissionOperation: operation,
	}
	reference = claimRef{kind: ClaimDirectory, id: claimID}
	session.directoryClaims[claimID] = entry
	session.nodeClaims[node] = reference
	session.pathClaims[directory.Path] = reference
	session.locatorClaims[locatorKey] = reference
	session.nameClaims[nameKey] = reference
	session.metadataBytes += charge
	if parent == 0 {
		session.rootClaim = claimID
	}
	if err := session.adjustActiveAncestorsLocked(parent, true); err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, claimID, OperationAdmitDirectory),
			session.markInvariantFailureLocked()
	}
	return entry, nil, false,
		session.traceLocked(operationID, OperationAdmitDirectory, TraceReserved, claimID, ClaimDirectory,
			ClaimPending, ClaimPending, fault.Fault{}), nil
}

func (session *Session) existingDirectoryReservationLocked(
	operationID uint64,
	reference claimRef,
	directory transfer.MaterializationDirectory,
	admission transfer.DirectoryAdmission,
	locatorKey string,
) (*directoryEntry, *directoryAdmissionOperation, bool, TraceEvent, error) {
	entry := session.directoryClaims[reference.id]
	if reference.kind != ClaimDirectory || entry == nil || entry.claim.directory != directory ||
		entry.claim.locatorKey != locatorKey || !sameAdmission(entry.admission, admission) {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, claimed := session.locatorClaims[locatorKey]; !claimed || owner != reference {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.markInvariantFailureLocked()
	}
	if entry.admissionOperation != nil {
		return entry, entry.admissionOperation, false,
			session.traceLocked(operationID, OperationAdmitDirectory, TraceCoalesced, entry.claim.id,
				ClaimDirectory, ClaimPending, ClaimPending, fault.Fault{}), nil
	}
	if entry.state != directoryPending && !entry.admission.IsZero() {
		if owner, claimed := session.receiptClaims[receiptKey(entry.admission)]; !claimed || owner != entry.claim.id {
			return nil, nil, false,
				session.bindingTraceLocked(operationID, entry.claim.id, OperationAdmitDirectory),
				session.markInvariantFailureLocked()
		}
		state := directoryClaimState(entry.state)
		return entry, nil, true,
			session.traceLocked(operationID, OperationAdmitDirectory, TraceAdmitted, entry.claim.id,
				ClaimDirectory, state, state, fault.Fault{}), nil
	}
	return nil, nil, false, session.bindingTraceLocked(operationID, entry.claim.id, OperationAdmitDirectory),
		session.operationRejectionOrInvariantLocked()
}

func (session *Session) directoryParentLocked(directory transfer.MaterializationDirectory) (ClaimID, error) {
	if directory.Path == "" {
		if session.rootClaim != 0 && session.directoryClaims[session.rootClaim] == nil {
			return 0, ErrExecutorContract
		}
		return 0, nil
	}
	claimID, ok := session.receiptClaims[receiptKey(directory.ParentAdmission)]
	if !ok {
		return 0, ErrDirectoryBinding
	}
	parent := session.directoryClaims[claimID]
	if parent == nil || !sameAdmission(parent.admission, directory.ParentAdmission) ||
		parent.claim.directory.Path != parentPath(directory.Path) {
		return 0, ErrDirectoryBinding
	}
	return claimID, nil
}

func (session *Session) existingClaimLocked(
	node catalog.NodeID,
	path string,
	name parentNameKey,
) (claimRef, bool, bool) {
	candidates := make([]claimRef, 0, 3)
	if value, ok := session.nodeClaims[node]; ok {
		candidates = append(candidates, value)
	}
	if value, ok := session.pathClaims[path]; ok {
		candidates = append(candidates, value)
	}
	if value, ok := session.nameClaims[name]; ok {
		candidates = append(candidates, value)
	}
	var found claimRef
	for index, candidate := range candidates {
		if index == 0 {
			found = candidate
			continue
		}
		if candidate != found {
			return found, true, false
		}
	}
	return found, len(candidates) != 0, true
}

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

func (session *Session) removeDirectoryReservationLocked(entry *directoryEntry) {
	reference := claimRef{kind: ClaimDirectory, id: entry.claim.id}
	delete(session.directoryClaims, entry.claim.id)
	if session.nodeClaims[catalog.NodeID(entry.claim.directory.DirectoryID)] == reference {
		delete(session.nodeClaims, catalog.NodeID(entry.claim.directory.DirectoryID))
	}
	if session.pathClaims[entry.claim.directory.Path] == reference {
		delete(session.pathClaims, entry.claim.directory.Path)
	}
	nameKey := parentNameKey{parent: entry.claim.parent, name: claimName(entry.claim.directory.Path)}
	if session.nameClaims[nameKey] == reference {
		delete(session.nameClaims, nameKey)
	}
	if session.locatorClaims[entry.claim.locatorKey] == reference {
		delete(session.locatorClaims, entry.claim.locatorKey)
	}
	session.metadataBytes -= entry.metadataBytes
	if session.rootClaim == entry.claim.id {
		session.rootClaim = 0
	}
}

func waitDirectoryAdmission(
	ctx context.Context,
	closing <-chan struct{},
	operation *directoryAdmissionOperation,
) (transfer.DirectoryAdmission, error) {
	select {
	case <-operation.done:
		return operation.admission, operation.err
	case <-ctx.Done():
		return transfer.DirectoryAdmission{}, ctx.Err()
	case <-closing:
		return transfer.DirectoryAdmission{}, sessionClosedError()
	}
}

func (session *Session) rejectDirectoryBinding(operationID uint64, claimID ClaimID, cause error) error {
	session.mu.Lock()
	err := session.rejectDirectoryBindingLocked(cause)
	event := session.bindingTraceLocked(operationID, claimID, OperationAdmitDirectory)
	session.mu.Unlock()
	session.emit(event)
	return err
}

func (session *Session) rejectDirectoryBindingLocked(cause error) error {
	value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputDirectoryBinding)
	session.requirePauseLocked(value, false)
	return outputFault(fault.ScopeOutputPause, fault.OutputDirectoryBinding, errors.Join(ErrDirectoryBinding, cause))
}

func (session *Session) bindingTraceLocked(
	operationID uint64,
	claimID ClaimID,
	operation OperationKind,
) TraceEvent {
	value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputDirectoryBinding)
	return session.traceLocked(operationID, operation, TraceRejected, claimID, ClaimDirectory,
		ClaimPending, ClaimPending, value)
}
