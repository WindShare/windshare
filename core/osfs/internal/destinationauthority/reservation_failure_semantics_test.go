package destinationauthority

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type lifecycleClaimHandle struct {
	claim           ReservationClaim
	claimAfterBind  ReservationClaim
	clearAfterBind  bool
	bindOutcome     ReservationMetadataClaimOutcome
	bindErr         error
	identityOutcome ReservationMetadataClaimOutcome
	identityErr     error
	rollbackOutcome ReservationMetadataClaimOutcome
	rollbackErr     error
	closeErr        error
}

func (handle *lifecycleClaimHandle) Claim() ReservationClaim { return handle.claim }
func (handle *lifecycleClaimHandle) BindReservation(receivecontract.DestinationReservation) (ReservationMetadataClaimOutcome, error) {
	if handle.clearAfterBind {
		handle.claim = ReservationClaim{}
	} else if handle.claimAfterBind != (ReservationClaim{}) {
		handle.claim = handle.claimAfterBind
	}
	return handle.bindOutcome, handle.bindErr
}
func (handle *lifecycleClaimHandle) BindDirectoryIdentity([]byte) (ReservationMetadataClaimOutcome, error) {
	return handle.identityOutcome, handle.identityErr
}
func (handle *lifecycleClaimHandle) Rollback() (ReservationMetadataClaimOutcome, error) {
	return handle.rollbackOutcome, handle.rollbackErr
}
func (handle *lifecycleClaimHandle) Close() error { return handle.closeErr }

type lifecycleClaimer struct {
	handle  ReservationClaimHandle
	outcome ReservationMetadataClaimOutcome
	err     error
}

func (claimer lifecycleClaimer) BeginReservation(ReservationClaimSpec) (ReservationClaimHandle, ReservationMetadataClaimOutcome, error) {
	return claimer.handle, claimer.outcome, claimer.err
}

type reservationDirectoryView struct{ outputcap.Directory }

type reservationIdentityDirectory struct {
	outputcap.Directory
	claim   []byte
	prepare error
	sync    error
}

func (directory reservationIdentityDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return append([]byte(nil), directory.claim...), directory.prepare
}
func (directory reservationIdentityDirectory) Sync() error { return directory.sync }

type reservationGuardPlatform struct {
	outputcap.Platform
	guard outputcap.PublicOperationGuard
	err   error
}

func (platform reservationGuardPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	return platform.guard, platform.err
}

func validLifecycleClaim(fill byte, generation uint64) ReservationClaim {
	var token ReservationClaimToken
	token[0] = fill
	return ReservationClaim{Token: token, Generation: generation}
}

func TestReservationValidationRejectsEveryUntrustedCoordinate(t *testing.T) {
	request := reservationRequest(t, singleFileArtifact(t), &reservationClaimer{})
	spec := ReservationClaimSpec{
		CanonicalNameKey: "file.txt", OperationID: request.OperationID,
		ReservationID: request.ReservationID, EntryKind: receivecontract.ContainerEntryKind(255),
		RequestedName: "file.txt", ReservedName: "file.txt",
	}
	if spec.Valid() {
		t.Fatal("unknown entry kind accepted")
	}
	if _, err := NewReservedEntry(receivecontract.DestinationReservation{}); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("zero canonical reservation = %v", err)
	}
	if (ReservationRequest{}).valid() {
		t.Fatal("zero reservation request accepted")
	}
	if (ExpectedReservation{}).valid() {
		t.Fatal("zero recovery proof accepted")
	}
	expected := ExpectedReservation{Reservation: requestReservation(t, request)}
	if expected.valid() {
		t.Fatal("recovery proof without claim accepted")
	}
	if _, _, ok := artifactReservationShape(receivecontract.ArtifactSpec{}); ok {
		t.Fatal("zero artifact exposed a reservation shape")
	}
	if _, _, ok := artifactReservationShape(receivecontract.NewCatalogRootDirectoryTree()); ok {
		t.Fatal("unbounded catalog root exposed a named reservation")
	}
	if err := rollbackReservationClaim(nil); err != nil || closeReservationClaimHandle(nil) != nil {
		t.Fatal("nil claim cleanup was not inert")
	}
	failing := &lifecycleClaimHandle{
		rollbackOutcome: ReservationMetadataClaimIndeterminate,
		rollbackErr:     errDestinationFake,
		closeErr:        errDestinationFake,
	}
	if err := rollbackReservationClaim(failing); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("indeterminate rollback = %v", err)
	}
}

func TestReservationReducerRejectsContradictoryClaimCuts(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	defer authority.Close()
	root := platform.Root()
	binding := authority.Binding()
	base := reservationRequest(t, singleFileArtifact(t), &reservationClaimer{})

	tests := []struct {
		name    string
		claimer ReservationMetadataClaimer
	}{
		{
			name: "committed-without-handle",
			claimer: lifecycleClaimer{
				outcome: ReservationMetadataClaimCommitted,
			},
		},
		{
			name: "bind-indeterminate",
			claimer: lifecycleClaimer{
				handle: &lifecycleClaimHandle{
					claim: validLifecycleClaim(1, 1), bindOutcome: ReservationMetadataClaimIndeterminate,
					bindErr: errDestinationFake,
				},
				outcome: ReservationMetadataClaimCommitted,
			},
		},
		{
			name: "claim-lost-after-bind",
			claimer: lifecycleClaimer{
				handle: &lifecycleClaimHandle{
					claim: validLifecycleClaim(2, 1), bindOutcome: ReservationMetadataClaimCommitted,
					clearAfterBind: true,
				},
				outcome: ReservationMetadataClaimCommitted,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.Metadata = test.claimer
			if _, err := reserveTopLevelOnRoot(
				root, binding, request, platform.CanonicalComponentKey,
			); !errors.Is(err, ErrReservationIndeterminate) {
				t.Fatalf("contradictory claim cut = %v", err)
			}
		})
	}

	platform.root.entries["file.txt"] = &destinationNode{id: 99, file: &destinationFile{}}
	request := base
	request.Metadata = lifecycleClaimer{
		handle: &lifecycleClaimHandle{
			claim:           validLifecycleClaim(3, 1),
			bindOutcome:     ReservationMetadataClaimCommitted,
			rollbackOutcome: ReservationMetadataClaimIndeterminate,
			rollbackErr:     errDestinationFake,
		},
		outcome: ReservationMetadataClaimCommitted,
	}
	if _, err := reserveTopLevelOnRoot(
		root, binding, request, platform.CanonicalComponentKey,
	); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("occupied name with failed rollback = %v", err)
	}

	request = base
	request.Metadata = &reservationClaimer{}
	if _, err := reserveTopLevelOnRoot(root, Binding{}, request, platform.CanonicalComponentKey); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("invalid binding = %v", err)
	}
	if _, err := reserveTopLevelOnRoot(root, binding, request, nil); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("nil canonicalizer = %v", err)
	}
	if _, err := reserveTopLevelOnRoot(
		root, binding, request, func(string) (string, error) { return "", errDestinationFake },
	); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("failed canonicalizer = %v", err)
	}
}

func TestTopLevelExecutionGuardRevalidatesContainerAndResultIdentity(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	single, err := authority.ReserveTopLevel(
		reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := authority.ReserveTopLevel(
		reservationRequest(t, resultRootArtifact(t), &reservationClaimer{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	defer result.Close()
	defer authority.Close()

	originalPlatform := authority.platform
	authority.platform = reservationGuardPlatform{Platform: originalPlatform}
	if _, err := single.AcquirePublicOperationGuard(); !errors.Is(err, ErrRetainedRootChanged) {
		t.Fatalf("nil public guard = %v", err)
	}
	authority.platform = originalPlatform

	foreign := &destinationNode{id: 200, identity: []byte("foreign"), entries: map[string]*destinationNode{}}
	platform.guardRoot = foreign
	if _, err := single.AcquirePublicOperationGuard(); !errors.Is(err, ErrRetainedRootChanged) {
		t.Fatalf("foreign container guard = %v", err)
	}
	platform.guardRoot = platform.root

	missingDirectory := &TopLevelReservation{
		entry: result.entry, canonical: result.canonical,
		metadataClaim: result.metadataClaim, authority: authority,
	}
	if _, err := missingDirectory.AcquirePublicOperationGuard(); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("missing retained result root = %v", err)
	}
	if _, err := (&TopLevelReservation{}).CanonicalComponentKey("name"); !errors.Is(err, ErrInvalidReservation) {
		t.Fatalf("invalid component authority = %v", err)
	}
}

func TestPersistentIdentityAndExactOpenRejectWeakNativeEvidence(t *testing.T) {
	platform := newDestinationPlatform()
	base := platform.Root()
	withoutEnrollment := reservationDirectoryView{Directory: base}
	if _, err := preparePersistentIdentityClaim(withoutEnrollment); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("missing identity enrollment = %v", err)
	}
	if _, err := readPersistentIdentityClaim(withoutEnrollment); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("missing identity recovery = %v", err)
	}
	invalid := reservationIdentityDirectory{Directory: base}
	if _, err := preparePersistentIdentityClaim(invalid); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("empty prepared identity = %v", err)
	}
	if _, err := readPersistentIdentityClaim(invalid); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("empty recovered identity = %v", err)
	}
	syncFailure := reservationIdentityDirectory{
		Directory: base, claim: []byte("identity"), sync: errDestinationFake,
	}
	if _, err := preparePersistentIdentityClaim(syncFailure); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("identity sync cut = %v", err)
	}
	if _, err := openExactPublicDirectory(base, "missing"); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("missing exact result root = %v", err)
	}
	platform.root.entries["file"] = &destinationNode{id: 4, file: &destinationFile{}}
	if _, err := openExactPublicDirectory(base, "file"); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("file substituted for result root = %v", err)
	}
	if !validPersistentIdentityClaim(bytes.Repeat([]byte{1}, outputcap.MaxRootIdentityClaimBytes)) ||
		validPersistentIdentityClaim(bytes.Repeat([]byte{1}, outputcap.MaxRootIdentityClaimBytes+1)) {
		t.Fatal("persistent identity bounds changed")
	}
}

func requestReservation(t *testing.T, request ReservationRequest) receivecontract.DestinationReservation {
	t.Helper()
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	defer authority.Close()
	reserved, err := authority.ReserveTopLevel(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Close()
	return reserved.CanonicalReservation()
}
