package sessioncore

import (
	"crypto/sha256"

	"github.com/windshare/windshare/core/session/protocolsession"
)

type operationContinuationKey struct {
	kind        protocolsession.MessageKind
	fingerprint [sha256.Size]byte
}

type operationContinuationDirection struct {
	seen        map[operationContinuationKey]struct{}
	overflowSet bool
	overflowKey operationContinuationKey
}

type operationContinuationState struct {
	contract   protocolsession.OperationContinuationContract
	scope      protocolsession.OperationContinuationScope
	maximum    int
	directions map[protocolsession.Direction]*operationContinuationDirection
}

func newOperationContinuationState(
	contract protocolsession.OperationContinuationContract,
) (*operationContinuationState, error) {
	if contract == nil {
		return nil, protocolsession.ErrContinuationContract
	}
	maximum := contract.MaximumContinuations()
	scope := contract.OperationContinuationScope()
	if maximum <= 0 || maximum > protocolsession.MaximumOperationContinuations ||
		scope == (protocolsession.OperationContinuationScope{}) {
		return nil, protocolsession.ErrContinuationContract
	}
	return &operationContinuationState{
		contract:   contract,
		scope:      scope,
		maximum:    maximum,
		directions: make(map[protocolsession.Direction]*operationContinuationDirection, 2),
	}, nil
}

func (state *operationContinuationState) direction(
	direction protocolsession.Direction,
) *operationContinuationDirection {
	current := state.directions[direction]
	if current == nil {
		current = &operationContinuationDirection{
			seen: make(map[operationContinuationKey]struct{}, state.maximum),
		}
		state.directions[direction] = current
	}
	return current
}

func (state *operationContinuationState) classify(
	message protocolsession.Message,
) (operationContinuationKey, bool, error) {
	if state == nil || state.contract == nil {
		return operationContinuationKey{}, false, protocolsession.ErrContinuationContract
	}
	fingerprint, tracked, err := state.contract.ClassifyOperationContinuation(message.Kind(), message.Body())
	if err != nil || !tracked {
		return operationContinuationKey{}, tracked, err
	}
	return operationContinuationKey{kind: message.Kind(), fingerprint: fingerprint}, true, nil
}

// admitActiveContinuationLocked checks and records a continuation fingerprint.
// Returned undo closures are only invoked while table.mu is already held (or in
// an unsent rollback helper that acquires table.mu).
func (table *operationTable) admitActiveContinuationLocked(
	record *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (drop bool, undo func(), err error) {
	if record == nil || record.continuations == nil {
		return false, nil, nil
	}
	key, tracked, err := record.continuations.classify(message)
	if err != nil || !tracked {
		if err != nil && tracked {
			record.recordSenderViolationLocked(
				direction, protocolsession.AuthenticatedOperationViolationContinuationContract,
			)
		}
		return false, nil, err
	}
	current := record.continuations.direction(direction)
	if _, exists := current.seen[key]; exists {
		return true, nil, nil
	}
	if current.overflowSet {
		// The first overflow already reached the operation-local quota owner.
		// Coalesce subsequent distinct values to keep replay memory bounded.
		return true, nil, nil
	}
	if len(current.seen) < record.continuations.maximum {
		current.seen[key] = struct{}{}
		return false, func() {
			delete(current.seen, key)
		}, nil
	}
	current.overflowSet = true
	current.overflowKey = key
	return false, func() {
		if current.overflowSet && current.overflowKey == key {
			current.overflowSet = false
			current.overflowKey = operationContinuationKey{}
		}
	}, nil
}

func (table *operationTable) acceptLateContinuationLocked(
	record *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (bool, error) {
	if record == nil || record.continuations == nil {
		return false, nil
	}
	_, tracked, err := record.continuations.classify(message)
	if err != nil || !tracked {
		if err != nil && tracked {
			record.recordSenderViolationLocked(
				direction, protocolsession.AuthenticatedOperationViolationContinuationContract,
			)
		}
		return tracked, err
	}
	return true, nil
}

func (table *operationTable) acceptUnboundLateContinuationLocked(
	record *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) (bool, error) {
	if table.continuations == nil || record == nil || record.continuations != nil {
		return false, nil
	}
	scope, tracked, err := table.continuations.ClassifyUnboundOperationContinuation(message.Kind(), message.Body())
	if err != nil || !tracked {
		if err != nil && tracked {
			record.recordSenderViolationLocked(
				direction, protocolsession.AuthenticatedOperationViolationContinuationContract,
			)
		}
		return tracked, err
	}
	if scope == (protocolsession.OperationContinuationScope{}) {
		return true, protocolsession.ErrContinuationContract
	}
	if record.hasDeferredContinuationScope && record.deferredContinuationScope != scope {
		record.recordSenderViolationLocked(
			direction, protocolsession.AuthenticatedOperationViolationContinuationContract,
		)
		return true, protocolsession.ErrConflictingContinuation
	}
	record.deferredContinuationScope = scope
	record.hasDeferredContinuationScope = true
	return true, nil
}

func deferredContinuationMatches(existing, candidate *operationRecord) bool {
	if !existing.hasDeferredContinuationScope {
		return true
	}
	return candidate.continuations != nil &&
		candidate.continuations.scope == existing.deferredContinuationScope
}

func clearContinuationReplayLocked(record *operationRecord) {
	if record == nil || record.continuations == nil {
		return
	}
	clear(record.continuations.directions)
}

type retiredPeerSlot struct {
	issuedThrough  uint64
	retiredThrough uint64
	active         map[uint64]struct{}
}

func operationPeerAttempt(record *operationRecord) (protocolsession.PeerAttemptBinding, bool) {
	if record == nil || record.continuations == nil {
		return protocolsession.PeerAttemptBinding{}, false
	}
	owner, ok := record.continuations.contract.(protocolsession.PeerAttemptAuthority)
	if !ok {
		return protocolsession.PeerAttemptBinding{}, false
	}
	return owner.PeerAttemptBinding(), true
}

func (table *operationTable) admitPeerAttemptLocked(record *operationRecord) error {
	binding, ok := operationPeerAttempt(record)
	if !ok {
		return nil
	}
	if !binding.Valid() {
		return protocolsession.ErrContinuationContract
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		slot = &retiredPeerSlot{active: make(map[uint64]struct{})}
		table.peerSlots[binding.PeerSlot] = slot
	}
	if binding.AttemptSequence <= slot.issuedThrough {
		return protocolsession.ErrOperationIDReused
	}
	slot.issuedThrough = binding.AttemptSequence
	slot.active[binding.AttemptSequence] = struct{}{}
	return nil
}

func (table *operationTable) retirePeerAttemptLocked(record *operationRecord) {
	binding, ok := operationPeerAttempt(record)
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

func (table *operationTable) rollbackPeerAttemptLocked(record *operationRecord) {
	binding, ok := operationPeerAttempt(record)
	if !ok {
		return
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		return
	}
	delete(slot.active, binding.AttemptSequence)
	if binding.AttemptSequence == slot.issuedThrough && slot.retiredThrough < slot.issuedThrough {
		highest := slot.retiredThrough
		for seq := range slot.active {
			if seq > highest {
				highest = seq
			}
		}
		slot.issuedThrough = highest
	}
}

func (table *operationTable) peerContinuationBinding(
	message protocolsession.Message,
) (protocolsession.PeerAttemptBinding, bool, error) {
	kind := message.Kind()
	if kind != protocolsession.MessagePeerOffer && kind != protocolsession.MessagePeerAnswer &&
		kind != protocolsession.MessagePeerCandidate && kind != protocolsession.MessageOperationError {
		return protocolsession.PeerAttemptBinding{}, false, nil
	}
	body := message.Body()
	if kind == protocolsession.MessageOperationError {
		failure, err := protocolsession.DecodeOperationFailure(body)
		if err != nil {
			return protocolsession.PeerAttemptBinding{}, true, err
		}
		if failure.PeerAttempt == nil {
			return protocolsession.PeerAttemptBinding{}, false, nil
		}
		return *failure.PeerAttempt, true, nil
	}
	classifier, ok := table.continuations.(protocolsession.PeerAttemptContinuationClassifier)
	if !ok {
		return protocolsession.PeerAttemptBinding{}, false, nil
	}
	return classifier.ClassifyPeerAttemptContinuation(kind, body)
}

func (table *operationTable) validatePeerAttemptLocked(
	record *operationRecord,
	direction protocolsession.Direction,
	message protocolsession.Message,
) error {
	expected, ok := operationPeerAttempt(record)
	if !ok || message.Kind() == protocolsession.MessagePeerOffer {
		return nil
	}
	actual, tracked, err := table.peerContinuationBinding(message)
	if err == nil && tracked && actual != expected {
		err = protocolsession.ErrConflictingContinuation
	}
	if err != nil {
		record.recordSenderViolationLocked(
			direction, protocolsession.AuthenticatedOperationViolationContinuationContract,
		)
	}
	return err
}

func (table *operationTable) dropsRetiredPeerLocked(message protocolsession.Message) bool {
	binding, tracked, err := table.peerContinuationBinding(message)
	if err != nil || !tracked {
		return false
	}
	slot := table.peerSlots[binding.PeerSlot]
	if slot == nil {
		return false
	}
	_, active := slot.active[binding.AttemptSequence]
	if message.Kind() == protocolsession.MessagePeerOffer && binding.AttemptSequence < slot.issuedThrough {
		return !active
	}
	return binding.AttemptSequence <= slot.retiredThrough && !active
}
