package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOrdinaryRecordsExposeEveryBoundCoordinateWithoutAliasing(t *testing.T) {
	intent, authority := ordinaryOperationIntentFixture(t, 0xd1)
	key, err := NewActiveOperationKeyV1(intent.SelectionSpec().Digest(), authority)
	if err != nil {
		t.Fatal(err)
	}
	locator := ordinaryClaimLocatorFixture(t, 0xd2)
	record, err := NewOrdinaryOperationRecord(OrdinaryOperationRecordSpec{
		ActiveKey: key, Intent: intent, LifecycleGeneration: 1,
		ReservationClaim: locator, Lifecycle: OrdinaryOperationActive,
		Lease: OrdinaryLeaseHeld, ClosedReason: OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if locator.Token() == ([sha256.Size]byte{}) || locator.Generation() != 3 ||
		record.ActiveOperationKey() != key || record.OperationID() != intent.OperationID() ||
		record.LifecycleGeneration() != 1 || record.ClosedReason() != OrdinaryReasonNone {
		t.Fatalf("operation coordinates = locator %+v record %+v", locator, record)
	}
	next, err := NextOrdinaryOperationRecord(record, NextOrdinaryOperationRecordSpec{
		Lifecycle: OrdinaryOperationActive, Lease: OrdinaryLeaseReleased,
		ClosedReason: OrdinaryReasonNone,
	})
	if err != nil || next.LifecycleGeneration() != 2 ||
		!SameOrdinaryOperation(record, next) {
		t.Fatalf("next operation record = (%+v, %v)", next, err)
	}
	if SameOrdinaryOperation(record, OrdinaryOperationRecord{}) {
		t.Fatal("zero row shared operation authority")
	}
	candidate, err := NewOrdinaryAdmissionCandidate(key, intent.OperationID())
	if err != nil {
		t.Fatal(err)
	}
	if candidate.ActiveOperationKey() != key || candidate.OperationID() != intent.OperationID() {
		t.Fatalf("candidate coordinates = %+v", candidate)
	}
}

func TestReservationClaimAccessorsPreserveExactNameAndIdentityBinding(t *testing.T) {
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat(
		[]byte{0xe1}, receivecontract.StableIdentityBytes,
	))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat(
		[]byte{0xe2}, receivecontract.StableIdentityBytes,
	))
	record, err := NewReservationClaimRecord(ReservationClaimRecordSpec{
		CanonicalNameKey: "download", OperationID: operation, ReservationID: reservationID,
		RequestedName: "download", ReservedName: "download-2",
		EntryKind: receivecontract.ContainerEntryResultRoot, CollisionIndex: 2,
		Generation: 1, Phase: ReservationClaimed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.CanonicalNameKey() != "download" || record.OperationID() != operation ||
		record.ReservationID() != reservationID || record.RequestedName() != "download" ||
		record.ReservedName() != "download-2" ||
		record.EntryKind() != receivecontract.ContainerEntryResultRoot ||
		record.CollisionIndex() != 2 || record.ReservationDigest() != (receivecontract.BindingDigest{}) {
		t.Fatalf("reservation accessors = %+v", record)
	}
	if _, err := NewReservationClaimRecord(ReservationClaimRecordSpec{}); !errors.Is(
		err, ErrInvalidReservationClaim,
	) {
		t.Fatalf("zero reservation claim = %v", err)
	}
	if _, err := NewReservationClaimLocator([sha256.Size]byte{}, 0); !errors.Is(
		err, ErrInvalidOrdinaryOperation,
	) {
		t.Fatalf("zero claim locator = %v", err)
	}
	if _, err := ActiveOperationKeyFromBytes(nil); !errors.Is(err, ErrInvalidOrdinaryOperation) {
		t.Fatalf("zero active key bytes = %v", err)
	}
	if _, err := NewOrdinaryAdmissionCandidate(ActiveOperationKey{}, operation); !errors.Is(
		err, ErrInvalidOrdinaryOperation,
	) {
		t.Fatalf("zero admission key = %v", err)
	}
}
