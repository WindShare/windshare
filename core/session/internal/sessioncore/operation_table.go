package sessioncore

import (
	"context"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/protocolsession"
)

const (
	operationReplayWindowBits  = protocolsession.OperationReplayWindow
	operationReplayWindowMask  = operationReplayWindowBits - 1
	operationReplayWindowWords = operationReplayWindowBits / 64
)

type operationReplayStatus uint8

const (
	operationReplayFresh operationReplayStatus = iota
	operationReplayDuplicate
	operationReplayBelowWindow
)

// operationReplayWindow is a fixed 8 KiB O(1) ring-buffer bitset that tracks
// seen monotonic 64-bit operation IDs across multi-lane reordering without
// retaining per-operation wall-clock tombstones or throttling session throughput.
type operationReplayWindow struct {
	highWater uint64
	bits      [operationReplayWindowWords]uint64
}

func (w *operationReplayWindow) classify(id protocolsession.OperationID) operationReplayStatus {
	v := id.Uint64()
	if v == 0 || v > w.highWater {
		return operationReplayFresh
	}
	diff := w.highWater - v
	if diff >= operationReplayWindowBits {
		return operationReplayBelowWindow
	}
	slot := v & operationReplayWindowMask
	if (w.bits[slot>>6] & (uint64(1) << (slot & 63))) != 0 {
		return operationReplayDuplicate
	}
	return operationReplayFresh
}

func (w *operationReplayWindow) mark(id protocolsession.OperationID) {
	v := id.Uint64()
	if v == 0 {
		return
	}
	if w.highWater == 0 {
		w.highWater = v
		slot := v & operationReplayWindowMask
		w.bits[slot>>6] |= uint64(1) << (slot & 63)
		return
	}
	if v > w.highWater {
		shift := v - w.highWater
		if shift >= operationReplayWindowBits {
			clear(w.bits[:])
		} else if shift > 1 {
			w.clearInterval(w.highWater+1, v)
		}
		w.highWater = v
		slot := v & operationReplayWindowMask
		w.bits[slot>>6] |= uint64(1) << (slot & 63)
		return
	}
	if w.highWater-v < operationReplayWindowBits {
		slot := v & operationReplayWindowMask
		w.bits[slot>>6] |= uint64(1) << (slot & 63)
	}
}

// clearInterval zeroes ring-buffer bits for every skipped ID in [from, to).
// Callers guarantee 0 < to - from < operationReplayWindowBits.
func (w *operationReplayWindow) clearInterval(from, to uint64) {
	for k := from; k < to; {
		slot := k & operationReplayWindowMask
		word := slot >> 6
		bitOffset := slot & 63
		remaining := to - k
		if bitOffset == 0 && remaining >= 64 {
			w.bits[word] = 0
			k += 64
			continue
		}
		count := min(remaining, 64-bitOffset)
		var clearMask uint64
		if count == 64 {
			clearMask = ^uint64(0)
		} else {
			clearMask = ((uint64(1) << count) - 1) << bitOffset
		}
		w.bits[word] &^= clearMask
		k += count
	}
}

type operationCapacity struct {
	available bool
	changed   <-chan struct{}
}

func (capacity operationCapacity) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-capacity.changed:
		return nil
	}
}

// operationRecord is the single per-operation authority owned by operationTable.
// It unifies protocol lifecycle, continuation replay, violation observation, and
// sender response lane affinity so callers pass one explicit Operation handle
// instead of smuggling hidden capabilities through context.Value.
type operationRecord struct {
	id                 protocolsession.OperationID
	requestKind        protocolsession.MessageKind
	requestFingerprint [32]byte
	answerSeen         bool
	answerFingerprint  [32]byte

	preferredLane session.LaneIdentity
	sendMu        sync.Mutex
	inFlight      uint32
	// Intrusive FIFO retirement links owned by operationTable under table.mu.
	tombstonePrev *operationRecord
	tombstoneNext *operationRecord

	continuations                   *operationContinuationState
	deferredContinuationScope       protocolsession.OperationContinuationScope
	hasDeferredContinuationScope    bool
	authenticatedViolation          protocolsession.AuthenticatedOperationViolation
	authenticatedViolationHandler   func(protocolsession.AuthenticatedOperationViolation)
	authenticatedViolationDelivered bool
}

type operationTombstone struct {
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
// sliding-window replay protection, continuation replay, and per-operation
// lane affinity.
type operationTable struct {
	mu sync.Mutex

	limits          protocolsession.OperationLimits
	continuations   protocolsession.OperationContinuationClassifier
	seen            operationReplayWindow
	lastIssued      protocolsession.OperationID
	active          map[protocolsession.OperationID]*operationRecord
	tombstones      map[protocolsession.OperationID]operationTombstone
	tombstoneHead   *operationRecord
	tombstoneTail   *operationRecord
	terminal        bool
	peerSlots       map[uint8]*retiredPeerSlot
	capacityChanged chan struct{}
}

func newOperationTable(
	limits protocolsession.OperationLimits,
	continuations protocolsession.OperationContinuationClassifier,
) (*operationTable, error) {
	if limits.MaxActive <= 0 || limits.MaxRecentTombstones <= 0 {
		return nil, fmt.Errorf("sessioncore: invalid operation limits: %+v", limits)
	}
	return &operationTable{
		limits:        limits,
		continuations: continuations,
		active:        make(map[protocolsession.OperationID]*operationRecord),
		tombstones:    make(map[protocolsession.OperationID]operationTombstone),
		peerSlots:     make(map[uint8]*retiredPeerSlot),
	}, nil
}

func (table *operationTable) operation(id protocolsession.OperationID) protocolsession.Operation {
	if id.IsZero() {
		return protocolsession.Operation{}
	}
	return protocolsession.NewOperation(id, table)
}

func (table *operationTable) Active(id protocolsession.OperationID) bool {
	if table == nil || id.IsZero() {
		return false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.active[id] != nil
}

func (table *operationTable) Current(id protocolsession.OperationID) bool {
	if table == nil || id.IsZero() {
		return false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	return table.currentRecordLocked(id) != nil
}

func (table *operationTable) RequestKind(id protocolsession.OperationID) (protocolsession.MessageKind, bool) {
	if table == nil || id.IsZero() {
		return 0, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	rec := table.currentRecordLocked(id)
	if rec == nil || rec.requestKind == 0 {
		return 0, false
	}
	return rec.requestKind, true
}

func (table *operationTable) PeerAttemptBinding(
	id protocolsession.OperationID,
) (protocolsession.PeerAttemptBinding, bool) {
	if table == nil || id.IsZero() {
		return protocolsession.PeerAttemptBinding{}, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	rec := table.currentRecordLocked(id)
	if rec == nil {
		return protocolsession.PeerAttemptBinding{}, false
	}
	return operationPeerAttempt(rec)
}

func (table *operationTable) MaximumContinuations(id protocolsession.OperationID) (int, bool) {
	if table == nil || id.IsZero() {
		return 0, false
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	rec := table.currentRecordLocked(id)
	if rec == nil || rec.continuations == nil {
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
	return table.currentRecordLocked(id) != nil || table.seen.classify(id) != operationReplayFresh, false
}

// WaitForCapacity waits until an active operation slot is available. Because
// retired tombstones are evicted FIFO rather than held for a fixed TTL,
// capacity waits only occur when MaxActive operations are simultaneously active.
func (table *operationTable) WaitForCapacity(ctx context.Context, observe func()) error {
	if table == nil {
		return protocolsession.ErrNilRuntimeDependency
	}
	observed := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		capacity, err := table.capacity()
		if err != nil || capacity.available {
			return err
		}
		if observe != nil && !observed {
			observed = true
			func() {
				defer func() { _ = recover() }()
				observe()
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
	if len(table.active) < table.limits.MaxActive {
		return operationCapacity{available: true}, nil
	}
	if table.capacityChanged == nil {
		table.capacityChanged = make(chan struct{})
	}
	return operationCapacity{changed: table.capacityChanged}, nil
}

func (table *operationTable) notifyCapacityLocked() {
	if table.capacityChanged != nil {
		close(table.capacityChanged)
		table.capacityChanged = nil
	}
}

func (table *operationTable) putTombstoneLocked(
	id protocolsession.OperationID,
	tombstone operationTombstone,
) {
	if existing, existed := table.tombstones[id]; existed {
		if existing.record != nil && existing.record != tombstone.record {
			table.unlinkTombstoneLocked(existing.record)
			table.appendTombstoneLocked(tombstone.record)
		}
		table.tombstones[id] = tombstone
		return
	}
	table.tombstones[id] = tombstone
	table.appendTombstoneLocked(tombstone.record)
	table.evictOverflowTombstonesLocked()
}

func (table *operationTable) appendTombstoneLocked(rec *operationRecord) {
	if rec == nil {
		return
	}
	rec.tombstonePrev = table.tombstoneTail
	rec.tombstoneNext = nil
	if table.tombstoneTail != nil {
		table.tombstoneTail.tombstoneNext = rec
	} else {
		table.tombstoneHead = rec
	}
	table.tombstoneTail = rec
}

func (table *operationTable) unlinkTombstoneLocked(rec *operationRecord) {
	if rec.tombstonePrev != nil {
		rec.tombstonePrev.tombstoneNext = rec.tombstoneNext
	} else {
		table.tombstoneHead = rec.tombstoneNext
	}
	if rec.tombstoneNext != nil {
		rec.tombstoneNext.tombstonePrev = rec.tombstonePrev
	} else {
		table.tombstoneTail = rec.tombstonePrev
	}
	rec.tombstonePrev = nil
	rec.tombstoneNext = nil
}

func (table *operationTable) clearTombstonesLocked() {
	// Detach intrusive links so any lingering in-flight releaser holding one
	// record cannot retain the rest of the retired chain for GC.
	for cur := table.tombstoneHead; cur != nil; {
		next := cur.tombstoneNext
		cur.tombstonePrev = nil
		cur.tombstoneNext = nil
		cur = next
	}
	table.tombstoneHead = nil
	table.tombstoneTail = nil
	clear(table.tombstones)
}

func (table *operationTable) evictOverflowTombstonesLocked() {
	excess := len(table.tombstones) - table.limits.MaxRecentTombstones
	for cur := table.tombstoneHead; excess > 0 && cur != nil; {
		next := cur.tombstoneNext
		if cur.inFlight == 0 {
			table.unlinkTombstoneLocked(cur)
			delete(table.tombstones, cur.id)
			excess--
		}
		cur = next
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
	table.clearTombstonesLocked()
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
		op = table.operation(rec.id)
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
		table.clearTombstonesLocked()
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
		if direction == protocolsession.DirectionSenderToReceiver {
			if id <= table.lastIssued && !isPeerControlResponse(message.Kind()) {
				return protocolsession.OperationDrop, nil, nil, nil
			}
			return protocolsession.OperationDrop, nil, nil, protocolsession.ErrUnknownOperation
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

func isPeerControlResponse(kind protocolsession.MessageKind) bool {
	return kind == protocolsession.MessagePeerAnswer || kind == protocolsession.MessagePeerCandidate
}

func (table *operationTable) beginOperationLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
	violationHandler func(protocolsession.AuthenticatedOperationViolation),
) (protocolsession.OperationDisposition, *operationRecord, error) {
	if message.Kind() == protocolsession.MessageCancel {
		if table.seen.classify(id) != operationReplayFresh {
			// Already seen or older than the replay window; nothing active to cancel.
			return protocolsession.OperationDrop, nil, nil
		}
		table.seen.mark(id)
		rec := &operationRecord{id: id}
		table.addTombstoneLocked(id, direction, message, true, rec)
		return protocolsession.OperationDeliver, rec, nil
	}
	if direction == protocolsession.DirectionReceiverToSender &&
		message.Kind() == protocolsession.MessagePeerCandidate &&
		table.seen.classify(id) == operationReplayBelowWindow {
		// Trickled ICE candidates can follow a delayed PeerOffer that fell behind
		// the replay window before the receiver observes OperationRetry. Validate
		// the continuation contract when configured, then drop without killing
		// the healthy session or advancing peerSlots for an unadmitted offer.
		return protocolsession.OperationDrop, nil, table.validateBelowWindowPeerCandidateLocked(message)
	}
	if direction != protocolsession.DirectionReceiverToSender ||
		(!message.Kind().IsRequest() && message.Kind() != protocolsession.MessageLaneAttach) {
		return protocolsession.OperationDrop, nil, protocolsession.ErrUnknownOperation
	}
	switch table.seen.classify(id) {
	case operationReplayDuplicate:
		return protocolsession.OperationDrop, nil, protocolsession.ErrOperationIDReused
	case operationReplayBelowWindow:
		// An ID older than the bounded sliding window can be a legitimate request
		// delayed on a slow lane rather than a proven replay. Refuse admission to
		// preserve operation-ID uniqueness, but let the lane request a fresh-ID retry
		// instead of terminating the healthy session.
		return protocolsession.OperationBelowReplayWindow, nil, nil
	}
	if len(table.active) >= table.limits.MaxActive {
		return protocolsession.OperationDrop, nil, protocolsession.ErrActiveOperationBudget
	}
	rec, err := table.newOperationRecordLocked(id, direction, lane, message, violationHandler)
	if err != nil {
		return protocolsession.OperationDrop, nil, err
	}
	if err := table.admitPeerAttemptLocked(rec); err != nil {
		return protocolsession.OperationDrop, nil, err
	}
	table.seen.mark(id)
	table.active[id] = rec
	return protocolsession.OperationDeliver, rec, nil
}

func (table *operationTable) validateBelowWindowPeerCandidateLocked(message protocolsession.Message) error {
	if table.continuations == nil {
		return nil
	}
	scope, tracked, err := table.continuations.ClassifyUnboundOperationContinuation(message.Kind(), message.Body())
	if err != nil {
		return err
	}
	if tracked && scope == (protocolsession.OperationContinuationScope{}) {
		return protocolsession.ErrContinuationContract
	}
	return nil
}

func (table *operationTable) newOperationRecordLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	lane session.LaneIdentity,
	message protocolsession.Message,
	violationHandler func(protocolsession.AuthenticatedOperationViolation),
) (*operationRecord, error) {
	rec := &operationRecord{
		id:                            id,
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
		table.finishOperationLocked(id, active, direction, message, true)
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
		table.finishOperationLocked(id, active, direction, message, false)
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
) {
	table.addTombstoneLocked(id, direction, message, cancelled, active)
	table.retirePeerAttemptLocked(active)
	clearContinuationReplayLocked(active)
	delete(table.active, id)
	table.notifyCapacityLocked()
}

func (table *operationTable) addTombstoneLocked(
	id protocolsession.OperationID,
	direction protocolsession.Direction,
	message protocolsession.Message,
	cancelled bool,
	record *operationRecord,
) {
	table.putTombstoneLocked(id, operationTombstone{
		finalKind:   message.Kind(),
		fingerprint: message.OperationFingerprint(direction),
		cancelled:   cancelled,
		record:      record,
	})
}
