package checkpointstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func resultRootReservationClaimFixture(
	t *testing.T,
	fill byte,
	collision uint32,
) (destinationauthority.ReservationClaimSpec, receivecontract.DestinationReservation) {
	t.Helper()
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat(
		[]byte{fill}, receivecontract.StableIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat(
		[]byte{fill + 1}, receivecontract.StableIdentityBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat(
		[]byte{fill + 2}, receivecontract.AuthorityRefBytes,
	))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	requested, _ := artifact.DirectoryTree()
	root, _ := requested.ResultRoot()
	reservedName, err := receivecontract.CollisionName(operation, root.Name(), collision, false)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, reservedName, collision,
	)
	if err != nil {
		t.Fatal(err)
	}
	return destinationauthority.ReservationClaimSpec{
		CanonicalNameKey: reservedName,
		OperationID:      operation,
		ReservationID:    reservationID,
		EntryKind:        reservation.EntryKind(),
		RequestedName:    reservation.RequestedName(),
		ReservedName:     reservation.ReservedName(),
		CollisionIndex:   collision,
	}, reservation
}

func TestReservationClaimDirectoryIdentityRollbackAndPagingAreExact(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	spec, reservation := resultRootReservationClaimFixture(t, 0x81, 0)
	handleValue, outcome, err := registry.BeginReservation(spec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("begin directory claim = (%d, %v)", outcome, err)
	}
	handle := handleValue.(*ReservationClaimHandle)
	if outcome, err = handle.BindReservation(reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind directory reservation = (%d, %v)", outcome, err)
	}
	identity := []byte("durable-directory-identity")
	if outcome, err = handle.BindDirectoryIdentity(identity); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind directory identity = (%d, %v)", outcome, err)
	}
	claim := handle.Claim()
	if !claim.Valid() {
		t.Fatal("bound directory claim is invalid")
	}
	if outcome, err = handle.Rollback(); err == nil ||
		outcome != destinationauthority.ReservationMetadataClaimIndeterminate {
		t.Fatalf("post-publication rollback = (%d, %v)", outcome, err)
	}

	page, err := registry.PageReservationClaims(ReservationClaimPageCursor{}, 1)
	if err != nil || len(page.Records()) != 1 || page.Unknown() ||
		!bytes.Equal(page.Records()[0].PersistentIdentity(), identity) {
		t.Fatalf("claim page = (%+v, unknown=%t, %v)", page.Records(), page.Unknown(), err)
	}
	cursor := NewReservationClaimPageCursor([32]byte(claim.Token))
	if cursor.IsZero() {
		t.Fatal("nonzero reservation cursor collapsed")
	}
	after, err := registry.PageReservationClaims(cursor, 1)
	if err != nil || len(after.Records()) != 0 || !after.Next().IsZero() {
		t.Fatalf("claim page after cursor = (%+v, %v)", after.Records(), err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if handle.Claim().Valid() {
		t.Fatal("closed claim handle exposed authority")
	}
	if _, err := handle.BindDirectoryIdentity(identity); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("closed identity bind error = %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	rollbackSpec, rollbackReservation := resultRootReservationClaimFixture(t, 0x91, 1)
	rollbackValue, outcome, err := registry.BeginReservation(rollbackSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("begin rollback claim = (%d, %v)", outcome, err)
	}
	rollback := rollbackValue.(*ReservationClaimHandle)
	if outcome, err = rollback.BindReservation(rollbackReservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("bind rollback reservation = (%d, %v)", outcome, err)
	}
	if outcome, err = rollback.Rollback(); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		t.Fatalf("rollback bound-only claim = (%d, %v)", outcome, err)
	}
	if rollback.Claim().Valid() {
		t.Fatal("rolled-back handle retained its claim")
	}
	if err := rollback.Close(); err != nil {
		t.Fatal(err)
	}

	page, err = registry.PageReservationClaims(ReservationClaimPageCursor{}, 8)
	if err != nil || len(page.Records()) != 1 {
		t.Fatalf("post-rollback claims = (%+v, %v)", page.Records(), err)
	}
	if _, err := registry.PageReservationClaims(ReservationClaimPageCursor{}, 0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero claim page error = %v", err)
	}
	if NewReservationClaimPageCursor([32]byte{}).IsZero() != true {
		t.Fatal("zero reservation cursor became nonzero")
	}
	var nilHandle *ReservationClaimHandle
	if nilHandle.Claim().Valid() || nilHandle.Close() != nil {
		t.Fatal("nil reservation handle exposed state")
	}
	if _, err := nilHandle.Rollback(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil rollback error = %v", err)
	}

	_ = checkpointmodel.ReservationClaimed
}
