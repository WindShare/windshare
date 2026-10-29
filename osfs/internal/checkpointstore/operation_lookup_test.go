package checkpointstore

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestLookupOperationDistinguishesMissingOperationFromMissingRecord(t *testing.T) {
	registry, err := OpenOperationRegistry(newMemoryDirectory())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	id := receivecontract.OperationID{1}
	lookup, err := registry.LookupOperation(id)
	if err != nil || lookup.State() != ActiveLookupNone || lookup.TakeLease() != nil {
		t.Fatalf("absent operation = %v, %v", lookup.State(), err)
	}
	names, err := registry.operations.Names(1)
	if err != nil || len(names) != 0 {
		t.Fatalf("lookup created operation state: %v, %v", names, err)
	}

	directory, err := registry.operations.CreateDirectory(operationNamespaceName(id), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	lookup, err = registry.LookupOperation(id)
	if !errors.Is(err, fs.ErrNotExist) || lookup.State() == ActiveLookupNone {
		t.Fatalf("missing record disguised as absent operation: %v, %v", lookup.State(), err)
	}

	registry.operations = &faultDirectory{
		Directory: registry.operations,
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return nil, fs.ErrPermission
		},
	}
	if _, err := registry.LookupOperation(id); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("lookup hid storage failure: %v", err)
	}
}

func TestLookupOperationExcludesTerminalDownloads(t *testing.T) {
	for _, lifecycle := range []checkpointmodel.OrdinaryOperationLifecycle{
		checkpointmodel.OrdinaryOperationCompleted,
		checkpointmodel.OrdinaryOperationDiscarded,
		checkpointmodel.OrdinaryOperationCleanupPending,
	} {
		t.Run(lifecycle.String(), func(t *testing.T) {
			_, registry, lease, repository, _, fixture := openRepositoryFixture(t, 0x71)
			defer registry.Close()
			defer lease.Close()
			defer repository.Close()
			previous := lease.Record()
			reason := checkpointmodel.OrdinaryReasonNone
			if lifecycle == checkpointmodel.OrdinaryOperationCleanupPending {
				completed, err := checkpointmodel.NextOrdinaryOperationRecord(previous, checkpointmodel.NextOrdinaryOperationRecordSpec{
					Lifecycle: checkpointmodel.OrdinaryOperationCompleted, Lease: checkpointmodel.OrdinaryLeaseHeld,
					ClosedReason: checkpointmodel.OrdinaryReasonNone,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := lease.Replace(previous, completed); err != nil {
					t.Fatal(err)
				}
				previous = completed
				reason = checkpointmodel.OrdinaryReasonCleanupUncertain
			}
			next, err := checkpointmodel.NextOrdinaryOperationRecord(previous, checkpointmodel.NextOrdinaryOperationRecordSpec{
				Lifecycle: lifecycle, Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: reason,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Replace(previous, next); err != nil {
				t.Fatal(err)
			}
			lookup, err := registry.LookupOperation(fixture.intent.OperationID())
			if err != nil || lookup.State() != ActiveLookupNone || lookup.TakeLease() != nil {
				t.Fatalf("terminal selection = %v, %v", lookup.State(), err)
			}
			record, _, err := registry.readOperation(fixture.intent.OperationID())
			if err != nil || !sameOrdinaryRecord(record, next) {
				t.Fatalf("lookup changed terminal record: %v", err)
			}
		})
	}
}
