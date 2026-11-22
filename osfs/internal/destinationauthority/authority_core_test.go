package destinationauthority

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestBindDestinationRetainsAuthorityAndOwnsCloseOrder(t *testing.T) {
	order := []string{}
	platform := newDestinationPlatform()
	platform.closeOrder = &order
	authority := bindFakeDestination(t, platform)

	if !authority.Binding().Valid() || platform.capCalls != 1 || platform.guardCalls != 1 {
		t.Fatalf("binding=%+v capabilities=%d guards=%d", authority.Binding(), platform.capCalls, platform.guardCalls)
	}
	if err := authority.Binding().Capabilities().ValidateSupport(); err != nil {
		t.Fatalf("capabilities support = %v", err)
	}
	if err := authority.OpenResumableState(func(control outputcap.Directory) (io.Closer, error) {
		if control == nil {
			t.Fatal("opener received no authenticated control authority")
		}
		return orderedCloser{order: &order}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.OpenResumableState(func(outputcap.Directory) (io.Closer, error) {
		return orderedCloser{order: &order}, nil
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("second resumable opener error=%v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(order, ","), "resumable,platform"; got != want {
		t.Fatalf("close order=%q want=%q", got, want)
	}
	if authority.Binding().Valid() || !platform.closed {
		t.Fatalf("closed binding=%+v platform=%t", authority.Binding(), platform.closed)
	}
	if err := authority.Close(); err != nil || len(order) != 2 {
		t.Fatalf("idempotent close err=%v order=%v", err, order)
	}
}

func TestBindDestinationReadsExistingIdentityAndRejectsInvalidConfiguration(t *testing.T) {
	platform := newDestinationPlatform()
	first := bindFakeDestination(t, platform)
	firstID := first.Binding().ID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := bindFakeDestination(t, platform)
	defer second.Close()
	if second.Binding().ID() != firstID || platform.capCalls != 2 {
		t.Fatalf("reopened id=%v want=%v capability calls=%d", second.Binding().ID(), firstID, platform.capCalls)
	}

	if _, err := BindDestination(BindConfig{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("empty bind error=%v", err)
	}
	if _, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: ".",
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("relative display path error=%v", err)
	}
	withoutSources := struct{ outputcap.Platform }{Platform: platform}
	if _, err := BindDestination(BindConfig{
		Platform: withoutSources, DisplayPath: filepath.Clean(t.TempDir()),
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("missing capability source error=%v", err)
	}
}

func TestBindDestinationRejectsUnsupportedCapabilitiesBeforeNamespaceWork(t *testing.T) {
	platform := &destinationPlatformWithoutNative{destinationPlatform: newDestinationPlatform()}
	if _, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("claimed native methods error=%v", err)
	}

	unsafe, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	unverifiable, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)
	supported := outputcap.SupportedCapability()

	for name, testCase := range map[string]struct {
		capabilities outputcap.DestinationCapabilities
		wantErr      error
	}{
		"unsafe publication":    {capabilities: mustDestinationCapabilities(t, unsafe, supported), wantErr: outputcap.ErrOrdinaryOutputUnsupported},
		"unverifiable recovery": {capabilities: mustDestinationCapabilities(t, supported, unverifiable), wantErr: outputcap.ErrRecoverableOutputUnsupported},
	} {
		t.Run(name, func(t *testing.T) {
			platform.capabilities = testCase.capabilities
			beforeGuards := platform.guardCalls
			authority, err := BindDestination(BindConfig{
				Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
			})
			if authority != nil || !errors.Is(err, testCase.wantErr) || platform.guardCalls != beforeGuards {
				t.Fatalf("unsupported destination must be rejected before namespace work: %v", err)
			}
		})
	}
}

func TestBoundDestinationRejectsReplacedRoot(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()

	platform.guardRoot = &destinationNode{id: 99, identity: []byte("foreign-root"), entries: map[string]*destinationNode{}}
	_, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}))
	if !errors.Is(err, ErrRetainedRootChanged) {
		t.Fatalf("replaced root error=%v", err)
	}
}

func TestReserveSingleFileClaimsMetadataWhilePublicNameStaysAbsent(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	claimer := &reservationClaimer{collisions: 2}
	reserved, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), claimer))
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Close()
	entry := reserved.ReservedEntry()
	if entry.CollisionIndex() != 2 || entry.EntryKind() != receivecontract.ContainerEntrySingleFile ||
		len(reserved.PersistentIdentityClaim()) != 0 || reserved.MetadataClaim().Generation != 2 {
		t.Fatalf("reservation=%+v identity=%x claim=%+v", entry, reserved.PersistentIdentityClaim(), reserved.MetadataClaim())
	}
	if len(claimer.specs) != 3 || claimer.specs[2].CanonicalNameKey != strings.ToUpper(entry.PhysicalName()) {
		t.Fatalf("claim specs=%+v", claimer.specs)
	}
	if _, present := platform.root.entries[entry.PhysicalName()]; present {
		t.Fatalf("single-file reservation mutated public name %q", entry.PhysicalName())
	}
	reopened, err := authority.ReopenTopLevel(ExpectedReservation{
		Reservation: reserved.CanonicalReservation(), MetadataClaim: reserved.MetadataClaim(),
	})
	if err != nil || reopened.ReservedEntry() != entry {
		t.Fatalf("reopen=%+v err=%v", reopened, err)
	}
	_ = reopened.Close()
}

func TestTopLevelReservationsExposeOnlyFreshlyRevalidatedExecutionRoots(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()

	single, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}))
	if err != nil {
		t.Fatal(err)
	}
	if single.RootOpenDisposition() != outputcap.CallerProvidedContainer {
		t.Fatalf("single-file root disposition = %q", single.RootOpenDisposition())
	}
	singleGuard, err := single.AcquirePublicOperationGuard()
	if err != nil || singleGuard.Root() == nil {
		t.Fatalf("single-file execution guard = (%T, %v)", singleGuard, err)
	}
	same, err := singleGuard.Root().SameDirectory(platform.Root())
	if err != nil || !same {
		t.Fatalf("single-file execution root = (%t, %v)", same, err)
	}
	if err := singleGuard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := single.CanonicalLocatorKey("file.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := single.CanonicalComponentKey("file.txt"); err != nil {
		t.Fatal(err)
	}
	if err := single.ValidateModifiedTime(catalog.ModifiedTime{}); err != nil {
		t.Fatal(err)
	}
	_ = single.Close()

	result, err := authority.ReserveTopLevel(reservationRequest(t, resultRootArtifact(t), &reservationClaimer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	if result.RootOpenDisposition() != outputcap.AuthorityCreatedRoot {
		t.Fatalf("result-root disposition = %q", result.RootOpenDisposition())
	}
	if err := result.BorrowResultRoot(func(root outputcap.Directory) error {
		if root == nil {
			t.Fatal("borrowed result root is nil")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resultGuard, err := result.AcquirePublicOperationGuard()
	if err != nil || resultGuard.Root() == nil {
		t.Fatalf("result-root execution guard = (%T, %v)", resultGuard, err)
	}
	same, err = resultGuard.Root().SameDirectory(result.directory)
	if err != nil || !same {
		t.Fatalf("result-root execution authority = (%t, %v)", same, err)
	}
	if err := resultGuard.Close(); err != nil {
		t.Fatal(err)
	}
	platform.root.entries[result.entry.PhysicalName()] = &destinationNode{
		id: 99, identity: []byte("replacement"), entries: map[string]*destinationNode{},
	}
	if _, err := result.AcquirePublicOperationGuard(); !errors.Is(err, ErrRetainedRootChanged) {
		t.Fatalf("replaced result root guard error = %v", err)
	}
}

func TestReservationClaimSpecRejectsNonCanonicalCandidate(t *testing.T) {
	request := reservationRequest(t, singleFileArtifact(t), &reservationClaimer{})
	spec := ReservationClaimSpec{
		CanonicalNameKey: "FILE.TXT", OperationID: request.OperationID, ReservationID: request.ReservationID,
		EntryKind: receivecontract.ContainerEntrySingleFile, RequestedName: "file.txt",
		LogicalReservedName: "foreign.txt", PhysicalName: "foreign.txt", CollisionIndex: 0,
	}
	if spec.Valid() {
		t.Fatal("claim spec accepted a name that was not derived by CollisionName")
	}
}

func TestReserveSingleFileRollsBackOccupiedClaimAndBoundsAttempts(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	platform.root.entries["file.txt"] = &destinationNode{id: 50, file: &destinationFile{}}
	claimer := &reservationClaimer{}
	reserved, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), claimer))
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Close()
	if reserved.ReservedEntry().CollisionIndex() != 1 || len(claimer.claims) != 2 || !claimer.claims[0].rolledBack {
		t.Fatalf("entry=%+v claims=%+v", reserved.ReservedEntry(), claimer.claims)
	}

	exhausted := &reservationClaimer{collisions: int(ordinaryoutput.MaximumResultNameReservationAttemptsV1)}
	if _, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), exhausted)); !errors.Is(err, ErrReservationExhausted) {
		t.Fatalf("exhaustion error=%v", err)
	}
}

func TestReserveRejectsContradictoryMetadataCollision(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	claimer := &reservationClaimer{collisions: 1, collisionHandle: true}
	if _, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), claimer)); !errors.Is(err, ErrReservationIndeterminate) || len(claimer.claims) != 1 || !claimer.claims[0].closed {
		t.Fatalf("contradictory claim error=%v claims=%+v", err, claimer.claims)
	}
}

func TestReserveDirectoryCreatesOrdinaryRootAndReopensExactIdentity(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	claimer := &reservationClaimer{}
	reserved, err := authority.ReserveTopLevel(reservationRequest(t, resultRootArtifact(t), claimer))
	if err != nil {
		t.Fatal(err)
	}
	entry := reserved.ReservedEntry()
	publicNode := platform.root.entries[entry.PhysicalName()]
	if publicNode == nil || publicNode.private || reserved.directory == nil ||
		!bytes.Equal(reserved.PersistentIdentityClaim(), publicNode.identity) || reserved.MetadataClaim().Generation != 3 {
		t.Fatalf("node=%+v identity=%x claim=%+v", publicNode, reserved.PersistentIdentityClaim(), reserved.MetadataClaim())
	}
	physical, err := PhysicalArtifactPath(entry.RequestedName()+"/child/file.txt", entry)
	if err != nil || physical != "child/file.txt" {
		t.Fatalf("physical=%q err=%v", physical, err)
	}
	rootPhysical, err := PhysicalArtifactPath(entry.RequestedName(), entry)
	if err != nil || rootPhysical != "" {
		t.Fatalf("root physical=%q err=%v", rootPhysical, err)
	}
	expected := ExpectedReservation{
		Reservation: reserved.CanonicalReservation(), PersistentIdentityClaim: reserved.PersistentIdentityClaim(),
		MetadataClaim: reserved.MetadataClaim(),
	}
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := authority.ReopenTopLevel(expected)
	if err != nil || reopened.directory == nil {
		t.Fatalf("reopen=%+v err=%v", reopened, err)
	}
	_ = reopened.Close()
	platform.root.entries[entry.PhysicalName()] = &destinationNode{
		id: 70, identity: []byte("replacement-identity"), entries: map[string]*destinationNode{},
	}
	if _, err := authority.ReopenTopLevel(expected); !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("replacement error=%v", err)
	}
}

func TestReserveDirectoryReleasesProvenCollisionButPreservesIndeterminate(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	platform.root.entries[receivecontract.DefaultResultRootName] = &destinationNode{
		id: 40, identity: []byte("foreign"), entries: map[string]*destinationNode{},
	}
	claimer := &reservationClaimer{}
	reserved, err := authority.ReserveTopLevel(reservationRequest(t, resultRootArtifact(t), claimer))
	if err != nil || reserved.ReservedEntry().CollisionIndex() != 1 || !claimer.claims[0].rolledBack {
		t.Fatalf("reservation=%+v err=%v claims=%+v", reserved, err, claimer.claims)
	}
	_ = reserved.Close()

	platform.directoryOutcome = outputcap.PublishNoReplaceIndeterminate
	uncertainClaimer := &reservationClaimer{}
	_, err = authority.ReserveTopLevel(reservationRequest(t, resultRootArtifact(t), uncertainClaimer))
	if !errors.Is(err, ErrReservationIndeterminate) || len(uncertainClaimer.claims) != 1 ||
		uncertainClaimer.claims[0].rolledBack || !uncertainClaimer.claims[0].closed {
		t.Fatalf("indeterminate err=%v claims=%+v", err, uncertainClaimer.claims)
	}
}

func TestPublicationRespectsNoReplaceContract(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform)
	defer authority.Close()
	source := &destinationFile{}
	outcome, err := authority.PublishFileNoReplace(source, "result.bin")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish outcome=%v err=%v", outcome, err)
	}
	platform.fileOutcome, platform.fileErr = outputcap.PublishNoReplaceCommitted, errDestinationFake
	if outcome, err := authority.PublishFileNoReplace(source, "uncertain.bin"); outcome != outputcap.PublishNoReplaceIndeterminate || !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("contradictory publish outcome=%v err=%v", outcome, err)
	}
}

func mustDestinationCapabilities(
	t *testing.T,
	safePublish outputcap.CapabilityEvidence,
	operationRecovery outputcap.CapabilityEvidence,
) outputcap.DestinationCapabilities {
	t.Helper()
	capabilities, err := outputcap.NewDestinationCapabilities(safePublish, operationRecovery)
	if err != nil {
		t.Fatal(err)
	}
	return capabilities
}

func bindFakeDestination(
	t *testing.T,
	platform outputcap.Platform,
) *BoundDestination {
	t.Helper()
	authority, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func reservationRequest(
	t *testing.T,
	artifact receivecontract.ArtifactSpec,
	claimer ReservationMetadataClaimer,
) ReservationRequest {
	t.Helper()
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{0x11}, 16))
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{0x22}, 16))
	if err != nil {
		t.Fatal(err)
	}
	return ReservationRequest{
		OperationID: operation, ReservationID: reservationID, Artifact: artifact, Metadata: claimer,
	}
}

func singleFileArtifact(t *testing.T) receivecontract.ArtifactSpec {
	t.Helper()
	var file catalog.FileID
	file[0] = 1
	artifact, err := receivecontract.NewSingleFileDirectoryTree(file, "file.txt", "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func resultRootArtifact(t *testing.T) receivecontract.ArtifactSpec {
	t.Helper()
	artifact, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

type identityOnlyDirectory struct{ outputcap.Directory }

type destinationPlatformWithoutNative struct{ *destinationPlatform }

func (platform *destinationPlatformWithoutNative) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	platform.guardCalls++
	return &destinationGuard{root: identityOnlyDirectory{Directory: platform.Root()}}, nil
}
