package destinationauthority

import (
	"errors"
	"testing"

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
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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
