package protocolsession

import "errors"

func (table *OperationTable) addTombstone(
	operationID OperationID,
	direction Direction,
	message Message,
	cancelled bool,
	requestKind MessageKind,
	requestFingerprint [32]byte,
	authority *operationAuthority,
) error {
	if _, exists := table.tombstones[operationID]; !exists && len(table.tombstones) >= table.limits.MaxTombstones {
		return ErrTombstoneBudget
	}
	if authority == nil {
		authority = &operationAuthority{}
	}
	table.tombstones[operationID] = operationTombstone{
		expiresAt:          table.now().Add(OperationTombstoneLifetime),
		requestKind:        requestKind,
		requestFingerprint: requestFingerprint,
		finalKind:          message.kind,
		fingerprint:        message.operationFingerprint(direction),
		cancelled:          cancelled,
		authority:          authority,
	}
	return nil
}

func (table *OperationTable) pruneExpired() {
	now := table.now()
	for operationID, tombstone := range table.tombstones {
		if !now.Before(tombstone.expiresAt) && tombstone.authority.pins == 0 {
			delete(table.tombstones, operationID)
		}
	}
}

func (table *OperationTable) observeTombstone(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	// CANCEL is a race signal rather than conflicting result content. If a final
	// was already admitted, that final wins; tearing down the ProtocolSession for
	// a normal cross-lane cancellation race would make multi-lane use unstable.
	if message.kind == MessageCancel {
		return OperationDrop, nil
	}
	if tombstone.cancelled {
		return table.observeCancelledTombstone(operationID, tombstone, direction, message)
	}
	if direction == DirectionReceiverToSender && message.kind.isRequest() {
		if tombstone.requestKind == message.kind &&
			tombstone.requestFingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		return OperationDrop, ErrOperationIDReused
	}
	if message.kind.isFinal() || message.kind == MessageLaneAttach {
		if tombstone.finalKind == message.kind && tombstone.fingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		if direction == DirectionSenderToReceiver {
			tombstone.authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{code: AuthenticatedOperationViolationConflictingFinal},
			)
		}
		return OperationDrop, ErrConflictingFinal
	}
	if messageAllowedForOperation(tombstone.requestKind, message.kind) {
		return OperationDrop, nil
	}
	return OperationDrop, ErrOperationIDReused
}

func (table *OperationTable) observeCancelledTombstone(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	isRequest := direction == DirectionReceiverToSender &&
		(message.kind.isRequest() || message.kind == MessageLaneAttach)
	if tombstone.requestKind == 0 {
		return table.bindPreemptivelyCancelledRequestLocked(operationID, tombstone, direction, message, isRequest)
	}
	if isRequest {
		if tombstone.requestKind == message.kind &&
			tombstone.requestFingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		return OperationDrop, ErrOperationIDReused
	}
	if !messageAllowedForOperation(tombstone.requestKind, message.kind) {
		return OperationDrop, ErrOperationIDReused
	}
	if message.kind.isFinal() ||
		(tombstone.requestKind == MessageLaneAttach && message.kind == MessageLaneAttach) {
		fingerprint := message.operationFingerprint(direction)
		if tombstone.finalKind == MessageCancel {
			// The first compatible final may have crossed the cancellation on another
			// lane. Remembering it makes only exact repeats idempotent thereafter.
			tombstone.finalKind = message.kind
			tombstone.fingerprint = fingerprint
			table.tombstones[operationID] = tombstone
			return OperationDrop, nil
		}
		if tombstone.finalKind != message.kind || tombstone.fingerprint != fingerprint {
			if direction == DirectionSenderToReceiver {
				tombstone.authority.recordAuthenticatedOperationViolationLocked(
					AuthenticatedOperationViolation{code: AuthenticatedOperationViolationConflictingFinal},
				)
			}
			return OperationDrop, ErrConflictingFinal
		}
	}
	return OperationDrop, nil
}

func (table *OperationTable) bindPreemptivelyCancelledRequestLocked(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
	isRequest bool,
) (OperationDisposition, error) {
	if !isRequest {
		return OperationDrop, ErrOperationIDReused
	}
	authority, err := table.newOperationAuthority(message)
	if err != nil {
		return OperationDrop, err
	}
	if !deferredContinuationMatches(tombstone.authority, authority) {
		if direction == DirectionSenderToReceiver {
			tombstone.authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{
					code: AuthenticatedOperationViolationContinuationAuthority,
				},
			)
		}
		return OperationDrop, ErrConflictingContinuation
	}
	// A preemptive CANCEL learns the request family from the raced request.
	// Retaining it narrows every later drop to that operation's continuations.
	tombstone.requestKind = message.kind
	tombstone.requestFingerprint = message.operationFingerprint(direction)
	tombstone.authority.continuations = authority.continuations
	tombstone.authority.deferredContinuationScope = OperationContinuationScope{}
	tombstone.authority.hasDeferredContinuationScope = false
	table.tombstones[operationID] = tombstone
	return OperationDrop, nil
}

// AcceptOutboundReplay validates an exact permit without repeating the normal
// state transition. Final expiry and PeerAnswer multiplicity remain unchanged.
func (table *OperationTable) AcceptOutboundReplay(
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if table == nil {
		return OutboundAdmission{Disposition: OperationDrop}, errors.New("protocolsession: nil operation table")
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpired()
	if table.terminal {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	operationID, ok := message.OperationID()
	if !ok || permit.table != table || permit.authority == nil || permit.direction != direction ||
		permit.kind != message.kind || permit.operationID != operationID {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationIDReused
	}
	if tombstone, found := table.tombstones[operationID]; found {
		return table.acceptTombstoneReplayLocked(operationID, tombstone, direction, message, permit)
	}
	active, found := table.active[operationID]
	if !found || active.authority != permit.authority {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	return table.acceptActiveReplayLocked(operationID, active, direction, message, permit)
}

func (table *OperationTable) acceptTombstoneReplayLocked(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if tombstone.authority != permit.authority {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if err := validateReplayFingerprint(message, direction, permit); err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if tombstone.cancelled && (direction != DirectionReceiverToSender || message.kind != MessageCancel) {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if tombstone.cancelled {
		return table.replayAdmissionLocked(operationID, tombstone.authority, permit)
	}
	if !message.kind.isFinal() && message.kind != MessageLaneAttach {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if tombstone.finalKind != message.kind || tombstone.fingerprint != permit.fingerprint {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	return table.replayAdmissionLocked(operationID, tombstone.authority, permit)
}

func (table *OperationTable) acceptActiveReplayLocked(
	operationID OperationID,
	active activeOperation,
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if err := validateReplayFingerprint(message, direction, permit); err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if direction == DirectionReceiverToSender && message.kind.isRequest() {
		if active.requestKind != message.kind || active.requestFingerprint != permit.fingerprint {
			return OutboundAdmission{Disposition: OperationDrop}, ErrOperationIDReused
		}
		return table.replayAdmissionLocked(operationID, active.authority, permit)
	}
	if !messageAllowedForOperation(active.requestKind, message.kind) {
		return OutboundAdmission{Disposition: OperationDrop}, ErrUnexpectedOperation
	}
	// Reserve only after every fallible admission precondition. A failed replay
	// must not leave a pending semantic fingerprint that suppresses its retry.
	if active.authority.pins >= MaximumOperationPins {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationPinBudget
	}
	continuation, drop, err := table.reserveReplayContinuationLocked(active.authority, direction, message)
	if err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if drop {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	admission, err := table.replayAdmissionLocked(operationID, active.authority, permit)
	admission.continuation = continuation
	return admission, err
}

func (table *OperationTable) replayAdmissionLocked(
	operationID OperationID,
	authority *operationAuthority,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if authority.pins >= MaximumOperationPins {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationPinBudget
	}
	return OutboundAdmission{
		Disposition: OperationDeliver,
		Generation: OperationGeneration{
			table: table, authority: authority, operationID: operationID,
		},
		Replay: permit,
		pin:    table.pinLocked(operationID, authority, false),
	}, nil
}

func validateReplayFingerprint(
	message Message,
	direction Direction,
	permit OutboundReplayPermit,
) error {
	if permit.fingerprint == message.operationFingerprint(direction) {
		return nil
	}
	if message.kind.isFinal() || message.kind == MessageLaneAttach {
		return ErrConflictingFinal
	}
	return ErrOperationIDReused
}
