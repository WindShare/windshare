package protocolsession

import (
	"crypto/sha256"
	"errors"
	"sync"

	"github.com/windshare/windshare/core/session/peerslot"
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
		if err != nil && tracked {
			authority.recordSenderOperationViolationLocked(
				direction, AuthenticatedOperationViolationContinuationAuthority,
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
		if err != nil && tracked {
			authority.recordSenderOperationViolationLocked(
				direction, AuthenticatedOperationViolationContinuationAuthority,
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
		if err != nil && tracked {
			authority.recordSenderOperationViolationLocked(
				direction, AuthenticatedOperationViolationContinuationAuthority,
			)
		}
		return tracked, err
	}
	if scope == (OperationContinuationScope{}) {
		return true, ErrContinuationAuthority
	}
	if authority.hasDeferredContinuationScope && authority.deferredContinuationScope != scope {
		authority.recordSenderOperationViolationLocked(
			direction, AuthenticatedOperationViolationContinuationAuthority,
		)
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

// PeerAttemptBinding identifies one negotiation. PeerSlot is session-scoped;
// its sequence survives replacement of the physical PeerPathID.
type PeerAttemptBinding struct {
	PeerSlot        uint8
	PeerPathID      [16]byte
	AttemptID       [16]byte
	AttemptSequence uint64
}

func (binding PeerAttemptBinding) valid() bool {
	return binding.PeerSlot < peerslot.Count && binding.PeerPathID != [16]byte{} && binding.AttemptID != [16]byte{} && binding.AttemptSequence != 0
}

func decodePeerAttemptBinding(value any) (PeerAttemptBinding, error) {
	fields, ok := value.([]any)
	if !ok || len(fields) != 4 {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	path, pathOK := fields[0].([]byte)
	attempt, attemptOK := fields[1].([]byte)
	sequence, sequenceOK := fields[2].(uint64)
	slot, slotOK := fields[3].(uint64)
	if !pathOK || len(path) != 16 || !attemptOK || len(attempt) != 16 || !sequenceOK || !slotOK || slot >= peerslot.Count {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	binding := PeerAttemptBinding{PeerSlot: uint8(slot), PeerPathID: [16]byte(path), AttemptID: [16]byte(attempt), AttemptSequence: sequence}
	if !binding.valid() {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	return binding, nil
}

// PeerAttemptContinuationClassifier supplies schema-validated identities only.
// Incoming messages may consult an existing retirement boundary, never create it.
type PeerAttemptContinuationClassifier interface {
	ClassifyPeerAttemptContinuation(MessageKind, []byte) (PeerAttemptBinding, bool, error)
}

type peerAttemptAuthority interface {
	PeerAttemptBinding() PeerAttemptBinding
}

type retiredPeerSlot struct {
	issuedThrough  uint64
	retiredThrough uint64
	active         map[uint64]struct{}
}

func operationPeerAttempt(authority *operationAuthority) (PeerAttemptBinding, bool) {
	if authority == nil || authority.continuations == nil {
		return PeerAttemptBinding{}, false
	}
	owner, ok := authority.continuations.authority.(peerAttemptAuthority)
	if !ok {
		return PeerAttemptBinding{}, false
	}
	return owner.PeerAttemptBinding(), true
}

// PeerAttemptBinding returns a copy owned by this opaque generation. A delayed
// producer cannot borrow the binding of a newer operation with the same ID.
func (generation OperationGeneration) PeerAttemptBinding() (PeerAttemptBinding, bool) {
	if generation.table == nil {
		return PeerAttemptBinding{}, false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	return operationPeerAttempt(generation.authority)
}

func (table *OperationTable) admitPeerAttemptLocked(authority *operationAuthority) error {
	binding, ok := operationPeerAttempt(authority)
	if !ok {
		return nil
	}
	if !binding.valid() {
		return ErrContinuationAuthority
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		slot = &retiredPeerSlot{active: make(map[uint64]struct{})}
		table.peerSlots[binding.PeerSlot] = slot
	}
	if binding.AttemptSequence <= slot.issuedThrough {
		return ErrOperationIDReused
	}
	slot.issuedThrough = binding.AttemptSequence
	slot.active[binding.AttemptSequence] = struct{}{}
	return nil
}

func (table *OperationTable) retirePeerAttemptLocked(authority *operationAuthority) {
	binding, ok := operationPeerAttempt(authority)
	if !ok {
		return
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		return
	}
	delete(slot.active, binding.AttemptSequence)
	if binding.AttemptSequence > slot.retiredThrough {
		slot.retiredThrough = binding.AttemptSequence
	}
}

func (table *OperationTable) peerContinuationBinding(direction Direction, message Message) (PeerAttemptBinding, bool, error) {
	if message.kind != MessagePeerOffer && message.kind != MessagePeerAnswer && message.kind != MessagePeerCandidate && message.kind != MessageOperationError {
		return PeerAttemptBinding{}, false, nil
	}
	body, err := operationContinuationSemanticBody(direction, message)
	if err != nil {
		return PeerAttemptBinding{}, true, err
	}
	if message.kind == MessageOperationError {
		failure, err := DecodeOperationFailure(body)
		if err != nil {
			return PeerAttemptBinding{}, true, err
		}
		if failure.PeerAttempt == nil {
			return PeerAttemptBinding{}, false, nil
		}
		return *failure.PeerAttempt, true, nil
	}
	classifier, ok := table.continuations.(PeerAttemptContinuationClassifier)
	if !ok {
		return PeerAttemptBinding{}, false, nil
	}
	return classifier.ClassifyPeerAttemptContinuation(message.kind, body)
}

func (table *OperationTable) validatePeerAttemptLocked(authority *operationAuthority, direction Direction, message Message) error {
	expected, ok := operationPeerAttempt(authority)
	if !ok || message.kind == MessagePeerOffer {
		return nil
	}
	actual, tracked, err := table.peerContinuationBinding(direction, message)
	if err == nil && tracked && actual != expected {
		err = ErrConflictingContinuation
	}
	if err != nil {
		authority.recordSenderOperationViolationLocked(direction, AuthenticatedOperationViolationContinuationAuthority)
	}
	return err
}

func (table *OperationTable) dropsRetiredPeerLocked(direction Direction, message Message) bool {
	binding, tracked, err := table.peerContinuationBinding(direction, message)
	if err != nil || !tracked {
		return false
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		return false
	}
	_, active := slot.active[binding.AttemptSequence]
	// A request overtaken on another lane cannot resurrect an older negotiation.
	// A current sequence still requires its exact operation authority.
	if message.kind == MessagePeerOffer && binding.AttemptSequence < slot.issuedThrough {
		return !active
	}
	return binding.AttemptSequence <= slot.retiredThrough && !active
}
