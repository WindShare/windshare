package checkpointmodel

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReservationPhysicalNameIsAnIndependentCanonicalComponent(t *testing.T) {
	spec := ReservationClaimRecordSpec{
		CanonicalNameKey: "report-compatible.pdf",
		OperationID:      receivecontract.OperationID{1}, ReservationID: receivecontract.DestinationReservationID{2},
		RequestedName: "report?.pdf", LogicalReservedName: "report?.pdf", PhysicalName: "report-compatible.pdf",
		EntryKind: receivecontract.ContainerEntrySingleFile, Generation: 1, Phase: ReservationClaimed,
	}
	record, err := NewReservationClaimRecord(spec)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReservationClaimRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeReservationClaimRecord(encoded)
	if err != nil || decoded.PhysicalName() != spec.PhysicalName || decoded.LogicalReservedName() != spec.LogicalReservedName {
		t.Fatalf("decode = %+v, %v", decoded, err)
	}
	for _, physical := range []string{"../escape.pdf", "folder/file.pdf", ".", "report:stream.pdf"} {
		spec.PhysicalName = physical
		if _, err := NewReservationClaimRecord(spec); !errors.Is(err, ErrInvalidReservationClaim) {
			t.Fatalf("%q accepted: %v", physical, err)
		}
	}
}
