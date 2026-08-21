package checkpointmodel

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReservationClaimCanonicalPhasesAndDefensiveIdentity(t *testing.T) {
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x71}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{0x72}, receivecontract.StableIdentityBytes))
	digest, _ := receivecontract.BindingDigestFromBytes(bytes.Repeat([]byte{0x73}, sha256.Size))
	base, err := NewReservationClaimRecord(ReservationClaimRecordSpec{
		CanonicalNameKey: "download", OperationID: operation, ReservationID: reservationID,
		RequestedName: "download", LogicalReservedName: "download", PhysicalName: "download",
		EntryKind:  receivecontract.ContainerEntryResultRoot,
		Generation: 1, Phase: ReservationClaimed,
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindReservationClaim(base, digest)
	identity := []byte("persistent-directory-identity")
	directory, directoryErr := BindReservationDirectory(bound, identity)
	var operationDigest [sha256.Size]byte
	operationDigest[0] = 1
	operationBound, operationErr := BindReservationOperation(directory, operationDigest)
	if err != nil || directoryErr != nil || operationErr != nil || operationBound.Generation() != 4 ||
		operationBound.Phase() != ReservationOperationBound {
		t.Fatalf("phases = %v/%v/%v record=%+v", err, directoryErr, operationErr, operationBound)
	}
	identity[0] ^= 0xff
	returned := operationBound.PersistentIdentity()
	returned[0] ^= 0xff
	if bytes.Equal(identity, operationBound.PersistentIdentity()) || bytes.Equal(returned, operationBound.PersistentIdentity()) {
		t.Fatal("claim exposed caller or returned identity storage")
	}
	encoded, err := EncodeReservationClaimRecord(operationBound)
	restored, decodeErr := ReservationClaimRecordFromCanonicalBytes(encoded, operationBound.Token(), operationBound.Generation())
	if err != nil || decodeErr != nil || !SameReservationClaim(restored, operationBound) ||
		restored.OperationBindingDigest() != operationDigest {
		t.Fatalf("round trip = %+v, %v/%v", restored, err, decodeErr)
	}
	if _, err := DecodeReservationClaimRecord(append(encoded, 0)); !errors.Is(err, ErrInvalidReservationClaim) {
		t.Fatalf("trailing claim = %v", err)
	}
	if _, err := ReservationClaimRecordFromCanonicalBytes(encoded, operationBound.Token(), operationBound.Generation()+1); !errors.Is(err, ErrInvalidReservationClaim) {
		t.Fatalf("stale generation = %v", err)
	}
}

func TestReservationClaimSingleFileBindsWithoutDirectoryIdentity(t *testing.T) {
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x81}, receivecontract.StableIdentityBytes))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{0x82}, receivecontract.StableIdentityBytes))
	var digest receivecontract.BindingDigest
	digest, _ = receivecontract.BindingDigestFromBytes(bytes.Repeat([]byte{0x83}, sha256.Size))
	base, err := NewReservationClaimRecord(ReservationClaimRecordSpec{
		CanonicalNameKey: "file", OperationID: operation, ReservationID: reservationID,
		RequestedName: "file", LogicalReservedName: "file", PhysicalName: "file",
		EntryKind:  receivecontract.ContainerEntrySingleFile,
		Generation: 1, Phase: ReservationClaimed,
	})
	bound, bindErr := BindReservationClaim(base, digest)
	if _, err := BindReservationDirectory(bound, []byte("identity")); !errors.Is(err, ErrInvalidReservationClaim) {
		t.Fatalf("single-file directory bind = %v", err)
	}
	var operationDigest [sha256.Size]byte
	operationDigest[0] = 1
	operationBound, operationErr := BindReservationOperation(bound, operationDigest)
	if err != nil || bindErr != nil || operationErr != nil || len(operationBound.PersistentIdentity()) != 0 {
		t.Fatalf("single-file phases = %v/%v/%v record=%+v", err, bindErr, operationErr, operationBound)
	}
}
