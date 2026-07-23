package protocolsession

import (
	"crypto/sha256"
	"errors"
	"sync"
)

const MaximumOperationContinuations = 1_024

var (
	ErrContinuationAuthority   = errors.New("protocolsession: operation continuation authority is invalid")
	ErrConflictingContinuation = errors.New("protocolsession: operation continuation conflicts with its authority")
	ErrContinuationPending     = errors.New("protocolsession: operation continuation admission is still pending")
)

// OperationContinuationClassifier creates the immutable semantic authority for
// one request generation. The protocol core owns replay state; an injected
// schema owner only decodes the opaque request and continuation bodies.
type OperationContinuationClassifier interface {
	BeginOperationContinuation(
		requestKind MessageKind,
		canonicalRequestBody []byte,
	) (OperationContinuationAuthority, bool, error)
	ClassifyUnboundOperationContinuation(
		kind MessageKind,
		canonicalBody []byte,
	) (OperationContinuationScope, bool, error)
}

type OperationContinuationScope [sha256.Size]byte

// OperationContinuationAuthority validates a continuation against the request
// that created it and returns its canonical semantic fingerprint. Implementations
// must be immutable: one authority is shared by the active operation and its
// tombstone and can be called while either lifecycle state is current.
type OperationContinuationAuthority interface {
	ClassifyOperationContinuation(
		kind MessageKind,
		canonicalBody []byte,
	) ([sha256.Size]byte, bool, error)
	OperationContinuationScope() OperationContinuationScope
	MaximumContinuations() int
}

type operationContinuationKey struct {
	kind        MessageKind
	fingerprint [sha256.Size]byte
}

type operationContinuationRecord struct {
	committed bool
}

type operationContinuationDirection struct {
	records     map[operationContinuationKey]*operationContinuationRecord
	overflowKey operationContinuationKey
	overflow    *operationContinuationRecord
	pendingKey  operationContinuationKey
	pending     *operationContinuationRecord
}

type operationContinuationState struct {
	authority  OperationContinuationAuthority
	scope      OperationContinuationScope
	maximum    int
	directions map[Direction]*operationContinuationDirection
}

func newOperationContinuationState(
	authority OperationContinuationAuthority,
) (*operationContinuationState, error) {
	if authority == nil {
		return nil, ErrContinuationAuthority
	}
	maximum := authority.MaximumContinuations()
	scope := authority.OperationContinuationScope()
	if maximum <= 0 || maximum > MaximumOperationContinuations || scope == (OperationContinuationScope{}) {
		return nil, ErrContinuationAuthority
	}
	return &operationContinuationState{
		authority: authority, scope: scope, maximum: maximum,
		directions: make(map[Direction]*operationContinuationDirection, 2),
	}, nil
}

func (state *operationContinuationState) direction(
	direction Direction,
) *operationContinuationDirection {
	current := state.directions[direction]
	if current == nil {
		current = &operationContinuationDirection{
			records: make(map[operationContinuationKey]*operationContinuationRecord, state.maximum),
		}
		state.directions[direction] = current
	}
	return current
}

func (state *operationContinuationState) classify(
	direction Direction,
	message Message,
) (operationContinuationKey, bool, error) {
	if state == nil || state.authority == nil {
		return operationContinuationKey{}, false, ErrContinuationAuthority
	}
	body, err := operationContinuationSemanticBody(direction, message)
	if err != nil {
		return operationContinuationKey{}, false, err
	}
	fingerprint, tracked, err := state.authority.ClassifyOperationContinuation(message.kind, body)
	if err != nil || !tracked {
		return operationContinuationKey{}, tracked, err
	}
	return operationContinuationKey{kind: message.kind, fingerprint: fingerprint}, true, nil
}

func operationContinuationSemanticBody(direction Direction, message Message) ([]byte, error) {
	if direction == DirectionSenderToReceiver {
		if _, err := senderControlDomain(message.kind); err == nil {
			return SenderControlSemanticBody(message)
		}
	}
	return message.Body(), nil
}

type operationContinuationReservation struct {
	once      sync.Once
	table     *OperationTable
	authority *operationAuthority
	direction Direction
	key       operationContinuationKey
	record    *operationContinuationRecord
	overflow  bool
}

func (reservation *operationContinuationReservation) commit() {
	reservation.settle(true)
}

func (reservation *operationContinuationReservation) rollback() {
	reservation.settle(false)
}

func (reservation *operationContinuationReservation) settle(commit bool) {
	if reservation == nil || reservation.table == nil || reservation.authority == nil ||
		reservation.record == nil {
		return
	}
	reservation.once.Do(func() {
		reservation.table.mu.Lock()
		defer reservation.table.mu.Unlock()
		reservation.settleLocked(commit)
	})
}

// settleLocked is the single state transition for both immediately observed
// continuations and asynchronously published reservations. Keeping commit and
// pending cleanup indivisible prevents a committed candidate from leaving the
// direction permanently closed to later candidates.
func (reservation *operationContinuationReservation) settleLocked(commit bool) {
	if reservation == nil || reservation.authority == nil || reservation.record == nil {
		return
	}
	state := reservation.authority.continuations
	if state == nil {
		return
	}
	direction := state.directions[reservation.direction]
	if direction == nil {
		return
	}
	if reservation.overflow {
		if direction.overflow != reservation.record || direction.overflowKey != reservation.key {
			return
		}
		if commit {
			direction.overflow.committed = true
		} else {
			direction.overflow = nil
			direction.overflowKey = operationContinuationKey{}
		}
		reservation.clearPendingLocked(direction)
		return
	}
	if direction.records[reservation.key] != reservation.record {
		return
	}
	if commit {
		direction.records[reservation.key].committed = true
	} else {
		delete(direction.records, reservation.key)
	}
	reservation.clearPendingLocked(direction)
}

func (reservation *operationContinuationReservation) clearPendingLocked(
	direction *operationContinuationDirection,
) {
	if direction != nil && direction.pending == reservation.record && direction.pendingKey == reservation.key {
		direction.pending = nil
		direction.pendingKey = operationContinuationKey{}
	}
}

func (table *OperationTable) reserveActiveContinuationLocked(
	authority *operationAuthority,
	direction Direction,
	message Message,
) (*operationContinuationReservation, bool, error) {
	if authority == nil || authority.continuations == nil {
		return nil, false, nil
	}
	key, tracked, err := authority.continuations.classify(direction, message)
	if err != nil || !tracked {
		if err != nil && tracked && direction == DirectionSenderToReceiver {
			authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{
					code: AuthenticatedOperationViolationContinuationAuthority,
				},
			)
		}
		return nil, false, err
	}
	current := authority.continuations.direction(direction)
	if record := current.records[key]; record != nil {
		if !record.committed {
			return nil, false, ErrContinuationPending
		}
		return nil, true, nil
	}
	if current.pending != nil {
		return nil, false, ErrContinuationPending
	}
	if current.overflow != nil {
		// The first overflow reaches the operation-local quota owner. Once that
		// outcome is inevitable, coalescing later distinct values keeps the core
		// replay authority bounded without duplicating business quota policy.
		if current.overflowKey == key && !current.overflow.committed {
			return nil, false, ErrContinuationPending
		}
		return nil, true, nil
	}
	record := &operationContinuationRecord{}
	reservation := &operationContinuationReservation{
		table: table, authority: authority, direction: direction, key: key, record: record,
	}
	if len(current.records) < authority.continuations.maximum {
		current.records[key] = record
		current.pendingKey = key
		current.pending = record
		return reservation, false, nil
	}
	current.overflowKey = key
	current.overflow = record
	current.pendingKey = key
	current.pending = record
	reservation.overflow = true
	return reservation, false, nil
}

func (table *OperationTable) reserveReplayContinuationLocked(
	authority *operationAuthority,
	direction Direction,
	message Message,
) (*operationContinuationReservation, bool, error) {
	if authority == nil || authority.continuations == nil {
		return nil, false, nil
	}
	key, tracked, err := authority.continuations.classify(direction, message)
	if err != nil || !tracked {
		return nil, false, err
	}
	current := authority.continuations.direction(direction)
	if record := current.records[key]; record != nil {
		// A committed semantic admission may be retried physically with its opaque
		// replay permit. A pending owner must settle before another route can decide.
		if !record.committed {
			return nil, false, ErrContinuationPending
		}
		return nil, false, nil
	}
	if current.pending != nil {
		return nil, false, ErrContinuationPending
	}
	if current.overflow != nil {
		if current.overflowKey == key && current.overflow.committed {
			return nil, false, nil
		}
		if current.overflowKey == key {
			return nil, false, ErrContinuationPending
		}
		return nil, true, nil
	}
	return table.reserveActiveContinuationLocked(authority, direction, message)
}

func (table *OperationTable) acceptLateContinuationLocked(
	authority *operationAuthority,
	direction Direction,
	message Message,
) (bool, error) {
	if authority == nil || authority.continuations == nil {
		return false, nil
	}
	key, tracked, err := authority.continuations.classify(direction, message)
	if err != nil || !tracked {
		if err != nil && tracked && direction == DirectionSenderToReceiver {
			authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{
					code: AuthenticatedOperationViolationContinuationAuthority,
				},
			)
		}
		return tracked, err
	}
	_ = key
	// Once terminal, physical cross-lane order cannot prove whether a distinct
	// candidate was sent before the final. Binding/schema validation remains
	// authoritative, while every compatible late candidate is idempotent.
	return true, nil
}

func (table *OperationTable) acceptUnboundLateContinuationLocked(
	authority *operationAuthority,
	direction Direction,
	message Message,
) (bool, error) {
	if table.continuations == nil || authority == nil || authority.continuations != nil {
		return false, nil
	}
	body, err := operationContinuationSemanticBody(direction, message)
	if err != nil {
		return false, err
	}
	scope, tracked, err := table.continuations.ClassifyUnboundOperationContinuation(message.kind, body)
	if err != nil || !tracked {
		if err != nil && tracked && direction == DirectionSenderToReceiver {
			authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{
					code: AuthenticatedOperationViolationContinuationAuthority,
				},
			)
		}
		return tracked, err
	}
	if scope == (OperationContinuationScope{}) {
		return true, ErrContinuationAuthority
	}
	if authority.hasDeferredContinuationScope && authority.deferredContinuationScope != scope {
		if direction == DirectionSenderToReceiver {
			authority.recordAuthenticatedOperationViolationLocked(
				AuthenticatedOperationViolation{
					code: AuthenticatedOperationViolationContinuationAuthority,
				},
			)
		}
		return true, ErrConflictingContinuation
	}
	authority.deferredContinuationScope = scope
	authority.hasDeferredContinuationScope = true
	return true, nil
}

func clearContinuationReplayLocked(authority *operationAuthority) {
	if authority == nil || authority.continuations == nil {
		return
	}
	clear(authority.continuations.directions)
}
