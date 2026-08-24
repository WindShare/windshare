package outputsession

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func (session *Session) AdmitDirectory(
	ctx context.Context,
	request transfer.DirectoryMaterializationRequest,
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
	if !transfer.DirectoryMaterializationMatchesIntent(session.intent, request) {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, ErrDirectoryBinding)
	}
	source := request.Source()
	directory, materialized := request.Directory()
	if !materialized {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, ErrDirectoryBinding)
	}
	admission, err := transfer.NewDirectoryAdmissionWithSecret(
		session.secret[:], session.scope, directory,
	)
	if err != nil {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, err)
	}
	artifact, materialize, err := directoryProjection(request.Projection())
	if err != nil {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, err)
	}
	if !materialize {
		return session.admitTraverseOnlyDirectory(operationID, source, admission)
	}
	artifactLocatorKey, destination, destinationLocatorKey, canonicalErr :=
		session.bindMaterializedArtifact(artifact)
	if canonicalErr != nil {
		return transfer.DirectoryAdmission{}, session.rejectDirectoryBinding(operationID, 0, canonicalErr)
	}
	if err := ctx.Err(); err != nil {
		return transfer.DirectoryAdmission{}, err
	}

	entry, pending, cached, event, err := session.reserveDirectory(
		operationID, source, admission, artifact, artifactLocatorKey,
		destination, destinationLocatorKey, request.ParentMaterialization(),
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
	if observation.Cut != MutationStable || !observation.Disposition.validFor(entry.claim.IsSessionRoot()) {
		return transfer.DirectoryAdmission{}, session.failDirectoryAdmission(
			ctx, operationID, entry, MutationAmbiguous, ErrExecutorContract,
		)
	}
	return session.commitDirectoryAdmission(operationID, entry, observation.Disposition)
}

func (session *Session) bindMaterializedArtifact(
	artifact ordinaryoutput.ArtifactPath,
) (string, DestinationPath, string, error) {
	if !artifact.Valid() {
		return "", DestinationPath{}, "", ErrDirectoryBinding
	}
	artifactLocatorKey, err := session.locator.CanonicalLocatorKey(artifact.String())
	if err != nil || !validCanonicalLocatorKey(artifact.String(), artifactLocatorKey) {
		if err == nil {
			err = ErrExecutorContract
		}
		return "", DestinationPath{}, "", err
	}
	destination, err := session.destinations.BindArtifactPath(artifact)
	if err != nil || !destination.Valid() {
		if err == nil {
			err = ErrExecutorContract
		}
		return "", DestinationPath{}, "", err
	}
	destinationLocatorKey, err := session.locator.CanonicalLocatorKey(destination.String())
	if err != nil || !validCanonicalLocatorKey(destination.String(), destinationLocatorKey) {
		if err == nil {
			err = ErrExecutorContract
		}
		return "", DestinationPath{}, "", err
	}
	return artifactLocatorKey, destination, destinationLocatorKey, nil
}

func directoryProjection(
	projection ordinaryoutput.ArtifactPathProjection,
) (ordinaryoutput.ArtifactPath, bool, error) {
	switch projection.Kind() {
	case ordinaryoutput.ArtifactTraverseOnly:
		return ordinaryoutput.ArtifactPath{}, false, nil
	case ordinaryoutput.ArtifactMaterialize:
		path, ok := projection.ArtifactPath()
		if !ok {
			return ordinaryoutput.ArtifactPath{}, false, ErrDirectoryBinding
		}
		return path, true, nil
	default:
		return ordinaryoutput.ArtifactPath{}, false, ErrDirectoryBinding
	}
}

func (session *Session) admitTraverseOnlyDirectory(
	operationID uint64,
	source transfer.AuthenticatedSourceDirectory,
	admission transfer.DirectoryAdmission,
) (transfer.DirectoryAdmission, error) {
	session.mu.Lock()
	result, event, err := session.admitTraverseOnlyDirectoryLocked(operationID, source, admission)
	session.mu.Unlock()
	session.emit(event)
	return result, err
}

func (session *Session) admitTraverseOnlyDirectoryLocked(
	operationID uint64,
	source transfer.AuthenticatedSourceDirectory,
	admission transfer.DirectoryAdmission,
) (transfer.DirectoryAdmission, TraceEvent, error) {
	parent, err := session.sourceParentLocked(source)
	if err != nil {
		err = session.rejectDirectoryBindingLocked(err)
		return transfer.DirectoryAdmission{}, session.bindingTraceLocked(
			operationID, parent, OperationAdmitDirectory,
		), err
	}
	node := catalog.NodeID(source.DirectoryID)
	sourcePath := source.SourcePath.String()
	reference, exists := session.sourceClaimLocked(node, sourcePath)
	if exists {
		entry := session.directoryClaims[reference.id]
		if reference.kind != ClaimDirectory || entry == nil || entry.claim.source != source ||
			!sameAdmission(entry.admission, admission) || entry.claim.artifact.Valid() {
			err = session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
			return transfer.DirectoryAdmission{}, session.bindingTraceLocked(
				operationID, reference.id, OperationAdmitDirectory,
			), err
		}
		state := directoryClaimState(entry.state)
		return entry.admission, session.traceLocked(
			operationID, OperationAdmitDirectory, TraceAdmitted, entry.claim.id,
			ClaimDirectory, state, state, fault.Fault{},
		), nil
	}
	if err := session.operationRejectionLocked(); err != nil {
		return transfer.DirectoryAdmission{}, session.bindingTraceLocked(
			operationID, parent, OperationAdmitDirectory,
		), err
	}
	charge := directoryMetadataBytes(
		source, ordinaryoutput.ArtifactPath{}, "", DestinationPath{}, "",
	)
	overMetadataBudget := session.metadataBytes > session.limits.DirectoryMetadataBytes ||
		charge > session.limits.DirectoryMetadataBytes-session.metadataBytes
	if uint64(len(session.directoryClaims)) >= session.limits.DirectoryClaims ||
		uint64(len(session.nodeClaims)) >= session.limits.NodeClaims || overMetadataBudget {
		value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputResourceBudget)
		session.requirePauseLocked(value, false)
		return transfer.DirectoryAdmission{}, session.traceLocked(
			operationID, OperationAdmitDirectory, TraceRejected, 0,
			ClaimDirectory, ClaimPending, ClaimPending, value,
		), resourceBudgetError()
	}
	claimID, err := session.nextClaimLocked()
	if err != nil {
		return transfer.DirectoryAdmission{}, session.bindingTraceLocked(
			operationID, 0, OperationAdmitDirectory,
		), err
	}
	entry := &directoryEntry{
		claim: DirectoryClaim{
			id: claimID, source: source, admission: admission, parent: parent,
		},
		admission: admission, state: directoryAdmitted, metadataBytes: charge,
		changed: make(chan struct{}),
	}
	reference = claimRef{kind: ClaimDirectory, id: claimID}
	session.directoryClaims[claimID] = entry
	session.nodeClaims[node] = reference
	session.pathClaims[sourcePath] = reference
	session.receiptClaims[receiptKey(admission)] = claimID
	session.metadataBytes += charge
	if parent == 0 {
		session.rootClaim = claimID
	} else if parentEntry := session.directoryClaims[parent]; parentEntry != nil {
		parentEntry.directUnsettledChildren++
	}
	return admission, session.traceLocked(
		operationID, OperationAdmitDirectory, TraceAdmitted, claimID,
		ClaimDirectory, ClaimPending, ClaimAdmitted, fault.Fault{},
	), nil
}

func (session *Session) reserveDirectory(
	operationID uint64,
	source transfer.AuthenticatedSourceDirectory,
	admission transfer.DirectoryAdmission,
	artifact ordinaryoutput.ArtifactPath,
	artifactLocatorKey string,
	destination DestinationPath,
	destinationLocatorKey string,
	parentMaterialization transfer.MaterializedDirectoryClaim,
) (*directoryEntry, *directoryAdmissionOperation, bool, TraceEvent, error) {
	session.mu.Lock()
	defer session.mu.Unlock()

	parent, err := session.sourceParentLocked(source)
	if err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, 0, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(err)
	}
	destinationParent, err := session.destinationParentLocked(parentMaterialization, destination)
	if err != nil {
		return nil, nil, false, session.bindingTraceLocked(operationID, parent, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(err)
	}
	node := catalog.NodeID(source.DirectoryID)
	sourcePath := source.SourcePath.String()
	nameKey := parentNameKey{parent: destinationParent, name: claimName(destination.String())}
	reference, exists, consistent := session.existingClaimLocked(
		node, sourcePath, artifact.String(), artifactLocatorKey, nameKey,
	)
	if !consistent {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.markInvariantFailureLocked()
	}
	if exists {
		return session.existingDirectoryReservationLocked(
			operationID, reference, source, admission, artifact, artifactLocatorKey,
			destination, destinationLocatorKey,
		)
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
	if owner, exists := session.locatorClaims[artifactLocatorKey]; exists {
		return nil, nil, false, session.bindingTraceLocked(operationID, owner.id, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, exists := session.destinationClaims[destinationLocatorKey]; exists {
		return nil, nil, false, session.bindingTraceLocked(operationID, owner.id, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	charge := directoryMetadataBytes(
		source, artifact, artifactLocatorKey, destination, destinationLocatorKey,
	)
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
			id: claimID, source: source, admission: admission, artifact: artifact,
			artifactLocatorKey: artifactLocatorKey, destination: destination,
			destinationLocatorKey: destinationLocatorKey,
			parent:                parent, destinationParent: destinationParent,
		},
		admission: admission, state: directoryPending, metadataBytes: charge,
		changed: make(chan struct{}), admissionOperation: operation,
	}
	reference = claimRef{kind: ClaimDirectory, id: claimID}
	session.directoryClaims[claimID] = entry
	session.nodeClaims[node] = reference
	session.pathClaims[sourcePath] = reference
	session.artifactClaims[artifact.String()] = reference
	session.locatorClaims[artifactLocatorKey] = reference
	session.destinationClaims[destinationLocatorKey] = reference
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
	source transfer.AuthenticatedSourceDirectory,
	admission transfer.DirectoryAdmission,
	artifact ordinaryoutput.ArtifactPath,
	artifactLocatorKey string,
	destination DestinationPath,
	destinationLocatorKey string,
) (*directoryEntry, *directoryAdmissionOperation, bool, TraceEvent, error) {
	entry := session.directoryClaims[reference.id]
	if reference.kind != ClaimDirectory || entry == nil || entry.claim.source != source ||
		entry.claim.artifact != artifact || entry.claim.artifactLocatorKey != artifactLocatorKey ||
		entry.claim.destination != destination ||
		entry.claim.destinationLocatorKey != destinationLocatorKey ||
		!sameAdmission(entry.admission, admission) {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.rejectDirectoryBindingLocked(ErrDirectoryBinding)
	}
	if owner, claimed := session.locatorClaims[artifactLocatorKey]; !claimed || owner != reference {
		return nil, nil, false, session.bindingTraceLocked(operationID, reference.id, OperationAdmitDirectory),
			session.markInvariantFailureLocked()
	}
	if owner, claimed := session.destinationClaims[destinationLocatorKey]; !claimed || owner != reference {
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

func (session *Session) sourceParentLocked(
	source transfer.AuthenticatedSourceDirectory,
) (ClaimID, error) {
	sourcePath := source.SourcePath.String()
	root := session.scope.RootExpectation()
	if source.ParentAdmission.IsZero() &&
		(sourcePath == "" || root.Kind() != transfer.DirectoryAdmissionNoRoot && root.DirectoryID() == source.DirectoryID) {
		if session.rootClaim != 0 && session.directoryClaims[session.rootClaim] == nil {
			return 0, ErrExecutorContract
		}
		return 0, nil
	}
	claimID, ok := session.receiptClaims[receiptKey(source.ParentAdmission)]
	if !ok {
		return 0, ErrDirectoryBinding
	}
	parent := session.directoryClaims[claimID]
	if parent == nil || !sameAdmission(parent.admission, source.ParentAdmission) ||
		parent.claim.source.SourcePath.String() != parentPath(sourcePath) {
		return 0, ErrDirectoryBinding
	}
	return claimID, nil
}

func (session *Session) sourceClaimLocked(node catalog.NodeID, path string) (claimRef, bool) {
	byNode, nodeOK := session.nodeClaims[node]
	byPath, pathOK := session.pathClaims[path]
	if nodeOK && pathOK && byNode != byPath {
		return byNode, true
	}
	if nodeOK {
		return byNode, true
	}
	return byPath, pathOK
}

func (session *Session) existingClaimLocked(
	node catalog.NodeID,
	sourcePath string,
	artifactPath string,
	locatorKey string,
	name parentNameKey,
) (claimRef, bool, bool) {
	candidates := make([]claimRef, 0, 5)
	if value, ok := session.nodeClaims[node]; ok {
		candidates = append(candidates, value)
	}
	if value, ok := session.pathClaims[sourcePath]; ok {
		candidates = append(candidates, value)
	}
	if value, ok := session.artifactClaims[artifactPath]; ok {
		candidates = append(candidates, value)
	}
	if value, ok := session.locatorClaims[locatorKey]; ok {
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

func (session *Session) removeDirectoryReservationLocked(entry *directoryEntry) {
	reference := claimRef{kind: ClaimDirectory, id: entry.claim.id}
	delete(session.directoryClaims, entry.claim.id)
	if session.nodeClaims[catalog.NodeID(entry.claim.source.DirectoryID)] == reference {
		delete(session.nodeClaims, catalog.NodeID(entry.claim.source.DirectoryID))
	}
	sourcePath := entry.claim.source.SourcePath.String()
	if session.pathClaims[sourcePath] == reference {
		delete(session.pathClaims, sourcePath)
	}
	artifactPath := entry.claim.artifact.String()
	if artifactPath != "" && session.artifactClaims[artifactPath] == reference {
		delete(session.artifactClaims, artifactPath)
	}
	nameKey := parentNameKey{
		parent: entry.claim.destinationParent, name: claimName(entry.claim.destination.String()),
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
