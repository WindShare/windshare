package destinationauthority

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReservedEntryAliasesOnlyLogicalTopLevelComponent(t *testing.T) {
	var fileID catalog.FileID
	fileID[0] = 1
	artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, "file.txt", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{1}, 16))
	reservationID, _ := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{2}, 16))
	authority, _ := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{3}, 32))
	reservedName, err := receivecontract.CollisionName(operation, "file.txt", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, artifact, authority, reservedName, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := NewReservedEntry(reservation)
	if err != nil || entry.PreferredName() != "file.txt" || entry.ReservedName() != reservedName ||
		entry.CollisionIndex() != 1 || entry.EntryKind() != receivecontract.ContainerEntrySingleFile {
		t.Fatalf("reserved entry = %+v, %v", entry, err)
	}
	physical, err := PhysicalArtifactPath("file.txt", entry)
	if err != nil || physical != reservedName {
		t.Fatalf("physical path = %q, %v", physical, err)
	}
	if _, err := PhysicalArtifactPath("other.txt", entry); !errors.Is(err, ErrInvalidArtifactPath) {
		t.Fatalf("foreign logical root error = %v", err)
	}
	for _, unsafe := range []string{"/file.txt", `file.txt\\child`, "file.txt//child", "file.txt/../child"} {
		if _, err := PhysicalArtifactPath(unsafe, entry); !errors.Is(err, ErrInvalidArtifactPath) {
			t.Fatalf("unsafe logical path %q error = %v", unsafe, err)
		}
	}
}
