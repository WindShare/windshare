package namestore

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/osfs/internal/destinationauthority"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputnames"
	"github.com/windshare/windshare/osfs/internal/outputportable"
)

var operationID = receivecontract.OperationID{1}

func TestMappingLedgerSurvivesReopenAndRejectsForeignOrChangedRecords(t *testing.T) {
	lease, _ := mappingLease(t)
	store := New(lease, operationID.Bytes(), true)
	component, err := outputnames.Candidate(operationID.Bytes(), "docs/report?.pdf", 0)
	if err != nil {
		t.Fatal(err)
	}
	record := outputnames.Mapping{LogicalPath: "docs/report?.pdf", PhysicalComponent: component}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.PhysicalComponent = "another.pdf"
	if err := store.Put(changed); !errors.Is(err, outputnames.ErrInvalidMapping) {
		t.Fatalf("changed choice = %v", err)
	}
	reopened := New(lease, operationID.Bytes(), false)
	if records, err := reopened.Load(); err != nil || !reflect.DeepEqual(records, []outputnames.Mapping{record}) {
		t.Fatalf("loaded = %+v, %v", records, err)
	}
	otherID := receivecontract.OperationID{2}
	if _, err := New(lease, otherID.Bytes(), false).Load(); !errors.Is(err, outputnames.ErrInvalidMapping) {
		t.Fatalf("foreign operation = %v", err)
	}
	if err := reopened.Clear(); err != nil {
		t.Fatal(err)
	}
	if records, err := New(lease, operationID.Bytes(), false).Load(); err != nil || len(records) != 0 {
		t.Fatalf("cleared = %+v, %v", records, err)
	}
	if err := reopened.Clear(); err != nil {
		t.Fatal(err)
	}
}

func TestNameReportsAreIdempotentAndPreserveForeignFiles(t *testing.T) {
	root := t.TempDir()
	platform, err := outputportable.Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	store := New(nil, operationID.Bytes(), true)
	shape := outputnames.Root{RequestedName: "docs", ReservedName: "docs", PhysicalName: "docs", Kind: receivecontract.ContainerEntryResultRoot}
	component, _ := outputnames.Candidate(operationID.Bytes(), "docs/report?.pdf", 0)
	record := outputnames.Mapping{LogicalPath: "docs/report?.pdf", PhysicalComponent: component}
	if err := store.Put(record); err != nil {
		t.Fatal(err)
	}
	records, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := outputnames.NewResolver(operationID.Bytes(), shape, records)
	if err != nil {
		t.Fatal(err)
	}
	name := "windshare-names-" + store.operationID + "-1-0.json"
	foreign := filepath.Join(root, name)
	if err := os.WriteFile(foreign, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	visit := func(callback func(outputcap.Directory) error) error { return callback(platform.Root()) }
	for range 2 {
		if err := store.Export(resolver, shape, visit); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(root, "windshare-names-*.json"))
	if err != nil || len(files) != 2 {
		t.Fatalf("reports = %v, %v", files, err)
	}
	if encoded, err := os.ReadFile(foreign); err != nil || string(encoded) != "foreign" {
		t.Fatal("foreign report changed")
	}
	if err := store.Clear(); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryCollisionDoesNotExportANameReport(t *testing.T) {
	store := New(nil, operationID.Bytes(), true)
	shape := outputnames.Root{RequestedName: "docs", ReservedName: "docs-2", PhysicalName: "docs-2", Kind: receivecontract.ContainerEntryResultRoot}
	resolver, err := outputnames.NewResolver(operationID.Bytes(), shape, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Export(resolver, shape, func(func(outputcap.Directory) error) error {
		t.Fatal("ordinary collision opened the destination for a report")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func mappingLease(t *testing.T) (*checkpointstore.OperationRegistryLease, outputcap.Directory) {
	t.Helper()
	platform, err := outputportable.Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = platform.Close() })
	registry, err := checkpointstore.OpenOperationRegistry(platform.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(catalog.ShareInstance{2}, catalog.DirectoryID{3}, rules)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewSingleFileDirectoryTree(catalog.FileID{4}, "file.txt", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(operationID, receivecontract.DestinationReservationID{5}, artifact, receivecontract.AuthorityRef{6}, "file.txt", "file.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(selection, artifact, plan)
	if err != nil {
		t.Fatal(err)
	}
	key, err := checkpointmodel.NewActiveOperationKeyV1(selection.Digest(), reservation.AuthorityRef())
	if err != nil {
		t.Fatal(err)
	}
	admission, _, err := registry.BeginActive(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admission.Close() })
	if err := admission.PrepareCandidate(operationID); err != nil {
		t.Fatal(err)
	}
	handle, _, err := admission.BeginReservation(destinationauthority.ReservationClaimSpec{
		CanonicalNameKey: "file.txt", OperationID: operationID, ReservationID: reservation.ID(),
		EntryKind: reservation.EntryKind(), RequestedName: "file.txt", LogicalReservedName: "file.txt", PhysicalName: "file.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.BindReservation(reservation); err != nil {
		t.Fatal(err)
	}
	claim := handle.Claim()
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	locator, err := checkpointmodel.NewReservationClaimLocator([32]byte(claim.Token), claim.Generation+1)
	if err != nil {
		t.Fatal(err)
	}
	record, err := checkpointmodel.NewOrdinaryOperationRecord(checkpointmodel.OrdinaryOperationRecordSpec{
		ActiveKey: key, Intent: intent, LifecycleGeneration: 1, ReservationClaim: locator,
		Lifecycle: checkpointmodel.OrdinaryOperationActive, Lease: checkpointmodel.OrdinaryLeaseHeld, ClosedReason: checkpointmodel.OrdinaryReasonNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := admission.Create(record, claim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return lease, platform.Root()
}
