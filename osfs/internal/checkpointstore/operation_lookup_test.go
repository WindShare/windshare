package checkpointstore

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/transfer"
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
	selection := ordinaryRegistryFixture(t, 0x70).intent.SelectionSpec()
	if _, err := registry.LookupOperation(id, transfer.SelectionSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero selection lookup = %v, want invalid output binding", err)
	}
	lookup, err := registry.LookupOperation(id, selection)
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
	lookup, err = registry.LookupOperation(id, selection)
	if !errors.Is(err, fs.ErrNotExist) || lookup.State() == ActiveLookupNone {
		t.Fatalf("missing record disguised as absent operation: %v, %v", lookup.State(), err)
	}

	registry.operations = &faultDirectory{
		Directory: registry.operations,
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return nil, fs.ErrPermission
		},
	}
	if _, err := registry.LookupOperation(id, selection); !errors.Is(err, fs.ErrPermission) {
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
			lookup, err := registry.LookupOperation(fixture.intent.OperationID(), fixture.intent.SelectionSpec())
			if err != nil || lookup.State() != ActiveLookupNone || lookup.TakeLease() != nil {
				t.Fatalf("terminal selection = %v, %v", lookup.State(), err)
			}
			record, _, _, err := registry.readOperation(fixture.intent.OperationID())
			if err != nil || !sameOrdinaryRecord(record, next) {
				t.Fatalf("lookup changed terminal record: %v", err)
			}
		})
	}
}

func TestLookupOperationReportsSelectionMismatchWithoutLeaseOrMutation(t *testing.T) {
	_, registry, lease, repository, _, fixture := openRepositoryFixture(t, 0x72)
	defer registry.Close()
	defer repository.Close()
	id := fixture.intent.OperationID()
	otherSelection := ordinaryRegistryFixture(t, 0x73).intent.SelectionSpec()

	// While running (lease held), a non-matching selection reports mismatch
	// rather than AlreadyRunning.
	heldRecord, _, _, err := registry.readOperation(id)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := registry.LookupOperation(id, otherSelection)
	if err != nil || lookup.State() != ActiveLookupSelectionMismatch ||
		lookup.Record().OperationID() != id || lookup.Intent().Digest() != fixture.intent.Digest() ||
		lookup.TakeLease() != nil {
		t.Fatalf("non-matching running lookup = %v, %v", lookup.State(), err)
	}
	afterHeld, _, _, err := registry.readOperation(id)
	if err != nil || !sameOrdinaryRecord(afterHeld, heldRecord) {
		t.Fatalf("non-matching running lookup changed record: %v", err)
	}

	// Once idle, a non-matching selection must not acquire a lease or advance
	// lifecycle generation on disk.
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	idleRecord, _, _, err := registry.readOperation(id)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err = registry.LookupOperation(id, otherSelection)
	if err != nil || lookup.State() != ActiveLookupSelectionMismatch ||
		lookup.Record().OperationID() != id || lookup.Intent().Digest() != fixture.intent.Digest() ||
		lookup.TakeLease() != nil {
		t.Fatalf("non-matching idle lookup = %v, %v", lookup.State(), err)
	}
	afterIdle, _, _, err := registry.readOperation(id)
	if err != nil || !sameOrdinaryRecord(afterIdle, idleRecord) {
		t.Fatalf("non-matching idle lookup mutated record: before gen=%d lease=%v, after gen=%d lease=%v, err=%v",
			idleRecord.LifecycleGeneration(), idleRecord.Lease(), afterIdle.LifecycleGeneration(), afterIdle.Lease(), err)
	}

	// Corrupt intent bytes still fail as corrupt records rather than mismatch.
	broken := operationPageRecord(t, 0x74)
	foreign := operationPageRecord(t, 0x75)
	corruptIntent := bytes.Replace(encodeOperationPageRecord(t, foreign), foreign.OperationID().Bytes(), broken.OperationID().Bytes(), 1)
	installOperationPageRecord(t, &registry, broken.OperationID(), corruptIntent)
	if _, err := registry.LookupOperation(broken.OperationID(), otherSelection); ErrorCodeFor(err) != ErrorCorruptRecord {
		t.Fatalf("corrupt intent lookup = %v, want corrupt record", err)
	}
}

func TestReadOperationIsolatesUnrelatedCorruptionAndFailsClosedOnTarget(t *testing.T) {
	control := newMemoryDirectory()
	absentID := receivecontract.OperationID{0x76}
	observed, err := ReadOperation(control, absentID)
	if err != nil || observed.State() != ObservedOperationAbsent {
		t.Fatalf("fresh control read = %v, %v", observed.State(), err)
	}

	registry, err := OpenOperationRegistry(control)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	healthy := operationPageRecord(t, 0x77)
	corrupt := operationPageRecord(t, 0x78)
	missingRecordID := receivecontract.OperationID{0x79}
	installOperationPageRecord(t, &registry, healthy.OperationID(), encodeOperationPageRecord(t, healthy))
	installOperationPageRecord(t, &registry, corrupt.OperationID(), []byte("corrupt record"))
	emptyDir, err := registry.operations.CreateDirectory(operationNamespaceName(missingRecordID), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := emptyDir.Close(); err != nil {
		t.Fatal(err)
	}

	// Healthy target succeeds despite an unrelated corrupt sibling record.
	observed, err = ReadOperation(control, healthy.OperationID())
	if err != nil || observed.State() != ObservedOperationPresent ||
		observed.Record().OperationID() != healthy.OperationID() ||
		observed.Intent().OperationID() != healthy.OperationID() {
		t.Fatalf("healthy target with corrupt sibling = %v, %v", observed.State(), err)
	}
	// Unknown ID remains absent rather than inheriting sibling corruption.
	observed, err = ReadOperation(control, absentID)
	if err != nil || observed.State() != ObservedOperationAbsent {
		t.Fatalf("absent target with corrupt sibling = %v, %v", observed.State(), err)
	}
	// Corrupt target and missing record inside an existing directory fail closed.
	observed, err = ReadOperation(control, corrupt.OperationID())
	if err != nil || observed.State() != ObservedOperationUnknown {
		t.Fatalf("corrupt target = %v, %v", observed.State(), err)
	}
	observed, err = ReadOperation(control, missingRecordID)
	if err != nil || observed.State() != ObservedOperationUnknown {
		t.Fatalf("missing record target = %v, %v", observed.State(), err)
	}
	// Genuine I/O errors propagate instead of turning into unknown or absent.
	faultControl := &faultDirectory{
		Directory: control,
		openDirectory: func(string, bool) (outputcap.Directory, error) {
			return nil, fs.ErrPermission
		},
	}
	if _, err := ReadOperation(faultControl, healthy.OperationID()); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("storage failure hidden by ReadOperation: %v", err)
	}
}
