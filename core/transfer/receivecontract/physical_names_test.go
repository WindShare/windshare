package receivecontract

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
)

func TestNativePhysicalNameSurvivesCanonicalPlanRoundTrip(t *testing.T) {
	operation := mustIdentity(OperationIDFromBytes(contractStable(20)))
	reservationID := mustIdentity(DestinationReservationIDFromBytes(contractStable(22)))
	authority := mustIdentity(AuthorityRefFromBytes(contractOpaque(23)))
	artifact, err := NewSingleFileDirectoryTree(contractID[catalog.FileID](27), "report?.pdf", "report?.pdf")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := NewNativeNamedEntryReservation(operation, reservationID, artifact, authority, "report?.pdf", "report-compatible.pdf", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewDirectTreePlan(artifact, reservation)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeMaterializationPlan(plan.CanonicalBytes(), artifact)
	if err != nil || !bytes.Equal(decoded.CanonicalBytes(), plan.CanonicalBytes()) {
		t.Fatalf("decode: %v", err)
	}
	restored, ok := decoded.DestinationReservation()
	if !ok || restored.RequestedName() != "report?.pdf" || restored.LogicalReservedName() != "report?.pdf" || restored.PhysicalName() != "report-compatible.pdf" {
		t.Fatalf("restored reservation = %+v", restored)
	}
	if _, err := NewNativeNamedEntryReservation(operation, reservationID, artifact, authority, "report?.pdf", "../escape.pdf", 0); !errors.Is(err, ErrInvalidReceiveContract) {
		t.Fatalf("physical path escape accepted: %v", err)
	}
}
