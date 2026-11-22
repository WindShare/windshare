package sessioncore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationCapacityWaitReason uint8

const (
	operationWaitingActiveCapacity operationCapacityWaitReason = iota + 1
	operationWaitingRetainedCapacity
)

type operationCapacity struct {
	available  bool
	changed    <-chan struct{}
	retryAfter time.Duration
	reason     operationCapacityWaitReason
}

func (capacity operationCapacity) timer() (<-chan time.Time, func()) {
	if capacity.retryAfter <= 0 {
		return nil, func() {}
	}
	timer := time.NewTimer(capacity.retryAfter)
	return timer.C, func() { timer.Stop() }
}

func (capacity operationCapacity) wait(ctx context.Context) error {
	expiry, stop := capacity.timer()
	defer stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-capacity.changed:
	case <-expiry:
	}
	return nil
}

// operationRecord is the single per-operation authority owned by operationTable.
// It unifies protocol lifecycle, continuation replay, violation observation, and
// sender response lane affinity so callers pass one explicit Operation handle
// instead of smuggling hidden capabilities through context.Value.
type operationRecord struct {
	ref                protocolsession.OperationRef
	requestKind        protocolsession.MessageKind
	requestFingerprint [32]byte
	answerSeen         bool
	answerFingerprint  [32]byte

	preferredLane session.LaneIdentity
	sendMu        sync.Mutex
	inFlight      uint32

	continuations                   *operationContinuationState
	deferredContinuationScope       protocolsession.OperationContinuationScope
	hasDeferredContinuationScope    bool
	authenticatedViolation          protocolsession.AuthenticatedOperationViolation
	authenticatedViolationHandler   func(protocolsession.AuthenticatedOperationViolation)
	authenticatedViolationDelivered bool
}

type operationTombstone struct {
	expiresAt   time.Time
	finalKind   protocolsession.MessageKind
	fingerprint [32]byte
	cancelled   bool
	record      *operationRecord
}

type operationViolationNotification struct {
	handler   func(protocolsession.AuthenticatedOperationViolation)
	violation protocolsession.AuthenticatedOperationViolation
}

func (notification operationViolationNotification) deliver() {
	if notification.handler != nil && notification.violation.Valid() {
		notification.handler(notification.violation)
	}
}

func (record *operationRecord) recordViolationLocked(code protocolsession.AuthenticatedOperationViolationCode) {
	violation, err := protocolsession.NewAuthenticatedOperationViolation(code)
	if record == nil || err != nil || record.authenticatedViolation.Valid() {
		return
	}
	record.authenticatedViolation = violation
}

func (record *operationRecord) recordSenderViolationLocked(
	direction protocolsession.Direction,
	code protocolsession.AuthenticatedOperationViolationCode,
) {
	if direction == protocolsession.DirectionSenderToReceiver {
		record.recordViolationLocked(code)
	}
}

func (record *operationRecord) takeViolationNotificationLocked() operationViolationNotification {
	if record == nil || record.authenticatedViolationDelivered ||
		record.authenticatedViolationHandler == nil || !record.authenticatedViolation.Valid() {
		return operationViolationNotification{}
	}
	record.authenticatedViolationDelivered = true
	return operationViolationNotification{
		handler:   record.authenticatedViolationHandler,
		violation: record.authenticatedViolation,
	}
}

// operationTable is the cross-lane source of truth for operation identity,
// bounded retention, continuation replay, and per-operation lane affinity.
type operationTable struct {
	mu sync.Mutex

	now             func() time.Time
	limits          protocolsession.OperationLimits
	continuations   protocolsession.OperationContinuationClassifier
	nextToken       uint64
	active          map[protocolsession.OperationID]*operationRecord
	tombstones      map[protocolsession.OperationID]operationTombstone
	terminal        bool
	peerSlots       map[uint8]*retiredPeerSlot
	capacityChanged chan struct{}
}

func newOperationTable(
	limits protocolsession.OperationLimits,
	now func() time.Time,
	continuations protocolsession.OperationContinuationClassifier,
) (*operationTable, error) {
	if limits.MaxActive <= 0 || limits.MaxTracked <= 0 {
		return nil, fmt.Errorf("sessioncore: invalid operation limits: %+v", limits)
	}
	if now == nil {
		now = time.Now
	}
	return &operationTable{
		now:           now,
		limits:        limits,
		continuations: continuations,
		active:        make(map[protocolsession.OperationID]*operationRecord),
		tombstones:    make(map[protocolsession.OperationID]operationTombstone),
		peerSlots:     make(map[uint8]*retiredPeerSlot),
	}, nil
}

func (table *operationTable) operation(ref protocolsession.OperationRef) protocolsession.Operation {
	if ref.IsZero() {
		return protocolsession.Operation{}
	}
	return protocolsession.NewOperation(ref, table)
}

func (table *operationTable) Active(ref protocolsession.OperationRef) bool {
	if table == nil || ref.IsZero() {
		return false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	rec := table.active[ref.ID]
	return rec != nil && rec.ref == ref
}

func (table *operationTable) Current(ref protocolsession.OperationRef) bool {
	if table == nil || ref.IsZero() {
		return false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	rec := table.currentRecordLocked(ref.ID)
	return rec != nil && rec.ref == ref
}

func (table *operationTable) RequestKind(ref protocolsession.OperationRef) (protocolsession.MessageKind, bool) {
	if table == nil || ref.IsZero() {
		return 0, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	rec := table.currentRecordLocked(ref.ID)
	if rec == nil || rec.ref != ref || rec.requestKind == 0 {
		return 0, false
	}
	return rec.requestKind, true
}

func (table *operationTable) PeerAttemptBinding(
	ref protocolsession.OperationRef,
) (protocolsession.PeerAttemptBinding, bool) {
	if table == nil || ref.IsZero() {
		return protocolsession.PeerAttemptBinding{}, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	rec := table.currentRecordLocked(ref.ID)
	if rec == nil || rec.ref != ref {
		return protocolsession.PeerAttemptBinding{}, false
	}
	return operationPeerAttempt(rec)
}

func (table *operationTable) MaximumContinuations(ref protocolsession.OperationRef) (int, bool) {
	if table == nil || ref.IsZero() {
		return 0, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	rec := table.currentRecordLocked(ref.ID)
	if rec == nil || rec.ref != ref || rec.continuations == nil {
		return 0, false
	}
	return rec.continuations.maximum, true
}

func (table *operationTable) currentRecordLocked(id protocolsession.OperationID) *operationRecord {
	if active := table.active[id]; active != nil {
		return active
	}
	return table.tombstones[id].record
}

func (table *operationTable) hasCurrentID(id protocolsession.OperationID) (bool, bool) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return false, true
	}
	table.pruneExpiredLocked()
	return table.currentRecordLocked(id) != nil, false
}

func (table *operationTable) nextRefLocked(id protocolsession.OperationID) protocolsession.OperationRef {
	table.nextToken++
	return protocolsession.OperationRef{ID: id, Token: table.nextToken}
}

func (table *operationTable) WaitForCapacity(
	ctx context.Context,
	observe func(operationCapacityWaitReason),
) error {
	if table == nil {
		return protocolsession.ErrNilRuntimeDependency
	}
	var observed operationCapacityWaitReason
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		capacity, err := table.capacity()
		if err != nil || capacity.available {
			return err
		}
		if observe != nil && observed != capacity.reason {
			observed = capacity.reason
			func() {
				defer func() { _ = recover() }()
				observe(capacity.reason)
			}()
		}
		if err := capacity.wait(ctx); err != nil {
			return err
		}
	}
}

func (table *operationTable) capacity() (operationCapacity, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return operationCapacity{}, protocolsession.ErrSessionTerminated
	}
	table.pruneExpiredLocked()
	if len(table.active) < table.limits.MaxActive &&
		len(table.active)+len(table.tombstones) < table.limits.MaxTracked {
		return operationCapacity{available: true}, nil
	}
	if table.capacityChanged == nil {
		table.capacityChanged = make(chan struct{})
	}
	capacity := operationCapacity{
		changed: table.capacityChanged,
		reason:  operationWaitingRetainedCapacity,
	}
	if len(table.active) >= table.limits.MaxActive {
		capacity.reason = operationWaitingActiveCapacity
	}
	now := table.now()
	for _, tombstone := range table.tombstones {
		if tombstone.record.inFlight != 0 {
			continue
		}
		delay := tombstone.expiresAt.Sub(now)
		if delay <= 0 {
			delay = time.Nanosecond
		}
		if capacity.retryAfter == 0 || delay < capacity.retryAfter {
			capacity.retryAfter = delay
		}
	}
	return capacity, nil
}

func (table *operationTable) notifyCapacityLocked() {
	if table.capacityChanged != nil {
		close(table.capacityChanged)
		table.capacityChanged = nil
	}
}

func (table *operationTable) pruneExpiredLocked() {
	now := table.now()
	for id, tombstone := range table.tombstones {
		if !now.Before(tombstone.expiresAt) && tombstone.record.inFlight == 0 {
			delete(table.tombstones, id)
			table.notifyCapacityLocked()
		}
	}
}

func (table *operationTable) TerminateLocal() error {
	if table == nil {
		return protocolsession.ErrNilRuntimeDependency
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return protocolsession.ErrSessionTerminated
	}
	table.terminal = true
	clear(table.active)
	clear(table.tombstones)
	table.notifyCapacityLocked()
	return nil
}

func (table *operationTable) Terminated() bool {
	if table == nil {
		return true
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.terminal
}

func (table *operationTable) ActiveCount() int {
	table.mu.Lock()
	defer table.mu.Unlock()
	return len(table.active)
}

func (table *operationTable) TombstoneCount() int {
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpiredLocked()
	return len(table.tombstones)
}

type inboundAdmission struct {
	disposition protocolsession.OperationDisposition
	op          protocolsession.Operation
	undo        func()
}

// ObserveInbound atomically classifies an authenticated inbound message, updates
// operation state and lane affinity, and returns an explicit Operation handle.
func (table *operationTable) ObserveInbound(
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
) (inboundAdmission, error) {
	if table == nil {
		return inboundAdmission{disposition: protocolsession.OperationDrop}, protocolsession.ErrNilRuntimeDependency
	}
	table.mu.Lock()
	disposition, rec, undo, err := table.observeLocked(direction, lane, message)
	var notification operationViolationNotification
	if err != nil || disposition != protocolsession.OperationDeliver {
		if id, ok := message.OperationID(); ok {
			notification = table.currentRecordLocked(id).takeViolationNotificationLocked()
		}
	}
	var op protocolsession.Operation
	if err == nil && disposition == protocolsession.OperationDeliver && rec != nil {
		op = table.operation(rec.ref)
	}
	table.mu.Unlock()
	notification.deliver()
	return inboundAdmission{disposition: disposition, op: op, undo: undo}, err
}

func (table *operationTable) observeLocked(
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, *operationRecord, func(), error) {
	table.pruneExpiredLocked()
	if table.terminal {
		return protocolsession.OperationDrop, nil, nil, nil
	}
	if err := protocolsession.ValidateKindDirection(direction, message.Kind()); err != nil {
		return protocolsession.OperationDrop, nil, nil, err
	}
	if message.Kind().IsSessionControl() {
		return protocolsession.OperationDeliver, nil, nil, nil
	}
	if message.Kind() == protocolsession.MessageSessionTerminal {
		table.terminal = true
		clear(table.active)
		clear(table.tombstones)
		table.notifyCapacityLocked()
		return protocolsession.OperationSessionTerminal, nil, nil, nil
	}

	id, ok := message.OperationID()
	if !ok {
		return protocolsession.OperationDrop, nil, nil, protocolsession.ErrInvalidOperationID
	}
	current := table.currentRecordLocked(id)
	if err := table.validatePeerAttemptLocked(current, direction, message); err != nil {
		return protocolsession.OperationDrop, nil, nil, err
	}
	if tombstone, found := table.tombstones[id]; found {
		if tracked, err := table.acceptLateContinuationLocked(tombstone.record, direction, message); tracked {
			return protocolsession.OperationDrop, nil, nil, err
		}
		if tombstone.record.requestKind == 0 {
			if tracked, err := table.acceptUnboundLateContinuationLocked(tombstone.record, direction, message); tracked {
				return protocolsession.OperationDrop, nil, nil, err
			}
		}
		disposition, err := table.observeTombstoneLocked(id, tombstone, direction, message)
		return disposition, nil, nil, err
	}

	active := table.active[id]
	if active == nil {
		if table.dropsRetiredPeerLocked(message) {
			return protocolsession.OperationDrop, nil, nil, nil
		}
		disposition, rec, err := table.beginOperationLocked(id, direction, lane, message, nil)
		return disposition, rec, nil, err
	}
	disposition, err := table.advanceOperationLocked(id, active, direction, message)
	if err != nil || disposition != protocolsession.OperationDeliver {
		return disposition, nil, nil, err
	}
	drop, undo, err := table.admitActiveContinuationLocked(active, direction, message)
	if err != nil {
		return protocolsession.OperationDrop, nil, nil, err
	}
	if drop {
		return protocolsession.OperationDrop, nil, nil, nil
	}
	return disposition, active, undo, nil
}

func (table *operationTable) beginOperationLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
	violationHandler func(protocolsession.AuthenticatedOperationViolation),
) (protocolsession.OperationDisposition, *operationRecord, error) {
	if message.Kind() == protocolsession.MessageCancel {
		rec := &operationRecord{ref: table.nextRefLocked(id)}
		if err := table.addTombstoneLocked(id, direction, message, true, rec); err != nil {
			return protocolsession.OperationDrop, nil, err
		}
		return protocolsession.OperationDeliver, rec, nil
	}
	if direction != protocolsession.DirectionReceiverToSender ||
		(!message.Kind().IsRequest() && message.Kind() != protocolsession.MessageLaneAttach) {
		return protocolsession.OperationDrop, nil, protocolsession.ErrUnknownOperation
	}
	if len(table.active) >= table.limits.MaxActive {
		return protocolsession.OperationDrop, nil, protocolsession.ErrActiveOperationBudget
	}
	if len(table.active)+len(table.tombstones) >= table.limits.MaxTracked {
		return protocolsession.OperationDrop, nil, protocolsession.ErrTrackedOperationBudget
	}
	rec, err := table.newOperationRecordLocked(id, direction, lane, message, violationHandler)
	if err != nil {
		return protocolsession.OperationDrop, nil, err
	}
	if err := table.admitPeerAttemptLocked(rec); err != nil {
		return protocolsession.OperationDrop, nil, err
	}
	table.active[id] = rec
	return protocolsession.OperationDeliver, rec, nil
}

func (table *operationTable) newOperationRecordLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
	violationHandler func(protocolsession.AuthenticatedOperationViolation),
) (*operationRecord, error) {
	rec := &operationRecord{
		ref:                           table.nextRefLocked(id),
		requestKind:                   message.Kind(),
		requestFingerprint:            message.OperationFingerprint(direction),
		preferredLane:                 lane,
		authenticatedViolationHandler: violationHandler,
	}
	if table.continuations == nil {
		return rec, nil
	}
	contract, tracked, err := table.continuations.BeginOperationContinuation(message.Kind(), message.Body())
	if err != nil {
		return nil, err
	}
	if !tracked {
		return rec, nil
	}
	state, err := newOperationContinuationState(contract)
	if err != nil {
		return nil, err
	}
	rec.continuations = state
	return rec, nil
}

func (table *operationTable) advanceOperationLocked(
	id protocolsession.OperationID,
	active *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	if message.Kind() == protocolsession.MessageCancel {
		if err := table.finishOperationLocked(id, active, direction, message, true); err != nil {
			return protocolsession.OperationDrop, err
		}
		return protocolsession.OperationDeliver, nil
	}
	if message.Kind().IsRequest() {
		if active.requestKind == message.Kind() &&
			active.requestFingerprint == message.OperationFingerprint(direction) {
			return protocolsession.OperationDrop, nil
		}
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	if message.Kind() == protocolsession.MessageLaneAttach && direction == protocolsession.DirectionReceiverToSender {
		return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
	}
	if !protocolsession.MessageAllowedForOperation(active.requestKind, message.Kind()) {
		return protocolsession.OperationDrop, fmt.Errorf(
			"%w: %d cannot answer %d", protocolsession.ErrUnexpectedOperation, message.Kind(), active.requestKind,
		)
	}
	if message.Kind().IsFinal() ||
		(active.requestKind == protocolsession.MessageLaneAttach && message.Kind() == protocolsession.MessageLaneAttach) {
		if err := table.finishOperationLocked(id, active, direction, message, false); err != nil {
			return protocolsession.OperationDrop, err
		}
	}
	if active.requestKind == protocolsession.MessagePeerOffer && message.Kind() == protocolsession.MessagePeerAnswer {
		return table.observePeerAnswerLocked(active, direction, message)
	}
	return protocolsession.OperationDeliver, nil
}

func (table *operationTable) observePeerAnswerLocked(
	active *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (protocolsession.OperationDisposition, error) {
	fingerprint := message.OperationFingerprint(direction)
	if !active.answerSeen {
		active.answerSeen = true
		active.answerFingerprint = fingerprint
		return protocolsession.OperationDeliver, nil
	}
	if active.answerFingerprint == fingerprint {
		return protocolsession.OperationDrop, nil
	}
	active.recordSenderViolationLocked(direction, protocolsession.AuthenticatedOperationViolationConflictingPeerAnswer)
	return protocolsession.OperationDrop, protocolsession.ErrOperationIDReused
}

func (table *operationTable) finishOperationLocked(
	id protocolsession.OperationID,
	active *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
	cancelled bool,
) error {
	if err := table.addTombstoneLocked(id, direction, message, cancelled, active); err != nil {
		return err
	}
	table.retirePeerAttemptLocked(active)
	clearContinuationReplayLocked(active)
	delete(table.active, id)
	table.notifyCapacityLocked()
	return nil
}

func (table *operationTable) addTombstoneLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	message protocolsession.Message,
	cancelled bool,
	record *operationRecord,
) error {
	if _, exists := table.tombstones[id]; !exists {
		if _, reserved := table.active[id]; !reserved &&
			len(table.active)+len(table.tombstones) >= table.limits.MaxTracked {
			return protocolsession.ErrTrackedOperationBudget
		}
	}
	table.tombstones[id] = operationTombstone{
		expiresAt:   table.now().Add(protocolsession.OperationTombstoneLifetime),
		finalKind:   message.Kind(),
		fingerprint: message.OperationFingerprint(direction),
		cancelled:   cancelled,
		record:      record,
	}
	return nil
}
