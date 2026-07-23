package protocolsession

import "sync"

type operationAuthority struct {
	pins                            uint32
	continuations                   *operationContinuationState
	deferredContinuationScope       OperationContinuationScope
	hasDeferredContinuationScope    bool
	authenticatedViolation          AuthenticatedOperationViolation
	authenticatedViolationHandler   func(AuthenticatedOperationViolation)
	authenticatedViolationDelivered bool
}

// OperationGeneration is a comparable opaque identity suitable for async
// ownership maps. It contains hostile same-ID collisions after bounded retention
// without making OperationID reuse valid protocol behavior.
type OperationGeneration struct {
	table       *OperationTable
	authority   *operationAuthority
	operationID OperationID
}

func (generation OperationGeneration) IsZero() bool { return generation.authority == nil }

func (generation OperationGeneration) Same(other OperationGeneration) bool {
	return !generation.IsZero() && generation == other
}

func (generation OperationGeneration) IsCurrent() bool {
	if generation.table == nil || generation.authority == nil || generation.operationID.IsZero() {
		return false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	generation.table.pruneExpired()
	return generation.table.operationAuthority(generation.operationID) == generation.authority
}

// IsActive distinguishes executable request authority from a tombstone that is
// still current only to suppress delayed continuations and cancellation races.
func (generation OperationGeneration) IsActive() bool {
	if generation.IsZero() || generation.table == nil {
		return false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	generation.table.pruneExpired()
	active, ok := generation.table.active[generation.operationID]
	return ok && active.authority == generation.authority
}

func (generation OperationGeneration) MaximumContinuations() (int, bool) {
	if generation.IsZero() || generation.table == nil {
		return 0, false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	generation.table.pruneExpired()
	if generation.table.operationAuthority(generation.operationID) != generation.authority ||
		generation.authority.continuations == nil {
		return 0, false
	}
	return generation.authority.continuations.maximum, true
}

// RequestKind reports the initial request semantic only while this exact
// generation remains active or tombstoned. It lets cancellation dispatch choose
// one owner without exposing mutable lifecycle state or routing by OperationID.
func (generation OperationGeneration) RequestKind() (MessageKind, bool) {
	if generation.IsZero() || generation.table == nil {
		return 0, false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	generation.table.pruneExpired()
	if active, ok := generation.table.active[generation.operationID]; ok &&
		active.authority == generation.authority {
		return active.requestKind, true
	}
	if tombstone, ok := generation.table.tombstones[generation.operationID]; ok &&
		tombstone.authority == generation.authority && tombstone.requestKind != 0 {
		return tombstone.requestKind, true
	}
	return 0, false
}

// CancelGeneration records local abandonment only for the exact admitted
// generation. A delayed owner can therefore never retire a newer same-ID call.
func (table *OperationTable) CancelGeneration(generation OperationGeneration) error {
	if table == nil || generation.table != table || generation.authority == nil || generation.operationID.IsZero() {
		return ErrInvalidOperationID
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return ErrSessionTerminated
	}
	table.pruneExpired()
	operationID := generation.operationID
	if tombstone, exists := table.tombstones[operationID]; exists {
		if tombstone.authority != generation.authority {
			return nil
		}
		// A peer final can win the race before local Unknown reconciliation. Its
		// semantic fingerprint must remain authoritative so an exact late final is
		// idempotent and a conflicting one still terminates the session.
		return nil
	}
	active, exists := table.active[operationID]
	if !exists || active.authority != generation.authority {
		return nil
	}
	if len(table.tombstones) >= table.limits.MaxTombstones {
		return ErrTombstoneBudget
	}
	delete(table.active, operationID)
	clearContinuationReplayLocked(generation.authority)
	table.tombstones[operationID] = operationTombstone{
		expiresAt:          table.now().Add(OperationTombstoneLifetime),
		requestKind:        active.requestKind,
		requestFingerprint: active.requestFingerprint,
		finalKind:          MessageCancel,
		cancelled:          true,
		authority:          generation.authority,
	}
	return nil
}

// OutboundOperationPermit binds the sender's first response attempt to the exact
// inbound operation generation that authorized its handler.
type OutboundOperationPermit struct {
	table       *OperationTable
	authority   *operationAuthority
	direction   Direction
	operationID OperationID
}

func (permit OutboundOperationPermit) IsZero() bool { return permit.authority == nil }

func (permit OutboundOperationPermit) Generation() OperationGeneration {
	if permit.IsZero() {
		return OperationGeneration{}
	}
	return OperationGeneration{
		table: permit.table, authority: permit.authority, operationID: permit.operationID,
	}
}

type OutboundOperationLease struct{ pin *outboundAdmissionPin }

func (permit OutboundOperationPermit) AcquireLease() (*OutboundOperationLease, error) {
	if permit.table == nil || permit.authority == nil || permit.operationID.IsZero() {
		return nil, ErrOperationIDReused
	}
	permit.table.mu.Lock()
	defer permit.table.mu.Unlock()
	permit.table.pruneExpired()
	if permit.table.operationAuthority(permit.operationID) != permit.authority {
		return nil, ErrUnknownOperation
	}
	if permit.authority.pins >= MaximumOperationPins {
		return nil, ErrOperationPinBudget
	}
	return &OutboundOperationLease{pin: permit.table.pinLocked(
		permit.operationID, permit.authority, true,
	)}, nil
}

func (lease *OutboundOperationLease) Release() {
	if lease == nil {
		return
	}
	lease.pin.release()
}

// OutboundReplayPermit is an opaque, operation-generation-scoped capability
// minted only after normal outbound admission.
type OutboundReplayPermit struct {
	table       *OperationTable
	authority   *operationAuthority
	direction   Direction
	kind        MessageKind
	operationID OperationID
	fingerprint [32]byte
}

func (permit OutboundReplayPermit) IsZero() bool { return permit.authority == nil }

type OutboundAdmission struct {
	Disposition  OperationDisposition
	Generation   OperationGeneration
	Operation    OutboundOperationPermit
	Replay       OutboundReplayPermit
	pin          *outboundAdmissionPin
	continuation *operationContinuationReservation
}

type InboundAdmission struct {
	Disposition  OperationDisposition
	Generation   OperationGeneration
	Outbound     OutboundOperationPermit
	continuation *operationContinuationReservation
}

type outboundAdmissionPin struct {
	once        sync.Once
	table       *OperationTable
	authority   *operationAuthority
	operationID OperationID
	refresh     bool
}

func (pin *outboundAdmissionPin) release() {
	if pin == nil || pin.table == nil || pin.authority == nil {
		return
	}
	pin.once.Do(func() {
		pin.table.mu.Lock()
		if pin.refresh {
			if tombstone, ok := pin.table.tombstones[pin.operationID]; ok &&
				tombstone.authority == pin.authority {
				minimumExpiry := pin.table.now().Add(OperationTombstoneLifetime)
				if tombstone.expiresAt.Before(minimumExpiry) {
					tombstone.expiresAt = minimumExpiry
					pin.table.tombstones[pin.operationID] = tombstone
				}
			}
		}
		if pin.authority.pins > 0 {
			pin.authority.pins--
		}
		pin.table.mu.Unlock()
	})
}
