package protocolsession

import (
	"crypto/sha256"
	"errors"
	"testing"
)

var errDefensiveContinuationClassification = errors.New("defensive continuation classification failed")

type defensiveContinuationClassifier struct {
	scope   OperationContinuationScope
	tracked bool
	err     error
}

func (classifier defensiveContinuationClassifier) BeginOperationContinuation(
	MessageKind,
	[]byte,
) (OperationContinuationAuthority, bool, error) {
	return nil, false, nil
}

func (classifier defensiveContinuationClassifier) ClassifyUnboundOperationContinuation(
	MessageKind,
	[]byte,
) (OperationContinuationScope, bool, error) {
	return classifier.scope, classifier.tracked, classifier.err
}

func TestOperationContinuationRejectsInvalidStateBeforeReplayAdmission(t *testing.T) {
	if _, err := newOperationContinuationState(nil); !errors.Is(err, ErrContinuationAuthority) {
		t.Fatalf("nil continuation authority error = %v", err)
	}
	if _, err := newOperationContinuationState(testContinuationAuthority{binding: 1}); !errors.Is(err, ErrContinuationAuthority) {
		t.Fatalf("zero continuation limit error = %v", err)
	}

	var nilState *operationContinuationState
	if _, tracked, err := nilState.classify(DirectionReceiverToSender, Message{}); tracked ||
		!errors.Is(err, ErrContinuationAuthority) {
		t.Fatalf("nil continuation state classification = tracked %t, err %v", tracked, err)
	}

	state, err := newOperationContinuationState(testContinuationAuthority{binding: 1, maximum: 1})
	if err != nil {
		t.Fatal(err)
	}
	malformedControl := Message{kind: MessagePeerCandidate, body: []byte{0xff}}
	if _, tracked, err := state.classify(DirectionSenderToReceiver, malformedControl); tracked || err == nil {
		t.Fatalf("malformed sender control classification = tracked %t, err %v", tracked, err)
	}
}

func TestOperationContinuationSettlementCannotMutateForeignReplayState(t *testing.T) {
	key := operationContinuationKey{kind: MessagePeerCandidate, fingerprint: sha256.Sum256([]byte("candidate"))}
	record := &operationContinuationRecord{}

	t.Run("incomplete-reservation", func(t *testing.T) {
		(&operationContinuationReservation{}).settleLocked(true)
	})

	t.Run("retired-authority", func(t *testing.T) {
		reservation := &operationContinuationReservation{
			authority: &operationAuthority{},
			record:    record,
		}
		reservation.settleLocked(true)
	})

	t.Run("foreign-overflow-record", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 1, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		direction := state.direction(DirectionReceiverToSender)
		direction.overflowKey = key
		direction.overflow = &operationContinuationRecord{}
		reservation := &operationContinuationReservation{
			authority: &operationAuthority{continuations: state},
			direction: DirectionReceiverToSender,
			key:       key,
			record:    record,
			overflow:  true,
		}
		reservation.settleLocked(true)
		if direction.overflow.committed {
			t.Fatal("foreign overflow record was committed")
		}
	})

	t.Run("overflow-rollback", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 1, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		direction := state.direction(DirectionReceiverToSender)
		direction.overflowKey = key
		direction.overflow = record
		direction.pendingKey = key
		direction.pending = record
		reservation := &operationContinuationReservation{
			authority: &operationAuthority{continuations: state},
			direction: DirectionReceiverToSender,
			key:       key,
			record:    record,
			overflow:  true,
		}
		reservation.settleLocked(false)
		if direction.overflow != nil || direction.pending != nil || direction.overflowKey != (operationContinuationKey{}) {
			t.Fatalf("overflow rollback retained replay authority: %+v", direction)
		}
	})

	t.Run("foreign-bounded-record", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 1, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		direction := state.direction(DirectionReceiverToSender)
		direction.records[key] = &operationContinuationRecord{}
		reservation := &operationContinuationReservation{
			authority: &operationAuthority{continuations: state},
			direction: DirectionReceiverToSender,
			key:       key,
			record:    record,
		}
		reservation.settleLocked(true)
		if direction.records[key].committed {
			t.Fatal("foreign bounded record was committed")
		}
	})
}

func TestReplayContinuationAdmissionPreservesPendingAndOverflowSemantics(t *testing.T) {
	type fixture struct {
		table     *OperationTable
		authority *operationAuthority
		message   Message
		key       operationContinuationKey
		direction *operationContinuationDirection
	}
	newFixture := func(t *testing.T, value string) fixture {
		t.Helper()
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 23, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		message := testContinuationCandidate(t, testOperationID(241), 23, value)
		key, tracked, err := state.classify(DirectionReceiverToSender, message)
		if err != nil || !tracked {
			t.Fatalf("classify replay fixture = tracked %t, err %v", tracked, err)
		}
		return fixture{
			table:     &OperationTable{},
			authority: &operationAuthority{continuations: state},
			message:   message,
			key:       key,
			direction: state.direction(DirectionReceiverToSender),
		}
	}

	t.Run("classification-error", func(t *testing.T) {
		current := newFixture(t, "known")
		wrongBinding := testContinuationCandidate(t, testOperationID(241), 24, "known")
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, wrongBinding,
		); reservation != nil || drop || !errors.Is(err, errTestContinuationBinding) {
			t.Fatalf("replay classification error = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("bounded-pending", func(t *testing.T) {
		current := newFixture(t, "pending")
		current.direction.records[current.key] = &operationContinuationRecord{}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || drop || !errors.Is(err, ErrContinuationPending) {
			t.Fatalf("pending bounded replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("bounded-committed", func(t *testing.T) {
		current := newFixture(t, "committed")
		current.direction.records[current.key] = &operationContinuationRecord{committed: true}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || drop || err != nil {
			t.Fatalf("committed bounded replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("different-pending-owner", func(t *testing.T) {
		current := newFixture(t, "blocked")
		current.direction.pending = &operationContinuationRecord{}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || drop || !errors.Is(err, ErrContinuationPending) {
			t.Fatalf("different pending replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("overflow-committed", func(t *testing.T) {
		current := newFixture(t, "overflow-committed")
		current.direction.overflowKey = current.key
		current.direction.overflow = &operationContinuationRecord{committed: true}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || drop || err != nil {
			t.Fatalf("committed overflow replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("overflow-pending", func(t *testing.T) {
		current := newFixture(t, "overflow-pending")
		current.direction.overflowKey = current.key
		current.direction.overflow = &operationContinuationRecord{}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || drop || !errors.Is(err, ErrContinuationPending) {
			t.Fatalf("pending overflow replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("overflow-coalesces-distinct", func(t *testing.T) {
		current := newFixture(t, "overflow-distinct")
		other := newFixture(t, "overflow-owner")
		current.direction.overflowKey = other.key
		current.direction.overflow = &operationContinuationRecord{committed: true}
		if reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		); reservation != nil || !drop || err != nil {
			t.Fatalf("distinct overflow replay = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})

	t.Run("fresh-reservation", func(t *testing.T) {
		current := newFixture(t, "fresh")
		reservation, drop, err := current.table.reserveReplayContinuationLocked(
			current.authority, DirectionReceiverToSender, current.message,
		)
		if err != nil || drop || reservation == nil {
			t.Fatalf("fresh replay reservation = reservation %v, drop %t, err %v", reservation, drop, err)
		}
		reservation.rollback()
	})
}

func TestContinuationAuthorityFailuresRecordOnlyAuthenticatedSenderTraffic(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 31, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		authority := &operationAuthority{continuations: state}
		message := testSignedContinuationCandidate(t, testOperationID(242), 32, "wrong-binding")
		if reservation, drop, err := (&OperationTable{}).reserveActiveContinuationLocked(
			authority, DirectionSenderToReceiver, message,
		); reservation != nil || drop || !errors.Is(err, errTestContinuationBinding) {
			t.Fatalf("active authority failure = reservation %v, drop %t, err %v", reservation, drop, err)
		}
		if authority.authenticatedViolation.code != AuthenticatedOperationViolationContinuationAuthority {
			t.Fatalf("active authenticated violation = %+v", authority.authenticatedViolation)
		}
	})

	t.Run("late", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 33, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		authority := &operationAuthority{continuations: state}
		message := testSignedContinuationCandidate(t, testOperationID(243), 34, "wrong-binding")
		if tracked, err := (&OperationTable{}).acceptLateContinuationLocked(
			authority, DirectionSenderToReceiver, message,
		); !tracked || !errors.Is(err, errTestContinuationBinding) {
			t.Fatalf("late authority failure = tracked %t, err %v", tracked, err)
		}
		if authority.authenticatedViolation.code != AuthenticatedOperationViolationContinuationAuthority {
			t.Fatalf("late authenticated violation = %+v", authority.authenticatedViolation)
		}
	})

	t.Run("active-overflow-pending", func(t *testing.T) {
		state, err := newOperationContinuationState(testContinuationAuthority{binding: 35, maximum: 1})
		if err != nil {
			t.Fatal(err)
		}
		message := testContinuationCandidate(t, testOperationID(244), 35, "overflow")
		key, tracked, err := state.classify(DirectionReceiverToSender, message)
		if err != nil || !tracked {
			t.Fatal(err)
		}
		direction := state.direction(DirectionReceiverToSender)
		direction.overflowKey = key
		direction.overflow = &operationContinuationRecord{}
		if reservation, drop, err := (&OperationTable{}).reserveActiveContinuationLocked(
			&operationAuthority{continuations: state}, DirectionReceiverToSender, message,
		); reservation != nil || drop || !errors.Is(err, ErrContinuationPending) {
			t.Fatalf("active pending overflow = reservation %v, drop %t, err %v", reservation, drop, err)
		}
	})
}

func TestUnboundLateContinuationRejectsUnauthenticatedOrConflictingScope(t *testing.T) {
	operationID := testOperationID(245)

	t.Run("malformed-sender-wrapper", func(t *testing.T) {
		table := &OperationTable{continuations: defensiveContinuationClassifier{tracked: true}}
		message := Message{kind: MessagePeerCandidate, body: []byte{0xff}}
		if tracked, err := table.acceptUnboundLateContinuationLocked(
			&operationAuthority{}, DirectionSenderToReceiver, message,
		); tracked || err == nil {
			t.Fatalf("malformed sender wrapper = tracked %t, err %v", tracked, err)
		}
	})

	t.Run("classifier-error", func(t *testing.T) {
		table := &OperationTable{continuations: defensiveContinuationClassifier{
			scope: testContinuationScope(41), tracked: true, err: errDefensiveContinuationClassification,
		}}
		authority := &operationAuthority{}
		message := testSignedContinuationCandidate(t, operationID, 41, "candidate")
		if tracked, err := table.acceptUnboundLateContinuationLocked(
			authority, DirectionSenderToReceiver, message,
		); !tracked || !errors.Is(err, errDefensiveContinuationClassification) {
			t.Fatalf("classifier error = tracked %t, err %v", tracked, err)
		}
		if authority.authenticatedViolation.code != AuthenticatedOperationViolationContinuationAuthority {
			t.Fatalf("classifier authenticated violation = %+v", authority.authenticatedViolation)
		}
	})

	t.Run("zero-scope", func(t *testing.T) {
		table := &OperationTable{continuations: defensiveContinuationClassifier{tracked: true}}
		message := testContinuationCandidate(t, operationID, 42, "candidate")
		if tracked, err := table.acceptUnboundLateContinuationLocked(
			&operationAuthority{}, DirectionReceiverToSender, message,
		); !tracked || !errors.Is(err, ErrContinuationAuthority) {
			t.Fatalf("zero scope = tracked %t, err %v", tracked, err)
		}
	})

	t.Run("conflicting-authenticated-scope", func(t *testing.T) {
		learned := testContinuationScope(43)
		candidate := testContinuationScope(44)
		table := &OperationTable{continuations: defensiveContinuationClassifier{scope: candidate, tracked: true}}
		authority := &operationAuthority{deferredContinuationScope: learned, hasDeferredContinuationScope: true}
		message := testSignedContinuationCandidate(t, operationID, 44, "candidate")
		if tracked, err := table.acceptUnboundLateContinuationLocked(
			authority, DirectionSenderToReceiver, message,
		); !tracked || !errors.Is(err, ErrConflictingContinuation) {
			t.Fatalf("conflicting scope = tracked %t, err %v", tracked, err)
		}
		if authority.authenticatedViolation.code != AuthenticatedOperationViolationContinuationAuthority {
			t.Fatalf("conflicting authenticated violation = %+v", authority.authenticatedViolation)
		}
	})
}
