package sessioncore

import (
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func (table *operationTable) observeTombstoneLocked(
	id protocolsession.OperationID,
	tombstone operationTombstone,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	if message.Kind() == protocolsession.MessageCancel {
		return protocolsession.OperationDrop, nil
	}
	if tombstone.cancelled {
		return table.observeCancelledTombstoneLocked(id, tombstone, direction, message)
	}
	if direction == protocolsession.DirectionReceiverToSender && message.Kind().IsRequest() {
		if tombstone.record.requestKind == message.Kind() &&
			tombstone.record.requestFingerprint == message.OperationFingerprint(direction) {
			return protocolsession.OperationDrop, nil
		}
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	if message.Kind().IsFinal() || message.Kind() == protocolsession.MessageLaneAttach {
		if tombstone.finalKind == message.Kind() &&
			tombstone.fingerprint == message.OperationFingerprint(direction) {
			return protocolsession.OperationDrop, nil
		}
		tombstone.record.recordSenderViolationLocked(direction, protocolsession.AuthenticatedOperationViolationConflictingFinal)
		return protocolsession.OperationDrop, protocolsession.ErrConflictingFinal
	}
	if protocolsession.MessageAllowedForOperation(tombstone.record.requestKind, message.Kind()) {
		return protocolsession.OperationDrop, nil
	}
	return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
}

func (table *operationTable) observeCancelledTombstoneLocked(
	id protocolsession.OperationID,
	tombstone operationTombstone,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	isRequest := direction == protocolsession.DirectionReceiverToSender &&
		(message.Kind().IsRequest() || message.Kind() == protocolsession.MessageLaneAttach)
	if tombstone.record.requestKind == 0 {
		return table.bindPreemptivelyCancelledRequestLocked(id, tombstone, direction, message, isRequest)
	}
	if isRequest {
		if tombstone.record.requestKind == message.Kind() &&
			tombstone.record.requestFingerprint == message.OperationFingerprint(direction) {
			return protocolsession.OperationDrop, nil
		}
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	if !protocolsession.MessageAllowedForOperation(tombstone.record.requestKind, message.Kind()) {
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	if message.Kind().IsFinal() ||
		(tombstone.record.requestKind == protocolsession.MessageLaneAttach && message.Kind() == protocolsession.MessageLaneAttach) {
		fingerprint := message.OperationFingerprint(direction)
		if tombstone.finalKind == protocolsession.MessageCancel {
			tombstone.finalKind = message.Kind()
			tombstone.fingerprint = fingerprint
			table.tombstones[id] = tombstone
			return protocolsession.OperationDrop, nil
		}
		if tombstone.finalKind != message.Kind() || tombstone.fingerprint != fingerprint {
			tombstone.record.recordSenderViolationLocked(direction, protocolsession.AuthenticatedOperationViolationConflictingFinal)
			return protocolsession.OperationDrop, protocolsession.ErrConflictingFinal
		}
	}
	return protocolsession.OperationDrop, nil
}

func (table *operationTable) bindPreemptivelyCancelledRequestLocked(
	id protocolsession.OperationID,
	tombstone operationTombstone,
	direction protocolsession.Direction,
	message protocolsession.Message,
	isRequest bool,
) (protocolsession.OperationDisposition, error) {
	if !isRequest {
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	candidate, err := table.newOperationRecordLocked(id, direction, session.LaneIdentity{}, message, nil)
	if err != nil {
		return protocolsession.OperationDrop, err
	}
	if !deferredContinuationMatches(tombstone.record, candidate) {
		tombstone.record.recordSenderViolationLocked(direction, protocolsession.AuthenticatedOperationViolationContinuationContract)
		return protocolsession.OperationDrop, protocolsession.ErrConflictingContinuation
	}
	if err := table.admitPeerAttemptLocked(candidate); err != nil {
		return protocolsession.OperationDrop, err
	}
	table.retirePeerAttemptLocked(candidate)
	tombstone.record.requestKind = candidate.requestKind
	tombstone.record.requestFingerprint = candidate.requestFingerprint
	tombstone.record.continuations = candidate.continuations
	tombstone.record.deferredContinuationScope = protocolsession.OperationContinuationScope{}
	tombstone.record.hasDeferredContinuationScope = false
	table.tombstones[id] = tombstone
	return protocolsession.OperationDrop, nil
}

// AdmitOutboundRequest allocates a new receiver-initiated operation and pins it
// while its initial request is handed to a lane writer.
func (table *operationTable) AdmitOutboundRequest(
	message protocolsession.Message,
	violationHandler func(protocolsession.AuthenticatedOperationViolation),
) (protocolsession.Operation, int, bool, func(refresh bool), error) {
	if table == nil {
		return protocolsession.Operation{}, 0, false, nil, protocolsession.ErrNilRuntimeDependency
	}
	id, ok := message.OperationID()
	if !ok {
		return protocolsession.Operation{}, 0, false, nil, protocolsession.ErrInvalidOperationID
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	if table.terminal {
		return protocolsession.Operation{}, 0, false, nil, protocolsession.ErrSessionTerminated
	}
	if err := protocolsession.ValidateKindDirection(protocolsession.DirectionReceiverToSender, message.Kind()); err != nil {
		return protocolsession.Operation{}, 0, false, nil, err
	}
	if !message.Kind().IsRequest() && message.Kind() != protocolsession.MessageLaneAttach {
		return protocolsession.Operation{}, 0, false, nil, protocolsession.ErrUnexpectedOperation
	}
	if table.currentRecordLocked(id) != nil {
		return protocolsession.Operation{}, 0, false, nil, protocolsession.ErrOperationIDReused
	}
	_, rec, err := table.beginOperationLocked(id, protocolsession.DirectionReceiverToSender, session.LaneIdentity{}, message, violationHandler)
	if err != nil {
		return protocolsession.Operation{}, 0, false, nil, err
	}
	rec.inFlight++
	maxContinuations := 0
	hasLimit := false
	if rec.continuations != nil {
		maxContinuations = rec.continuations.maximum
		hasLimit = true
	}
	release := table.makeInFlightReleaser(rec)
	return table.operation(rec.ref), maxContinuations, hasLimit, release, nil
}

// RollbackUnsentRequest removes a receiver request proved never to have been
// handed to transport, returning its active slot without retaining a tombstone.
func (table *operationTable) RollbackUnsentRequest(ref protocolsession.OperationRef) {
	if table == nil || ref.IsZero() {
		return
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if active := table.active[ref.ID]; active != nil && active.ref == ref {
		table.rollbackPeerAttemptLocked(active)
		delete(table.active, ref.ID)
		table.notifyCapacityLocked()
	}
}

// AdmitOutboundContinuation validates and records one receiver-side continuation
// (such as PeerCandidate) for an active operation.
func (table *operationTable) AdmitOutboundContinuation(
	ref protocolsession.OperationRef,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, func(), func(refresh bool), error) {
	if table == nil || ref.IsZero() {
		return protocolsession.OperationDrop, nil, nil, session.ErrOperationMissing
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	if table.terminal {
		return protocolsession.OperationDrop, nil, nil, protocolsession.ErrSessionTerminated
	}
	active := table.active[ref.ID]
	if active == nil || active.ref != ref {
		return protocolsession.OperationDrop, nil, nil, nil
	}
	if err := protocolsession.ValidateKindDirection(protocolsession.DirectionReceiverToSender, message.Kind()); err != nil {
		return protocolsession.OperationDrop, nil, nil, err
	}
	if !protocolsession.MessageAllowedForOperation(active.requestKind, message.Kind()) {
		return protocolsession.OperationDrop, nil, nil, protocolsession.ErrUnexpectedOperation
	}
	if err := table.validatePeerAttemptLocked(active, protocolsession.DirectionReceiverToSender, message); err != nil {
		return protocolsession.OperationDrop, nil, nil, err
	}
	drop, rawUndo, err := table.admitActiveContinuationLocked(active, protocolsession.DirectionReceiverToSender, message)
	if err != nil || drop {
		return protocolsession.OperationDrop, nil, nil, err
	}
	active.inFlight++
	var undo func()
	if rawUndo != nil {
		var undoOnce sync.Once
		undo = func() {
			undoOnce.Do(func() {
				table.mu.Lock()
				rawUndo()
				table.mu.Unlock()
			})
		}
	}
	return protocolsession.OperationDeliver, undo, table.makeInFlightReleaser(active), nil
}

type outboundResponseLease struct {
	record           *operationRecord
	requestKind      protocolsession.MessageKind
	preferredLane    session.LaneIdentity
	undoAnswer       bool
	undoContinuation func()
	releaseInFlight  func(refresh bool)
}

func (table *operationTable) lookupRecord(ref protocolsession.OperationRef) *operationRecord {
	if table == nil || ref.IsZero() {
		return nil
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	rec := table.currentRecordLocked(ref.ID)
	if rec == nil || rec.ref != ref {
		return nil
	}
	return rec
}

// BeginOutboundResponse serializes one sender response against its operation
// record, checks active/continuation authority, and pins retention while in flight.
func (table *operationTable) BeginOutboundResponse(
	ref protocolsession.OperationRef,
	message protocolsession.Message,
) (*outboundResponseLease, error) {
	rec := table.lookupRecord(ref)
	if rec == nil {
		return nil, protocolsession.ErrOperationSuppressed
	}
	rec.sendMu.Lock()
	table.mu.Lock()
	table.pruneExpiredLocked()
	if table.terminal {
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrSessionTerminated)
	}
	active := table.active[ref.ID]
	if active != rec {
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.ErrOperationSuppressed
	}
	if err := protocolsession.ValidateKindDirection(protocolsession.DirectionSenderToReceiver, message.Kind()); err != nil {
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.MarkNotSent(err)
	}
	if !protocolsession.MessageAllowedForOperation(rec.requestKind, message.Kind()) {
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.MarkNotSent(protocolsession.ErrUnexpectedOperation)
	}
	if err := table.validatePeerAttemptLocked(rec, protocolsession.DirectionSenderToReceiver, message); err != nil {
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.MarkNotSent(err)
	}
	undoAnswer := false
	if rec.requestKind == protocolsession.MessagePeerOffer && message.Kind() == protocolsession.MessagePeerAnswer {
		fingerprint := message.OperationFingerprint(protocolsession.DirectionSenderToReceiver)
		if rec.answerSeen {
			table.mu.Unlock()
			rec.sendMu.Unlock()
			if rec.answerFingerprint == fingerprint {
				return nil, protocolsession.ErrOperationSuppressed
			}
			return nil, protocolsession.MarkNotSent(protocolsession.ErrOperationIDReused)
		}
		rec.answerSeen = true
		rec.answerFingerprint = fingerprint
		undoAnswer = true
	}
	drop, undoContinuation, err := table.admitActiveContinuationLocked(rec, protocolsession.DirectionSenderToReceiver, message)
	if err != nil {
		if undoAnswer {
			rec.answerSeen = false
			rec.answerFingerprint = [32]byte{}
		}
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.MarkNotSent(err)
	}
	if drop {
		if undoAnswer {
			rec.answerSeen = false
			rec.answerFingerprint = [32]byte{}
		}
		table.mu.Unlock()
		rec.sendMu.Unlock()
		return nil, protocolsession.ErrOperationSuppressed
	}
	rec.inFlight++
	preferred := rec.preferredLane
	requestKind := rec.requestKind
	table.mu.Unlock()
	return &outboundResponseLease{
		record:           rec,
		requestKind:      requestKind,
		preferredLane:    preferred,
		undoAnswer:       undoAnswer,
		undoContinuation: undoContinuation,
		releaseInFlight:  table.makeInFlightReleaser(rec),
	}, nil
}

func (table *operationTable) updatePreferredLane(rec *operationRecord, lane session.LaneIdentity) {
	if table == nil || rec == nil || !lane.Valid(true) {
		return
	}
	table.mu.Lock()
	rec.preferredLane = lane
	table.mu.Unlock()
}

// CompleteOutboundResponse commits final retirement or rolls back unsent
// answer/continuation state and releases the per-operation send lock.
func (table *operationTable) CompleteOutboundResponse(
	lease *outboundResponseLease,
	message protocolsession.Message,
	final bool,
	deliveredOrUncertain bool,
	abandon bool,
) {
	if lease == nil || lease.record == nil {
		return
	}
	rec := lease.record
	table.mu.Lock()
	defer func() {
		table.mu.Unlock()
		rec.sendMu.Unlock()
	}()
	if !deliveredOrUncertain {
		table.rollbackOutboundResponseLocked(lease, abandon)
		return
	}
	if !final || table.terminal {
		return
	}
	if active := table.active[rec.ref.ID]; active == rec {
		_ = table.finishOperationLocked(rec.ref.ID, active, protocolsession.DirectionSenderToReceiver, message, false)
		return
	}
	tombstone, exists := table.tombstones[rec.ref.ID]
	if exists && tombstone.record == rec && tombstone.cancelled && tombstone.finalKind == protocolsession.MessageCancel {
		tombstone.finalKind = message.Kind()
		tombstone.fingerprint = message.OperationFingerprint(protocolsession.DirectionSenderToReceiver)
		table.tombstones[rec.ref.ID] = tombstone
	}
}

func (table *operationTable) rollbackOutboundResponseLocked(lease *outboundResponseLease, abandon bool) {
	rec := lease.record
	if lease.undoAnswer {
		rec.answerSeen = false
		rec.answerFingerprint = [32]byte{}
	}
	if lease.undoContinuation != nil {
		lease.undoContinuation()
	}
	if abandon && !table.terminal {
		table.cancelRecordLocked(rec)
	}
}

func (table *operationTable) pinInFlight(rec *operationRecord) func(refresh bool) {
	if table == nil || rec == nil {
		return nil
	}
	table.mu.Lock()
	rec.inFlight++
	table.mu.Unlock()
	return table.makeInFlightReleaser(rec)
}

func (table *operationTable) makeInFlightReleaser(rec *operationRecord) func(refresh bool) {
	var once sync.Once
	return func(refresh bool) {
		once.Do(func() {
			table.mu.Lock()
			if refresh {
				if tombstone, ok := table.tombstones[rec.ref.ID]; ok && tombstone.record == rec {
					minimumExpiry := table.now().Add(protocolsession.OperationTombstoneLifetime)
					if tombstone.expiresAt.Before(minimumExpiry) {
						tombstone.expiresAt = minimumExpiry
						table.tombstones[rec.ref.ID] = tombstone
					}
				}
			}
			if rec.inFlight > 0 {
				rec.inFlight--
			}
			if rec.inFlight == 0 {
				table.notifyCapacityLocked()
			}
			table.mu.Unlock()
		})
	}
}

// CancelOperation idempotently retires the exact admitted operation generation
// and reports whether it transitioned an active operation to cancelled.
func (table *operationTable) CancelOperation(ref protocolsession.OperationRef) (bool, error) {
	if table == nil || ref.IsZero() {
		return false, protocolsession.ErrInvalidOperationID
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return false, nil
	}
	table.pruneExpiredLocked()
	active := table.active[ref.ID]
	if active == nil || active.ref != ref {
		return false, nil
	}
	table.cancelRecordLocked(active)
	return true, nil
}

func (table *operationTable) cancelRecordLocked(active *operationRecord) {
	if active == nil {
		return
	}
	if table.active[active.ref.ID] != active {
		return
	}
	table.retirePeerAttemptLocked(active)
	delete(table.active, active.ref.ID)
	clearContinuationReplayLocked(active)
	table.tombstones[active.ref.ID] = operationTombstone{
		expiresAt: table.now().Add(protocolsession.OperationTombstoneLifetime),
		finalKind: protocolsession.MessageCancel,
		cancelled: true,
		record:    active,
	}
	table.notifyCapacityLocked()
}

func (table *operationTable) RecordAuthenticatedOperationViolation(
	message protocolsession.Message,
	violation protocolsession.AuthenticatedOperationViolation,
) (bool, error) {
	if table == nil || !violation.Valid() || !violation.MatchesAuthenticatedMessage(message) {
		return false, protocolsession.ErrInvalidAuthenticatedOperationViolation
	}
	id, ok := message.OperationID()
	if !ok {
		return false, protocolsession.ErrInvalidAuthenticatedOperationViolation
	}
	table.mu.Lock()
	table.pruneExpiredLocked()
	rec := table.currentRecordLocked(id)
	if rec == nil {
		table.mu.Unlock()
		return false, nil
	}
	if violation.Code() == protocolsession.AuthenticatedOperationViolationMalformedPeerControl &&
		rec.requestKind != protocolsession.MessagePeerOffer {
		table.mu.Unlock()
		return false, nil
	}
	rec.recordViolationLocked(violation.Code())
	notification := rec.takeViolationNotificationLocked()
	table.mu.Unlock()
	notification.deliver()
	return true, nil
}
