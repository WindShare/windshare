package destinationauthority

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func TestBoundDestinationProjectsCheckpointAuthorityOnlyWhileOpen(t *testing.T) {
	var nilAuthority *BoundDestination
	if nilAuthority.Binding().Valid() || nilAuthority.LiveCleanupProfile() != 0 {
		t.Fatal("nil authority exposed retained state")
	}
	if _, err := nilAuthority.FileCheckpointOwnership(outputcap.CallerProvidedContainer); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil checkpoint ownership error = %v", err)
	}
	if _, err := (Binding{}).ExecutionMode(); !errors.Is(err, ErrInvalidBinding) {
		t.Fatalf("zero binding mode error = %v", err)
	}

	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	binding := authority.Binding()
	if authority.LiveCleanupProfile() != checkpointmodel.LiveCleanupWindowsNTFSV1 {
		t.Fatalf("cleanup profile = %d", authority.LiveCleanupProfile())
	}
	for _, disposition := range []outputcap.RootOpenDisposition{
		outputcap.CallerProvidedContainer,
		outputcap.AuthorityCreatedRoot,
	} {
		ownership, err := authority.FileCheckpointOwnership(disposition)
		if err != nil || !ownership.Valid() ||
			ownership.AuthorityRef() != binding.AuthorityRef() ||
			string(ownership.RootOpenDisposition()) != string(disposition) {
			t.Fatalf("ownership for %q = (%+v, %v)", disposition, ownership, err)
		}
	}
	if _, err := authority.FileCheckpointOwnership(outputcap.RootOpenDisposition("unknown")); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("unknown disposition error = %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if authority.LiveCleanupProfile() != 0 || authority.Binding().Valid() {
		t.Fatal("closed authority exposed retained state")
	}
	if _, err := authority.FileCheckpointOwnership(outputcap.CallerProvidedContainer); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed checkpoint ownership error = %v", err)
	}
	if err := authority.OpenResumableState(func(outputcap.Directory) (io.Closer, error) {
		return nil, nil
	}); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed resumable state error = %v", err)
	}
}

func TestReservationValuesAndClosedExecutionFailClosed(t *testing.T) {
	for outcome := ReservationMetadataClaimCommitted; outcome <= ReservationMetadataClaimIndeterminate; outcome++ {
		if !outcome.valid() {
			t.Fatalf("valid claim outcome %d rejected", outcome)
		}
	}
	if ReservationMetadataClaimOutcome(0).valid() || ReservationMetadataClaimOutcome(4).valid() {
		t.Fatal("unknown claim outcome accepted")
	}
	var token ReservationClaimToken
	if !token.IsZero() || (ReservationClaim{Token: token, Generation: 1}).Valid() {
		t.Fatal("zero claim token became authoritative")
	}
	token[0] = 1
	if token.IsZero() || !(ReservationClaim{Token: token, Generation: 1}).Valid() ||
		(ReservationClaim{Token: token}).Valid() {
		t.Fatal("claim validity changed")
	}

	var nilReservation *TopLevelReservation
	if nilReservation.ReservedEntry().Valid() ||
		!nilReservation.CanonicalReservation().IsZero() ||
		nilReservation.PersistentIdentityClaim() != nil ||
		nilReservation.MetadataClaim().Valid() ||
		nilReservation.RootOpenDisposition() != "" ||
		nilReservation.BorrowResultRoot(func(outputcap.Directory) error { return nil }) == nil {
		t.Fatal("nil reservation exposed authority")
	}
	if _, err := nilReservation.AcquirePublicOperationGuard(); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("nil reservation guard error = %v", err)
	}
	if err := nilReservation.Close(); err != nil {
		t.Fatal(err)
	}
	var nilGuard *reservationExecutionGuard
	if nilGuard.Root() != nil || nilGuard.Close() != nil {
		t.Fatal("nil execution guard was not inert")
	}

	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	reserved, err := authority.ReserveTopLevel(
		reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	claim := reserved.MetadataClaim()
	expected := ExpectedReservation{
		Reservation:   reserved.CanonicalReservation(),
		MetadataClaim: claim,
	}
	if !expected.valid() {
		t.Fatal("exact single-file recovery proof rejected")
	}
	expected.PersistentIdentityClaim = []byte("unexpected")
	if expected.valid() {
		t.Fatal("single-file recovery accepted a directory identity")
	}
	expected.PersistentIdentityClaim = nil
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reserved.AcquirePublicOperationGuard(); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed reservation guard error = %v", err)
	}
	if _, err := reserved.CanonicalLocatorKey("file.txt"); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed locator error = %v", err)
	}
	if err := reserved.ValidateModifiedTime(catalogModifiedZero()); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed metadata validation error = %v", err)
	}
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCleanupPublicationGuardsPreserveRecordedEvidence(t *testing.T) {
	var nilAuthority *BoundDestination
	source := &destinationFile{}
	if _, err := nilAuthority.PublishFileNoReplace(source, "file.bin"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil publish error = %v", err)
	}
	if _, err := nilAuthority.PublishFileNoReplace(nil, "file.bin"); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("nil source error = %v", err)
	}
	if _, _, err := nilAuthority.CreateLiveCleanupStage(
		context.Background(), nil, checkpointmodel.LiveCleanupTicket{},
	); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("zero cleanup ticket error = %v", err)
	}
	if err := nilAuthority.RemoveLiveCleanupStage(checkpointmodel.LiveCleanupTicket{}, source); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("zero cleanup removal error = %v", err)
	}

	platform := newDestinationPlatform()
	journal := &destinationJournal{}
	authority := bindFakeDestination(t, platform, journal)
	parent := destinationRootLiveStageParent(platform)
	committed := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 7)
	if _, _, err := authority.CreateLiveCleanupStage(
		context.Background(), parent,
		cleanupTicket(t, checkpointmodel.LiveCleanupStageCreated, 8),
	); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("already-created ticket error = %v", err)
	}
	if err := authority.RemoveLiveCleanupStage(committed, source); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("uncreated cleanup removal error = %v", err)
	}
	nonce := make([]byte, checkpointmodel.LiveCleanupNonceBytesV1)
	nonce[0] = 1
	mismatched, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce:      nonce,
		ExactSize:  1,
		Profile:    checkpointmodel.LiveCleanupLinuxExt4V1,
		Generation: 1,
		State:      checkpointmodel.LiveCleanupTicketCommitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authority.CreateLiveCleanupStage(
		context.Background(), parent, mismatched,
	); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("foreign cleanup profile error = %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authority.CreateLiveCleanupStage(
		context.Background(), parent, committed,
	); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed cleanup creation error = %v", err)
	}
}

func catalogModifiedZero() catalog.ModifiedTime {
	return catalog.ModifiedTime{}
}
