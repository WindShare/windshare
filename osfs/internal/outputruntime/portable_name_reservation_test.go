package outputruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputportable"
)

func TestPortableReservationRenamesExistingNativeAlias(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Report.txt"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "report.txt")); os.IsNotExist(err) {
		t.Skip("destination has case-sensitive lookup")
	} else if err != nil {
		t.Fatal(err)
	}
	authority, err := New(Config{RootPath: root, PlatformFactory: outputportable.Open})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if _, err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0x31)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x34), "report.txt", "report.txt",
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatalf("native alias must select a collision name: %v", err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("missing receive intent")
	}
	reservation, ok := intent.MaterializationPlan().DestinationReservation()
	if !ok || reservation.CollisionIndex() != 1 || reservation.PhysicalName() == "report.txt" {
		t.Fatalf("unexpected reservation: %+v", reservation)
	}
	if actual, err := os.ReadFile(filepath.Join(root, "Report.txt")); err != nil || string(actual) != "foreign" {
		t.Fatalf("foreign file changed: %q, %v", actual, err)
	}
}
