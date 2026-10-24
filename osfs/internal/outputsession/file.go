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
	file transfer.MaterializationFile,
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

	artifactLocatorKey, destination, destinationLocatorKey, canonicalErr :=
		session.bindMaterializedArtifact(file.ArtifactPath())
	if canonicalErr != nil {
		return transfer.FileStart{}, session.rejectFileBinding(operationID, 0, canonicalErr)
	}
	if err := ctx.Err(); err != nil {
		return transfer.FileStart{}, err
	}

	entry, pending, cached, event, err := session.reserveFile(
		operationID, file, artifactLocatorKey, destination, destinationLocatorKey,
	)
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

func validateFileRequest(session *Session, file transfer.MaterializationFile) error {
	artifactPath := file.ArtifactPath().String()
	materializationPath := file.MaterializationRelativePath().String()
	canonicalArtifact, artifactErr := catalog.CanonicalPath(artifactPath)
	canonicalMaterialization, materializationErr := catalog.CanonicalPath(materializationPath)
	target := file.Target()
	descriptor := file.Descriptor()
	if artifactErr != nil || materializationErr != nil ||
		!transfer.MaterializationFileMatchesIntent(session.intent, file) ||
		!file.SourcePath().Valid() || !file.ArtifactPath().Valid() ||
		!file.MaterializationRelativePath().Valid() || artifactPath == "" || materializationPath == "" ||
		canonicalArtifact != artifactPath || canonicalMaterialization != materializationPath ||
		descriptor.ShareInstance() != session.intent.ShareInstance() ||
		descriptor.FileID().IsZero() || descriptor.FileRevision().IsZero() ||
		file.ExpectedSize() != descriptor.ExactSize() || target.OutputSessionID() != session.sessionID ||
		target.Descriptor() != descriptor ||
		target.ExactSize() != file.ExpectedSize() || target.Locator().IsZero() {
		return ErrDirectoryBinding
	}
	if target.Locator().Kind() == transfer.MaterializationPathLocator &&
		target.Locator().CanonicalPath() != materializationPath {
		return ErrDirectoryBinding
	}
	return nil
}

func (session *Session) reserveFile(
	operationID uint64,
	file transfer.MaterializationFile,
	artifactLocatorKey string,
	destination DestinationPath,
	destinationLocatorKey string,
) (*fileEntry, *fileBeginOperation, bool, TraceEvent, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	sourceParentID, sourceParent, err := session.validateFileParentLocked(file)
	if err != nil {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, sourceParentID),
			session.rejectDirectoryBindingLocked(err)
	}
	parentID, err := session.destinationParentLocked(file.ParentMaterialization(), destination)
	if err != nil {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, parentID),
			session.rejectDirectoryBindingLocked(err)
	}
	node := catalog.NodeID(file.Descriptor().FileID())
	nameKey := parentNameKey{parent: parentID, name: claimName(destination.String())}
	reference, exists, consistent := session.existingClaimLocked(
		node, file.SourcePath().String(), file.ArtifactPath().String(), artifactLocatorKey, nameKey,
	)
	if !consistent {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
			session.markInvariantFailureLocked()
	}
	if exists {
		entry := session.fileClaims[reference.id]
		if reference.kind != ClaimFile || entry == nil || entry.claim.file != file ||
			entry.claim.artifactLocatorKey != artifactLocatorKey ||
			entry.claim.destination != destination ||
			entry.claim.destinationLocatorKey != destinationLocatorKey {
			return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
				session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
		}
		if owner, claimed := session.locatorClaims[artifactLocatorKey]; !claimed || owner != reference {
			return nil, nil, false, session.fileBindingTraceLocked(operationID, reference.id),
				session.markInvariantFailureLocked()
		}
		if owner, claimed := session.destinationClaims[destinationLocatorKey]; !claimed || owner != reference {
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
	if sourceParent != nil && (sourceParent.state != directoryAdmitted || sourceParent.uncertain) {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, parentID),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, exists := session.locatorClaims[artifactLocatorKey]; exists {
		return nil, nil, false, session.fileBindingTraceLocked(operationID, owner.id),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, exists := session.destinationClaims[destinationLocatorKey]; exists {
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
		claim: FileClaim{
			id: claimID, file: file, artifactLocatorKey: artifactLocatorKey,
			destination: destination, destinationLocatorKey: destinationLocatorKey, parent: parentID,
		},
		state: filePending, beginOperation: operation,
	}
	reference = claimRef{kind: ClaimFile, id: claimID}
	session.fileClaims[claimID] = entry
	session.nodeClaims[node] = reference
	session.pathClaims[file.SourcePath().String()] = reference
	session.artifactClaims[file.ArtifactPath().String()] = reference
	session.locatorClaims[artifactLocatorKey] = reference
	session.destinationClaims[destinationLocatorKey] = reference
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

func (session *Session) validateFileParentLocked(
	file transfer.MaterializationFile,
) (ClaimID, *directoryEntry, error) {
	parent := file.Parent()
	switch parent.Kind() {
	case transfer.MaterializationFileParentReference:
		if session.scope.RootExpectation().Kind() != transfer.DirectoryAdmissionNoRoot ||
			parent.SourcePath().String() != parentPath(file.SourcePath().String()) ||
			file.ParentMaterialization().Valid() {
			return 0, nil, ErrDirectoryBinding
		}
		return 0, nil, nil
	case transfer.MaterializationFileParentDirectory:
		sourceParentID, ok := session.receiptClaims[receiptKey(parent.Admission())]
		sourceParent := session.directoryClaims[sourceParentID]
		if !ok || sourceParent == nil || !sameAdmission(sourceParent.admission, parent.Admission()) ||
			sourceParent.claim.source.DirectoryID != parent.DirectoryID() ||
			sourceParent.claim.source.Generation != parent.Generation() ||
			sourceParent.claim.source.SourcePath != parent.SourcePath() ||
			parent.SourcePath().String() != parentPath(file.SourcePath().String()) {
			return sourceParentID, nil, ErrDirectoryBinding
		}
		return sourceParentID, sourceParent, nil
	default:
		return 0, nil, ErrDirectoryBinding
	}
}

func (session *Session) destinationParentLocked(
	claim transfer.MaterializedDirectoryClaim,
	destination DestinationPath,
) (ClaimID, error) {
	if !destination.Valid() {
		return 0, ErrDirectoryBinding
	}
	destinationParent := parentPath(destination.String())
	if !claim.Valid() {
		if destination.IsSessionRoot() {
			return 0, nil
		}
		if destinationParent != "" {
			return 0, ErrDirectoryBinding
		}
		return 0, nil
	}
	if destination.IsSessionRoot() {
		return 0, ErrDirectoryBinding
	}
	claimID, ok := session.receiptClaims[receiptKey(claim.Admission())]
	parent := session.directoryClaims[claimID]
	if !ok || parent == nil || parent.admission.Path() != claim.Path().String() ||
		parent.state != directoryAdmitted || parent.uncertain {
		return 0, ErrDirectoryBinding
	}
	if parent.claim.IsSessionRoot() {
		if destinationParent != "" {
			return 0, ErrDirectoryBinding
		}
		return claimID, nil
	}
	if parent.claim.destination.String() != destinationParent {
		return 0, ErrDirectoryBinding
	}
	return claimID, nil
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
		if binding.Target() != entry.claim.file.Target() || observation.Durable.Binding() != binding ||
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
		if err != nil || observation.Settlement.Target() != entry.claim.file.Target() ||
			observation.Durable.Binding() != (transfer.MaterializedFileBinding{}) {
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
	node := catalog.NodeID(entry.claim.file.Descriptor().FileID())
	if session.nodeClaims[node] == reference {
		delete(session.nodeClaims, node)
	}
	if session.pathClaims[entry.claim.file.SourcePath().String()] == reference {
		delete(session.pathClaims, entry.claim.file.SourcePath().String())
	}
	if session.artifactClaims[entry.claim.file.ArtifactPath().String()] == reference {
		delete(session.artifactClaims, entry.claim.file.ArtifactPath().String())
	}
	nameKey := parentNameKey{
		parent: entry.claim.parent, name: claimName(entry.claim.destination.String()),
	}
	if session.nameClaims[nameKey] == reference {
		delete(session.nameClaims, nameKey)
	}
	if session.locatorClaims[entry.claim.artifactLocatorKey] == reference {
		delete(session.locatorClaims, entry.claim.artifactLocatorKey)
	}
	if session.destinationClaims[entry.claim.destinationLocatorKey] == reference {
		delete(session.destinationClaims, entry.claim.destinationLocatorKey)
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
