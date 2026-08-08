package outputsession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
)

func (session *Session) BeginFile(
	ctx context.Context,
	file transfer.OutputFile,
) (transfer.FileStart, error) {
	lease, operationID, err := session.beginOperation()
	if err != nil {
		return transfer.FileStart{}, err
	}
	defer lease.release()
	if ctx == nil {
		return transfer.FileStart{}, executorContractError(ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}
	if err := validateFileRequest(session, file); err != nil {
		return transfer.FileStart{}, session.rejectFileBinding(operationID, 0, err)
	}

	locatorKey, canonicalErr := session.locator.CanonicalLocatorKey(file.Path)
	if canonicalErr != nil || !validCanonicalLocatorKey(file.Path, locatorKey) {
		if canonicalErr == nil {
			canonicalErr = ErrExecutorContract
		}
		return transfer.FileStart{}, session.rejectFileBinding(operationID, 0, canonicalErr)
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}

	entry, pending, cached, event, err := session.reserveFile(operationID, file, locatorKey)
	session.emit(event)
	if err != nil {
		return transfer.FileStart{}, err
	}
	if cached {
		start, startErr := transfer.NewFileSettlementStart(entry.settlement)
		if startErr != nil {
			return transfer.FileStart{}, outputFault(
				fault.ScopeFileLocal, fault.OutputContract, errors.Join(ErrConflictingSettlement, startErr),
			)
		}
		return start, nil
	}
	if pending != nil {
		return waitFileBegin(ctx, lease.closing(), pending)
	}

	observation, executeErr := session.files.BeginFile(ctx, entry.claim)
	if executeErr != nil {
		cut := observation.Cut
		// Without a returned transaction or settlement, even a reconciled backend
		// state is not usable authority for this runtime claim.
		if !cut.valid() || cut == MutationStable {
			cut = MutationAmbiguous
			executeErr = executorContractError(executeErr)
		}
		return transfer.FileStart{}, session.failFileBegin(ctx, operationID, entry, cut, executeErr)
	}
	if observation.Cut != MutationStable {
		return transfer.FileStart{}, session.failFileBegin(
			ctx, operationID, entry, MutationAmbiguous, ErrExecutorContract,
		)
	}
	return session.commitFileBegin(ctx, operationID, entry, observation)
}

func validateFileRequest(session *Session, file transfer.OutputFile) error {
	canonical, err := catalog.CanonicalPath(file.Path)
	target := file.Target
	descriptor := file.Descriptor
	if err != nil || file.Path == "" || canonical != file.Path || file.ParentAdmission.IsZero() ||
		descriptor.ShareInstance() != session.intent.ShareInstance() ||
		descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() ||
		file.ExpectedSize != descriptor.ExactSize() || target.BackendID() != session.BackendID() ||
		target.OutputSessionID() != session.sessionID || target.Descriptor() != descriptor ||
		target.ExactSize() != file.ExpectedSize || target.Locator().IsZero() {
		return ErrDirectoryBinding
	}
	if target.Locator().Kind() == transfer.OutputPathLocator && target.Locator().CanonicalPath() != file.Path {
		return ErrDirectoryBinding
	}
	return nil
}

func (session *Session) reserveFile(
	operationID uint64,
	file transfer.OutputFile,
	locatorKey string,
) (*fileEntry, *fileBeginOperation, bool, TraceEvent, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	parentID, ok := session.receiptClaims[receiptKey(file.ParentAdmission)]
	parent := session.directoryClaims[parentID]
	if !ok || parent == nil || !sameAdmission(parent.admission, file.ParentAdmission) ||
		parent.claim.directory.Path != parentPath(file.Path) {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, parentID),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	node := catalog.NodeID(file.Descriptor.FileID())
	nameKey := parentNameKey{parent: parentID, name: claimName(file.Path)}
	reference, exists, consistent := session.existingClaimLocked(node, file.Path, nameKey)
	if !consistent {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
			session.markInvariantFailureLocked()
	}
	if exists {
		entry := session.fileClaims[reference.id]
		if reference.kind != ClaimFile || entry == nil || entry.claim.file != file ||
			entry.claim.locatorKey != locatorKey {
			return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
				session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
		}
		if owner, claimed := session.locatorClaims[locatorKey]; !claimed || owner != reference {
			return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
				session.markInvariantFailureLocked()
		}
		switch {
		case entry.beginOperation != nil:
			return entry, entry.beginOperation, false,
				session.traceLocked(operationID, OperationBeginFile, TraceCoalesced, entry.claim.id,
					ClaimFile, ClaimPending, ClaimPending, fault.Fault{}), nil
		case entry.state == fileActive && !entry.uncertain:
			value, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputFileAlreadyActive)
			return nil, nil, false,
				session.traceLocked(operationID, OperationBeginFile, TraceRejected, entry.claim.id,
					ClaimFile, ClaimActive, ClaimActive, value), alreadyActiveError()
		case entry.state == fileSettled:
			return entry, nil, true,
				session.traceLocked(operationID, OperationBeginFile, TraceSettled, entry.claim.id,
					ClaimFile, ClaimSettled, ClaimSettled, fault.Fault{}), nil
		default:
			return nil, nil, false, session.fileBindingTraceLocked(operationID, entry.claim.id),
				session.operationRejectionOrInvariantLocked()
		}
	}
	if err := session.operationRejectionLocked(); err != nil {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, 0), err
	}
	if parent.state != directoryAdmitted || parent.uncertain {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, parentID),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, exists := session.locatorClaims[locatorKey]; exists {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, owner.id),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if uint64(len(session.nodeClaims)) >= session.limits.NodeClaims ||
		session.fileSlots >= session.limits.ActiveFileClaims {
		value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputResourceBudget)
		session.requirePauseLocked(value, false)
		return nil, nil, false,
			session.traceLocked(operationID, OperationBeginFile, TraceRejected, 0, ClaimFile,
				ClaimPending, ClaimPending, value), resourceBudgetError()
	}
	claimID, err := session.nextClaimLocked()
	if err != nil {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, 0), err
	}
	operation := &fileBeginOperation{done: make(chan struct{})}
	entry := &fileEntry{
		claim: FileClaim{id: claimID, file: file, locatorKey: locatorKey, parent: parentID},
		state: filePending, beginOperation: operation,
	}
	reference = claimRef{kind: ClaimFile, id: claimID}
	session.fileClaims[claimID] = entry
	session.nodeClaims[node] = reference
	session.pathClaims[file.Path] = reference
	session.locatorClaims[locatorKey] = reference
	session.nameClaims[nameKey] = reference
	session.fileSlots++
	if err := session.adjustActiveAncestorsLocked(parentID, true); err != nil {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, claimID),
			session.markInvariantFailureLocked()
	}
	return entry, nil, false,
		session.traceLocked(operationID, OperationBeginFile, TraceReserved, claimID, ClaimFile,
			ClaimPending, ClaimPending, fault.Fault{}), nil
}

func (session *Session) commitFileBegin(
	ctx context.Context,
	operationID uint64,
	entry *fileEntry,
	observation FileBeginObservation,
) (transfer.FileStart, error) {
	var (
		start       transfer.FileStart
		transaction *guardedTransaction
		settlement  transfer.FileSettlement
		terminal    bool
	)
	if observation.Transaction != nil {
		binding := observation.Transaction.Binding()
		if binding.Target() != entry.claim.file.Target || observation.Durable.Binding() != binding ||
			observation.Settlement.Kind() != 0 {
			return transfer.FileStart{}, session.failFileBegin(
				ctx, operationID, entry, MutationAmbiguous, ErrExecutorContract,
			)
		}
		transaction = &guardedTransaction{
			session: session, claimID: entry.claim.id, executor: observation.Transaction, binding: binding,
		}
		var err error
		start, err = transfer.NewFileTransactionStart(transaction, observation.Durable)
		if err != nil {
			return transfer.FileStart{}, session.failFileBegin(
				ctx, operationID, entry, MutationAmbiguous, err,
			)
		}
	} else {
		var err error
		start, err = transfer.NewFileSettlementStart(observation.Settlement)
		if err != nil || observation.Settlement.Target() != entry.claim.file.Target ||
			observation.Durable.Binding() != (transfer.OutputFileBinding{}) {
			return transfer.FileStart{}, session.failFileBegin(
				ctx, operationID, entry, MutationAmbiguous, executorContractError(err),
			)
		}
		settlement = observation.Settlement
		terminal = true
	}

	session.mu.Lock()
	if session.fileClaims[entry.claim.id] != entry || entry.state != filePending || entry.beginOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return transfer.FileStart{}, err
	}
	operation := entry.beginOperation
	entry.beginOperation = nil
	operation.start = start
	from, to, decision := ClaimPending, ClaimActive, TraceActive
	var (
		value     fault.Fault
		commitErr error
	)
	if terminal {
		entry.state = fileSettled
		entry.settlement = settlement
		entry.terminalAction = actionBeginSettlement
		session.fileSlots--
		if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
			commitErr = session.markInvariantFailureLocked()
			operation.err = commitErr
			value = session.requiredFault
		}
		to, decision = ClaimSettled, TraceSettled
		if commitErr != nil {
			decision = TraceRejected
		}
		if fileSettlementNeedsAttention(settlement) {
			session.attention = true
		}
	} else {
		entry.state = fileActive
		entry.transaction = transaction
		session.activeFiles++
	}
	close(operation.done)
	event := session.traceLocked(operationID, OperationBeginFile, decision, entry.claim.id,
		ClaimFile, from, to, value)
	session.mu.Unlock()
	session.emit(event)
	return start, commitErr
}

func (session *Session) failFileBegin(
	ctx context.Context,
	operationID uint64,
	entry *fileEntry,
	cut MutationCut,
	cause error,
) error {
	session.mu.Lock()
	if session.fileClaims[entry.claim.id] != entry || entry.state != filePending || entry.beginOperation == nil {
		err := session.markInvariantFailureLocked()
		session.mu.Unlock()
		return err
	}
	value, normalized := session.normalizeFailureLocked(ctx, cause, cut)
	decision := TraceRolledBack
	if cut == MutationNoChange {
		normalized = session.completeFileBeginRollbackLocked(ctx, entry, normalized)
	} else {
		operation := entry.beginOperation
		entry.beginOperation = nil
		entry.uncertain = true
		if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
			normalized = joinFailures(ctx, normalized, session.markInvariantFailureLocked())
			value = fault.Join(value, session.requiredFault)
		}
		operation.err = normalized
		session.attention = true
		close(operation.done)
		decision = TraceAmbiguous
	}
	event := session.traceLocked(operationID, OperationBeginFile, decision, entry.claim.id,
		ClaimFile, ClaimPending, ClaimPending, value)
	session.mu.Unlock()
	session.emit(event)
	return normalized
}

func (session *Session) completeFileBeginRollbackLocked(
	ctx context.Context,
	entry *fileEntry,
	err error,
) error {
	operation := entry.beginOperation
	entry.beginOperation = nil
	if ancestorErr := session.adjustActiveAncestorsLocked(entry.claim.parent, false); ancestorErr != nil {
		err = joinFailures(ctx, err, session.markInvariantFailureLocked())
	}
	operation.err = err
	session.removeFileReservationLocked(entry)
	close(operation.done)
	return err
}

func (session *Session) removeFileReservationLocked(entry *fileEntry) {
	reference := claimRef{kind: ClaimFile, id: entry.claim.id}
	delete(session.fileClaims, entry.claim.id)
	node := catalog.NodeID(entry.claim.file.Descriptor.FileID())
	if session.nodeClaims[node] == reference {
		delete(session.nodeClaims, node)
	}
	if session.pathClaims[entry.claim.file.Path] == reference {
		delete(session.pathClaims, entry.claim.file.Path)
	}
	nameKey := parentNameKey{parent: entry.claim.parent, name: claimName(entry.claim.file.Path)}
	if session.nameClaims[nameKey] == reference {
		delete(session.nameClaims, nameKey)
	}
	if session.locatorClaims[entry.claim.locatorKey] == reference {
		delete(session.locatorClaims, entry.claim.locatorKey)
	}
	session.fileSlots--
}

func waitFileBegin(
	ctx context.Context,
	closing <-chan struct{},
	operation *fileBeginOperation,
) (transfer.FileStart, error) {
	select {
	case <-operation.done:
		if _, _, active := operation.start.Transaction(); active && operation.err == nil {
			return transfer.FileStart{}, alreadyActiveError()
		}
		return operation.start, operation.err
	case <-ctx.Done():
		return transfer.FileStart{}, ctx.Err()
	case <-closing:
		return transfer.FileStart{}, sessionClosedError()
	}
}

func fileSettlementNeedsAttention(settlement transfer.FileSettlement) bool {
	return settlement.Kind() == transfer.FilePublishBlocked || settlement.Kind() == transfer.FileQuarantined
}

func (session *Session) rejectFileBinding(operationID uint64, claimID ClaimID, cause error) error {
	session.mu.Lock()
	err := session.rejectDirectoryBindingLocked(cause)
	event := session.fileBindingTraceLocked(operationID, claimID)
	session.mu.Unlock()
	session.emit(event)
	return err
}

func (session *Session) fileBindingTraceLocked(operationID uint64, claimID ClaimID) TraceEvent {
	value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputDirectoryBinding)
	return session.traceLocked(operationID, OperationBeginFile, TraceRejected, claimID, ClaimFile,
		ClaimPending, ClaimPending, value)
}
