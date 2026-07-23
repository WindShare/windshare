package protocolsession

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Role determines, rather than merely documents, the only directions a runtime
// may receive and send. This prevents accidentally wiring two receiver halves.
type Role uint8

const (
	RoleReceiver Role = iota
	RoleSender
)

const (
	OperationTombstoneLifetime = 30 * time.Second
	// Each logical lane owns bounded control/data queues plus one physical send.
	// A larger count cannot be produced by a conforming runtime.
	MaximumOperationPins = uint32(DefaultMaxLogicalLanes * (ControlQueueFrameLimit + DataQueueFrameLimit + 1))
)

var (
	ErrInvalidRole           = errors.New("protocolsession: invalid role")
	ErrInvalidDirection      = errors.New("protocolsession: message kind is invalid for direction")
	ErrUnknownOperation      = errors.New("protocolsession: message refers to an unknown operation")
	ErrOperationIDReused     = errors.New("protocolsession: operation identity was reused")
	ErrUnexpectedOperation   = errors.New("protocolsession: message is invalid for the operation")
	ErrConflictingFinal      = errors.New("protocolsession: operation final conflicts with its tombstone")
	ErrSessionTerminated     = errors.New("protocolsession: session is terminal")
	ErrActiveOperationBudget = errors.New("protocolsession: active operation budget exhausted")
	ErrTombstoneBudget       = errors.New("protocolsession: operation tombstone budget exhausted")
	ErrOperationPinBudget    = errors.New("protocolsession: operation in-flight pin budget exhausted")
)

type OperationLimits struct {
	MaxActive     int
	MaxTombstones int
}

// OperationDisposition tells a router whether a valid message is new, an
// idempotent late arrival, or the authenticated end of the entire session.
type OperationDisposition uint8

const (
	OperationDeliver OperationDisposition = iota
	OperationDrop
	OperationSessionTerminal
)

type activeOperation struct {
	requestKind        MessageKind
	requestFingerprint [32]byte
	answerSeen         bool
	answerFingerprint  [32]byte
	authority          *operationAuthority
}

type operationTombstone struct {
	expiresAt          time.Time
	requestKind        MessageKind
	requestFingerprint [32]byte
	finalKind          MessageKind
	fingerprint        [32]byte
	cancelled          bool
	authority          *operationAuthority
}

// OperationTable is the cross-lane source of truth for operation identity.
// Its clock is injectable because tombstone expiry must be deterministic in
// tests and must not be inferred from relay time.
type OperationTable struct {
	mu sync.Mutex

	now           func() time.Time
	limits        OperationLimits
	continuations OperationContinuationClassifier
	active        map[OperationID]activeOperation
	tombstones    map[OperationID]operationTombstone
	terminal      bool
}

func NewOperationTable(limits OperationLimits, now func() time.Time) (*OperationTable, error) {
	return NewOperationTableWithContinuations(limits, now, nil)
}

func NewOperationTableWithContinuations(
	limits OperationLimits,
	now func() time.Time,
	continuations OperationContinuationClassifier,
) (*OperationTable, error) {
	if limits.MaxActive <= 0 || limits.MaxTombstones <= 0 {
		return nil, fmt.Errorf("protocolsession: invalid operation limits: %+v", limits)
	}
	if now == nil {
		now = time.Now
	}
	return &OperationTable{
		now:           now,
		limits:        limits,
		continuations: continuations,
		active:        make(map[OperationID]activeOperation),
		tombstones:    make(map[OperationID]operationTombstone),
	}, nil
}

func (role Role) InboundDirection() (Direction, error) {
	switch role {
	case RoleReceiver:
		return DirectionSenderToReceiver, nil
	case RoleSender:
		return DirectionReceiverToSender, nil
	default:
		return 0, ErrInvalidRole
	}
}

func (role Role) OutboundDirection() (Direction, error) {
	inbound, err := role.InboundDirection()
	if err != nil {
		return 0, err
	}
	if inbound == DirectionReceiverToSender {
		return DirectionSenderToReceiver, nil
	}
	return DirectionReceiverToSender, nil
}

// Observe atomically validates direction, request/response multiplicity, final
// uniqueness, cancellation races, and the 30-second late-arrival window.
func (table *OperationTable) Observe(direction Direction, message Message) (OperationDisposition, error) {
	if table == nil {
		return OperationDrop, errors.New("protocolsession: nil operation table")
	}
	table.mu.Lock()
	disposition, reservation, err := table.observeAdmissionLocked(direction, message)
	if err == nil && disposition == OperationDeliver && reservation != nil {
		reservation.settleLocked(true)
	}
	notification := table.authenticatedOperationViolationNotificationLocked(message)
	table.mu.Unlock()
	notification.deliver()
	return disposition, err
}

// ObserveInbound returns sender response authority in the same critical section
// that creates or advances the inbound operation. This prevents cancellation or
// same-ID reuse from changing the generation between observation and minting.
func (table *OperationTable) ObserveInbound(direction Direction, message Message) (InboundAdmission, error) {
	if table == nil {
		return InboundAdmission{Disposition: OperationDrop}, errors.New("protocolsession: nil operation table")
	}

	table.mu.Lock()
	disposition, reservation, err := table.observeAdmissionLocked(direction, message)
	admission := InboundAdmission{Disposition: disposition}
	if err != nil || disposition != OperationDeliver {
		notification := table.authenticatedOperationViolationNotificationLocked(message)
		table.mu.Unlock()
		notification.deliver()
		return admission, err
	}
	operationID, ok := message.OperationID()
	if !ok {
		table.mu.Unlock()
		return admission, nil
	}
	authority := table.operationAuthority(operationID)
	if authority == nil {
		table.mu.Unlock()
		return admission, ErrUnknownOperation
	}
	admission.Generation = OperationGeneration{
		table: table, authority: authority, operationID: operationID,
	}
	if direction != DirectionReceiverToSender {
		admission.continuation = reservation
		table.mu.Unlock()
		return admission, nil
	}
	admission.Outbound = OutboundOperationPermit{
		table: table, authority: authority, direction: DirectionSenderToReceiver,
		operationID: operationID,
	}
	admission.continuation = reservation
	table.mu.Unlock()
	return admission, nil
}

func (table *OperationTable) observeAdmissionLocked(
	direction Direction,
	message Message,
) (OperationDisposition, *operationContinuationReservation, error) {
	table.pruneExpired()

	// A terminal is a monotonic session state, not another operation final. Once
	// accepted, every lane drops later authenticated traffic instead of allowing
	// it to perturb already-terminated state.
	if table.terminal {
		return OperationDrop, nil, nil
	}
	if err := validateKindDirection(direction, message.kind); err != nil {
		return OperationDrop, nil, err
	}
	if message.kind == MessageSessionTerminal {
		table.terminal = true
		clear(table.active)
		clear(table.tombstones)
		return OperationSessionTerminal, nil, nil
	}

	operationID, ok := message.OperationID()
	if !ok {
		return OperationDrop, nil, ErrInvalidOperationID
	}
	if tombstone, found := table.tombstones[operationID]; found {
		if tracked, err := table.acceptLateContinuationLocked(tombstone.authority, direction, message); tracked {
			return OperationDrop, nil, err
		}
		if tombstone.requestKind == 0 {
			if tracked, err := table.acceptUnboundLateContinuationLocked(
				tombstone.authority, direction, message,
			); tracked {
				return OperationDrop, nil, err
			}
		}
		disposition, err := table.observeTombstone(operationID, tombstone, direction, message)
		return disposition, nil, err
	}

	active, found := table.active[operationID]
	if !found {
		disposition, err := table.beginOperation(operationID, direction, message)
		return disposition, nil, err
	}
	disposition, err := table.advanceOperation(operationID, active, direction, message)
	if err != nil || disposition != OperationDeliver {
		return disposition, nil, err
	}
	reservation, drop, err := table.reserveActiveContinuationLocked(active.authority, direction, message)
	if err != nil {
		return OperationDrop, nil, err
	}
	if drop {
		return OperationDrop, nil, nil
	}
	return disposition, reservation, nil
}

// AdmitOutbound atomically performs the normal operation transition. Initial
// receiver requests mint an opaque replay capability so a settled ambiguous send
// can establish dependency order on a replacement lane; normal duplicate
// admission still has no path to that capability.
func (table *OperationTable) AdmitOutbound(
	direction Direction,
	message Message,
	permit OutboundOperationPermit,
) (OutboundAdmission, error) {
	if table == nil {
		return OutboundAdmission{Disposition: OperationDrop}, errors.New("protocolsession: nil operation table")
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	requiresPermit := direction == DirectionSenderToReceiver ||
		(direction == DirectionReceiverToSender && !message.kind.isRequest() && message.kind != MessageLaneAttach)
	if requiresPermit {
		table.pruneExpired()
		operationID, ok := message.OperationID()
		if !ok || permit.table != table || permit.authority == nil ||
			permit.direction != direction || permit.operationID != operationID {
			return OutboundAdmission{Disposition: OperationDrop}, ErrOperationIDReused
		}
		current := table.operationAuthority(operationID)
		if current != permit.authority {
			// A genuine capability can become stale after cancel/expiry. That is an
			// item-local suppression, not a malformed permit or lane failure.
			return OutboundAdmission{Disposition: OperationDrop}, nil
		}
		if current.pins >= MaximumOperationPins {
			return OutboundAdmission{Disposition: OperationDrop}, ErrOperationPinBudget
		}
	}
	disposition, continuation, err := table.observeAdmissionLocked(direction, message)
	admission := OutboundAdmission{Disposition: disposition}
	if err != nil || disposition != OperationDeliver {
		return admission, err
	}
	operationID, ok := message.OperationID()
	if !ok {
		return admission, nil
	}
	authority := table.operationAuthority(operationID)
	if authority == nil {
		return admission, ErrUnknownOperation
	}
	admission.Generation = OperationGeneration{
		table: table, authority: authority, operationID: operationID,
	}
	admission.continuation = continuation
	if direction == DirectionReceiverToSender {
		admission.Operation = OutboundOperationPermit{
			table: table, authority: authority, direction: direction, operationID: operationID,
		}
	}
	if direction == DirectionSenderToReceiver || message.kind == MessageCancel ||
		(direction == DirectionReceiverToSender && message.kind.isRequest()) {
		admission.Replay = OutboundReplayPermit{
			table: table, authority: authority, direction: direction, kind: message.kind,
			operationID: operationID, fingerprint: message.operationFingerprint(direction),
		}
	}
	// Sender transactions hold a separate generation lease across lane retries;
	// receiver requests/continuations rely on this writer-owned pin.
	admission.pin = table.pinLocked(operationID, authority, direction == DirectionReceiverToSender)
	return admission, nil
}

func (table *OperationTable) pinLocked(
	operationID OperationID,
	authority *operationAuthority,
	refresh bool,
) *outboundAdmissionPin {
	authority.pins++
	return &outboundAdmissionPin{
		table: table, authority: authority, operationID: operationID, refresh: refresh,
	}
}

func (table *OperationTable) operationAuthority(operationID OperationID) *operationAuthority {
	if active, ok := table.active[operationID]; ok {
		return active.authority
	}
	return table.tombstones[operationID].authority
}

// TerminateLocal closes operation admission before a queued terminal reaches a
// transport. This keeps a slow or blocked Send from leaving a locally terminal
// session able to accept new cross-lane work.
func (table *OperationTable) TerminateLocal() error {
	if table == nil {
		return errors.New("protocolsession: nil operation table")
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return ErrSessionTerminated
	}
	table.terminal = true
	clear(table.active)
	clear(table.tombstones)
	return nil
}

func (table *OperationTable) Terminated() bool {
	if table == nil {
		return false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.terminal
}

func (table *OperationTable) ActiveCount() int {
	table.mu.Lock()
	defer table.mu.Unlock()
	return len(table.active)
}

func (table *OperationTable) TombstoneCount() int {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpired()
	return len(table.tombstones)
}

func (table *OperationTable) beginOperation(
	operationID OperationID,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	if message.kind == MessageCancel {
		// A cancel can race ahead of a request on another lane. Remembering it
		// makes that race idempotent instead of resurrecting canceled work.
		if err := table.addTombstone(operationID, direction, message, true, 0, [32]byte{}, nil); err != nil {
			return OperationDrop, err
		}
		return OperationDeliver, nil
	}
	if direction != DirectionReceiverToSender || (!message.kind.isRequest() && message.kind != MessageLaneAttach) {
		return OperationDrop, ErrUnknownOperation
	}
	if len(table.active) >= table.limits.MaxActive {
		return OperationDrop, ErrActiveOperationBudget
	}
	authority, err := table.newOperationAuthority(message)
	if err != nil {
		return OperationDrop, err
	}
	table.active[operationID] = activeOperation{
		requestKind: message.kind, requestFingerprint: message.operationFingerprint(direction),
		authority: authority,
	}
	return OperationDeliver, nil
}

func (table *OperationTable) newOperationAuthority(message Message) (*operationAuthority, error) {
	authority := &operationAuthority{}
	if table.continuations == nil {
		return authority, nil
	}
	continuation, tracked, err := table.continuations.BeginOperationContinuation(message.kind, message.Body())
	if err != nil {
		return nil, err
	}
	if !tracked {
		return authority, nil
	}
	state, err := newOperationContinuationState(continuation)
	if err != nil {
		return nil, err
	}
	authority.continuations = state
	return authority, nil
}

func (table *OperationTable) advanceOperation(
	operationID OperationID,
	active activeOperation,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	if message.kind == MessageCancel {
		if err := table.finishOperation(operationID, active, direction, message, true); err != nil {
			return OperationDrop, err
		}
		return OperationDeliver, nil
	}
	if message.kind.isRequest() {
		if active.requestKind == message.kind &&
			active.requestFingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		return OperationDrop, ErrOperationIDReused
	}
	if message.kind == MessageLaneAttach && direction == DirectionReceiverToSender {
		return OperationDrop, ErrOperationIDReused
	}
	if !messageAllowedForOperation(active.requestKind, message.kind) {
		return OperationDrop, fmt.Errorf("%w: %d cannot answer %d", ErrUnexpectedOperation, message.kind, active.requestKind)
	}
	if message.kind.isFinal() || (active.requestKind == MessageLaneAttach && message.kind == MessageLaneAttach) {
		if err := table.finishOperation(operationID, active, direction, message, false); err != nil {
			return OperationDrop, err
		}
	}
	if active.requestKind == MessagePeerOffer && message.kind == MessagePeerAnswer {
		return table.observePeerAnswer(operationID, active, direction, message)
	}
	return OperationDeliver, nil
}

func (table *OperationTable) observePeerAnswer(
	operationID OperationID,
	active activeOperation,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	fingerprint := message.operationFingerprint(direction)
	if !active.answerSeen {
		active.answerSeen = true
		active.answerFingerprint = fingerprint
		table.active[operationID] = active
		return OperationDeliver, nil
	}
	if active.answerFingerprint == fingerprint {
		return OperationDrop, nil
	}
	if direction == DirectionSenderToReceiver {
		active.authority.recordAuthenticatedOperationViolationLocked(
			AuthenticatedOperationViolation{
				code: AuthenticatedOperationViolationConflictingPeerAnswer,
			},
		)
	}
	return OperationDrop, ErrOperationIDReused
}

func (table *OperationTable) finishOperation(
	operationID OperationID,
	active activeOperation,
	direction Direction,
	message Message,
	cancelled bool,
) error {
	if err := table.addTombstone(
		operationID, direction, message, cancelled,
		active.requestKind, active.requestFingerprint, active.authority,
	); err != nil {
		return err
	}
	clearContinuationReplayLocked(active.authority)
	delete(table.active, operationID)
	return nil
}

func deferredContinuationMatches(existing, candidate *operationAuthority) bool {
	if !existing.hasDeferredContinuationScope {
		return true
	}
	return candidate.continuations != nil &&
		candidate.continuations.scope == existing.deferredContinuationScope
}

func messageAllowedForOperation(requestKind, messageKind MessageKind) bool {
	if messageKind == MessageOperationError {
		return true
	}
	switch requestKind {
	case MessageListChildren:
		return messageKind == MessageScanProgress || messageKind == MessageCatalogResult
	case MessageOpenRevisions:
		return messageKind == MessageOpenResults
	case MessageRenewLease:
		return messageKind == MessageLeaseResult
	case MessageReleaseLease:
		return messageKind == MessageOperationComplete
	case MessageRequestBlocks:
		return messageKind == MessageBlockFragment || messageKind == MessageOperationComplete
	case MessageLaneAttach:
		return messageKind == MessageLaneAttach
	case MessagePeerOffer:
		return messageKind == MessagePeerAnswer || messageKind == MessagePeerCandidate
	default:
		return false
	}
}

func validateKindDirection(direction Direction, kind MessageKind) error {
	if !kind.valid() {
		return ErrUnknownMessageKind
	}
	switch direction {
	case DirectionReceiverToSender:
		if kind.isRequest() || kind == MessageCancel || kind == MessageOperationError || kind == MessageLaneAttach ||
			kind == MessagePeerCandidate {
			return nil
		}
	case DirectionSenderToReceiver:
		switch kind {
		case MessageCatalogResult, MessageOpenResults, MessageBlockFragment,
			MessageOperationError, MessageSessionTerminal, MessageLaneAttach,
			MessageScanProgress, MessageOperationComplete, MessageLeaseResult,
			MessagePeerAnswer, MessagePeerCandidate:
			return nil
		}
	}
	return fmt.Errorf("%w: direction=%d kind=%d", ErrInvalidDirection, direction, kind)
}
