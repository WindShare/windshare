package destinationauthority

import (
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type rejectedPublicDirectoryReservation struct {
	outputcap.Directory
	cause error
}

func (directory rejectedPublicDirectoryReservation) ReservePublicDirectoryNoReplace(string) (
	outputcap.Directory, outputcap.PublishNoReplaceOutcome, error,
) {
	return nil, 0, directory.cause
}

func TestResultRootRejectionRollsBackBoundClaim(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	claimer := &reservationClaimer{}
	root := rejectedPublicDirectoryReservation{
		Directory: platform.Root(), cause: outputcap.ErrUnsafeNamespace,
	}
	reservation, err := reserveTopLevelOnRoot(
		root, authority.Binding(), reservationRequest(t, resultRootArtifact(t), claimer),
		platform.CanonicalComponentKey,
	)
	if reservation != nil || !errors.Is(err, root.cause) || errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("definite rejection: reservation=%v err=%v", reservation, err)
	}
	if len(claimer.claims) != 1 || !claimer.claims[0].rolledBack || !claimer.claims[0].closed {
		t.Fatalf("rejected creation retained its bound claim: %+v", claimer.claims)
	}
}

func TestTopLevelReservationRepairsContainerControlPrefixAndKeepsRecoveryPrefix(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()

	for _, name := range []string{".windshare-output", ".WINDSHARE-OUTPUT", ".windshare-output.probe-abc"} {
		fileID, _ := catalog.FileIDFromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
		artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, name, name)
		if err != nil {
			t.Fatalf("%q artifact: %v", name, err)
		}
		reservation, err := authority.ReserveTopLevel(reservationRequest(t, artifact, &reservationClaimer{}))
		if err != nil {
			t.Fatalf("%q reserve: %v", name, err)
		}
		physical := reservation.ReservedEntry().PhysicalName()
		_ = reservation.Close()
		if outputcap.ReservedRootName(outputcap.ReservedNamespaceDestinationContainer, physical) || !strings.Contains(physical, ".windshare-") {
			t.Fatalf("%q physical = %q, want compatible repair", name, physical)
		}
	}

	// Recovery identity markers live inside result roots, not the destination
	// container, so a top-level .wsresume* file or directory keeps its spelling.
	fileID, _ := catalog.FileIDFromBytes([]byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	single, err := receivecontract.NewSingleFileDirectoryTree(fileID, ".wsresume-notes.txt", ".wsresume-notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	res, err := authority.ReserveTopLevel(reservationRequest(t, single, &reservationClaimer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if got := res.ReservedEntry().PhysicalName(); got != ".wsresume-notes.txt" {
		t.Fatalf("top-level .wsresume file physical = %q", got)
	}
}
