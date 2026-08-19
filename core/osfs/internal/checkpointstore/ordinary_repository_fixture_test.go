package checkpointstore

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/core/transfer"
)

type operationFixture struct {
	intent transfer.ReceiveIntent
}

func openRepositoryFixture(
	t *testing.T,
	fill byte,
) (*memoryDirectory, OperationRegistry, *OperationRegistryLease, Repository, checkpointmodel.Ownership, operationFixture) {
	t.Helper()
	control := newMemoryDirectory()
	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	fixture := ordinaryRegistryFixture(t, fill)
	ownership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       checkpointmodel.CertificationWindowsNTFSProcessRestart,
		AuthorityRef:        fixture.reservation.AuthorityRef().Bytes(),
		RootOpenDisposition: checkpointmodel.CallerProvidedContainer,
	})
	if err != nil {
		registry.Close()
		t.Fatal(err)
	}
	admission, lookup, err := registry.BeginActive(fixture.key)
	if err != nil || lookup.State() != ActiveLookupNone {
		registry.Close()
		t.Fatalf("begin ordinary fixture = %v, %v", lookup.State(), err)
	}
	if err := admission.PrepareCandidate(fixture.intent.OperationID()); err != nil {
		admission.Close()
		registry.Close()
		t.Fatal(err)
	}
	handle, outcome, err := admission.BeginReservation(fixture.claimSpec)
	if err != nil || outcome != destinationauthority.ReservationMetadataClaimCommitted {
		admission.Close()
		registry.Close()
		t.Fatalf("create ordinary fixture claim = %v, %v", outcome, err)
	}
	if outcome, err = handle.BindReservation(fixture.reservation); err != nil ||
		outcome != destinationauthority.ReservationMetadataClaimCommitted {
		handle.Close()
		admission.Close()
		registry.Close()
		t.Fatalf("bind ordinary fixture reservation = %v, %v", outcome, err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		admission.Close()
		registry.Close()
		t.Fatal(err)
	}
	lease, err := admission.Create(
		fixture.record(t, checkpointmodel.OrdinaryLeaseHeld, claim), claim,
	)
	if err != nil {
		admission.Close()
		registry.Close()
		t.Fatal(err)
	}
	binding, err := checkpointmodel.NewBinding(
		ownership, fixture.intent.OperationID(), fixture.intent.Digest(), fixture.intent.BindingDigest(),
	)
	if err != nil {
		lease.Close()
		registry.Close()
		t.Fatal(err)
	}
	repository, err := OpenOrdinaryFileRepository(lease, binding, true)
	if err != nil {
		lease.Close()
		registry.Close()
		t.Fatal(err)
	}
	return control, registry, lease, repository, ownership, operationFixture{intent: fixture.intent}
}

func checkpointRecordFixture(
	t *testing.T,
	ownership checkpointmodel.Ownership,
	fixture operationFixture,
	fill byte,
) checkpointmodel.Record {
	t.Helper()
	var fileID catalog.FileID
	var revision content.FileRevision
	for index := range fileID {
		fileID[index] = fill
		revision[index] = fill + 1
	}
	record, err := checkpointmodel.NewRecord(checkpointmodel.RecordSpec{
		OperationID:                  fixture.intent.OperationID(),
		ReceiveIntentDigest:          fixture.intent.Digest(),
		MaterializationBindingDigest: fixture.intent.BindingDigest(),
		FileID:                       fileID,
		FileRevision:                 revision,
		CanonicalPath:                "folder/file.bin",
		ExactSize:                    64,
		MaterializerKind:             ownership.MaterializerKind(),
		AuthorityRef:                 ownership.AuthorityRef().Bytes(),
		OwnedObjectID:                bytes.Repeat([]byte{fill + 2}, sha256.Size),
		StateGeneration:              1,
		CheckpointGeneration:         0,
		Phase:                        checkpointmodel.PhaseActive,
		CommitState:                  checkpointmodel.CommitCandidate,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}
