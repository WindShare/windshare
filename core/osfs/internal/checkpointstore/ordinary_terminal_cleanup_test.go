package checkpointstore

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOrdinaryTerminalCleanupRemovesOnlyAuthenticatedEmptyPrivateState(t *testing.T) {
	_, registry, lease, repository, _, _ := openRepositoryFixture(t, 0xe1)
	t.Cleanup(func() {
		_ = repository.Close()
		_ = lease.Close()
		_ = registry.Close()
	})
	operationID := lease.Record().OperationID()
	proof, err := registry.RecoveryProof(lease.Record())
	if err != nil || !proof.Valid() {
		t.Fatalf("recovery proof = (%+v, %v)", proof, err)
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	previous := lease.Record()
	completed, err := checkpointmodel.NextOrdinaryOperationRecord(
		previous,
		checkpointmodel.NextOrdinaryOperationRecordSpec{
			Lifecycle:    checkpointmodel.OrdinaryOperationCompleted,
			Lease:        checkpointmodel.OrdinaryLeaseHeld,
			ClosedReason: checkpointmodel.OrdinaryReasonNone,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Replace(previous, completed); err != nil {
		t.Fatal(err)
	}
	if err := lease.CleanupEmptyFileState(); err != nil {
		t.Fatal(err)
	}
	if err := lease.CleanupEmptyFileState(); err != nil {
		t.Fatalf("restartable empty cleanup = %v", err)
	}
	if err := lease.DeleteTerminal(); err != nil {
		t.Fatal(err)
	}
	if !lease.Deleted() || lease.Record().Valid() {
		t.Fatal("terminal deletion retained row mutation authority")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.AcquireOperationLease(operationID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted operation reacquired = %v", err)
	}
	page, err := registry.PageOperations(OperationPageCursor{}, 4)
	if err != nil || len(page.Records()) != 0 || page.Unknown() {
		t.Fatalf("post-cleanup page = (%+v, unknown %t, %v)", page.Records(), page.Unknown(), err)
	}
}

func TestOrdinaryOperationCursorAndTerminalGuardsAreExplicit(t *testing.T) {
	fixture := ordinaryRegistryFixture(t, 0xf1)
	cursor := NewOperationPageCursor(fixture.intent.OperationID())
	after, ok := cursor.After()
	if cursor.IsZero() || !ok || after != fixture.intent.OperationID() {
		t.Fatalf("operation cursor = (zero %t, after %x, ok %t)", cursor.IsZero(), after.Bytes(), ok)
	}
	if zero := NewOperationPageCursor(receivecontract.OperationID{}); !zero.IsZero() {
		t.Fatal("zero operation produced a nonzero cursor")
	}
	if after, ok := (OperationPageCursor{}).After(); ok || !after.IsZero() {
		t.Fatalf("zero cursor after = (%x, %t)", after.Bytes(), ok)
	}
	var nilLease *OperationRegistryLease
	if err := nilLease.CleanupEmptyFileState(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil cleanup = %v", err)
	}
	if err := nilLease.DeleteTerminal(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil terminal delete = %v", err)
	}
	var nilRegistry *OperationRegistry
	if _, err := nilRegistry.RecoveryProof(checkpointmodel.OrdinaryOperationRecord{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil recovery proof = %v", err)
	}
}
