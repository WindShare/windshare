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

func TestOperationRegistryCapabilitiesFailClosedWithoutExactLease(t *testing.T) {
	for state := ActiveLookupNone; state <= ActiveLookupAmbiguous; state++ {
		if !state.Valid() {
			t.Fatalf("active lookup state %d rejected", state)
		}
	}
	if ActiveLookupState(0).Valid() || ActiveLookupState(255).Valid() {
		t.Fatal("unknown lookup state accepted")
	}
	var nilLookup *ActiveLookup
	if nilLookup.TakeLease() != nil {
		t.Fatal("nil lookup exposed a lease")
	}
	dummyLease := &OperationRegistryLease{}
	lookup := ActiveLookup{state: ActiveLookupReopenable, lease: dummyLease}
	if lookup.TakeLease() != dummyLease || lookup.TakeLease() != nil {
		t.Fatal("lease transfer was not one-shot")
	}
	lookup = ActiveLookup{state: ActiveLookupNeedsAttention, lease: dummyLease}
	if lookup.TakeLease() != nil {
		t.Fatal("non-reopenable lookup exposed a lease")
	}

	var token destinationauthority.ReservationClaimToken
	token[0] = 1
	proof := ReservationRecoveryProof{
		claim:              destinationauthority.ReservationClaim{Token: token, Generation: 1},
		persistentIdentity: []byte("identity"),
	}
	clone := proof.Clone()
	identity := clone.PersistentIdentity()
	identity[0] ^= 0xff
	if !proof.Valid() || clone.Claim() != proof.Claim() ||
		bytes.Equal(identity, proof.PersistentIdentity()) {
		t.Fatal("recovery proof did not clone identity evidence")
	}

	if _, err := OpenOperationRegistry(nil); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil registry root = %v", err)
	}
	var registry *OperationRegistry
	if err := registry.Close(); err != nil {
		t.Fatalf("nil registry close = %v", err)
	}
	if _, _, err := registry.BeginActive(checkpointmodel.ActiveOperationKey{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil begin active = %v", err)
	}
	if _, err := registry.LookupActive(checkpointmodel.ActiveOperationKey{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil lookup = %v", err)
	}
	if _, err := registry.AcquireOperationLease(receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil acquire = %v", err)
	}
	if _, err := registry.PageOperations(OperationPageCursor{}, 1); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil page = %v", err)
	}
	if _, err := registry.RecoveryProof(checkpointmodel.OrdinaryOperationRecord{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil proof = %v", err)
	}
	if err := registry.removeAdmissionCandidate(checkpointmodel.OrdinaryAdmissionCandidate{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil candidate removal = %v", err)
	}
	if err := registry.removeOperationCandidate(checkpointmodel.OrdinaryOperationRecord{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil operation candidate removal = %v", err)
	}
}

func TestOperationRegistryLeaseAndAdmissionGuardsRejectPartialAuthority(t *testing.T) {
	var admission *ActiveAdmission
	if err := admission.Close(); err != nil {
		t.Fatalf("nil admission close = %v", err)
	}
	if err := admission.PrepareCandidate(receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil admission prepare = %v", err)
	}
	if _, _, err := admission.BeginReservation(destinationauthority.ReservationClaimSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil reservation begin = %v", err)
	}
	if err := admission.BindCandidateReservation(destinationauthority.ReservationClaim{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil candidate bind = %v", err)
	}
	if err := admission.RequireAttention(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil attention = %v", err)
	}
	if err := admission.RollbackCandidate(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil rollback = %v", err)
	}
	if _, err := admission.Create(checkpointmodel.OrdinaryOperationRecord{}, destinationauthority.ReservationClaim{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil create = %v", err)
	}

	var lease *OperationRegistryLease
	if lease.Record().Valid() || lease.Deleted() {
		t.Fatal("nil lease exposed state")
	}
	if _, err := lease.OpenFileState(true); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil file state = %v", err)
	}
	if err := lease.Replace(checkpointmodel.OrdinaryOperationRecord{}, checkpointmodel.OrdinaryOperationRecord{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil replace = %v", err)
	}
	if err := lease.CleanupEmptyFileState(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil empty cleanup = %v", err)
	}
	if err := lease.DeleteTerminal(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil delete = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("nil lease close = %v", err)
	}
}

func TestOperationRegistryNamespaceAndPagingInputsRemainBounded(t *testing.T) {
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.PageOperations(OperationPageCursor{}, 0); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero page size = %v", err)
	}
	if _, err := registry.PageOperations(
		OperationPageCursor{}, MaximumOrdinaryOperationPageSizeV1+1,
	); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("oversized page = %v", err)
	}
	if _, _, err := registry.BeginActive(checkpointmodel.ActiveOperationKey{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero active key = %v", err)
	}
	if _, err := registry.AcquireOperationLease(receivecontract.OperationID{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero operation = %v", err)
	}
	if validActiveIndexDigest(nil, checkpointmodel.OrdinaryOperationRecord{}, nil) {
		t.Fatal("invalid active index digest accepted")
	}
	for _, name := range []string{"short", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if _, err := parseOperationNamespaceName(name); !errors.Is(err, checkpointmodel.ErrRecordBinding) {
			t.Fatalf("unsafe operation namespace %q = %v", name, err)
		}
	}
	if _, err := registry.PageOperations(OperationPageCursor{after: "opaque"}, 1); err != nil {
		t.Fatalf("opaque cursor observation = %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("idempotent registry close = %v", err)
	}
	if _, err := registry.PageOperations(OperationPageCursor{}, 1); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("closed page = %v", err)
	}
}

func TestOrdinaryRecordTransitionRequiresHeldExactLifecycle(t *testing.T) {
	_, registry, lease, repository, _, _ := openRepositoryFixture(t, 0xd1)
	defer registry.Close()
	defer lease.Close()
	defer repository.Close()
	held := lease.Record()
	released, err := checkpointmodel.NextOrdinaryOperationRecord(
		held,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle: held.Lifecycle(), Lease: checkpointmodel.OrdinaryLeaseReleased,
			ClosedReason: held.ClosedReason(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOrdinaryRecordTransition(held, released); err != nil {
		t.Fatalf("held release transition = %v", err)
	}
	if err := validateOrdinaryRecordTransition(released, held); !errors.Is(err, checkpointmodel.ErrInvalidOrdinaryLifecycle) {
		t.Fatalf("released mutation transition = %v", err)
	}
	if err := validateOrdinaryRecordTransition(held, checkpointmodel.OrdinaryOperationRecord{}); !errors.Is(err, checkpointmodel.ErrInvalidOrdinaryLifecycle) {
		t.Fatalf("invalid next transition = %v", err)
	}
}
