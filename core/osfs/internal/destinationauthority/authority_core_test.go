package destinationauthority

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestBindDestinationRetainsAuthorityAndOwnsCloseOrder(t *testing.T) {
	order := []string{}
	platform := newDestinationPlatform()
	platform.closeOrder = &order
	journal := &destinationJournal{order: &order}
	authority := bindFakeDestination(t, platform, journal)

	if !authority.Binding().Valid() || platform.capCalls != 1 || platform.guardCalls != 1 {
		t.Fatalf("binding=%+v capabilities=%d guards=%d", authority.Binding(), platform.capCalls, platform.guardCalls)
	}
	mode, err := authority.Binding().ExecutionMode()
	if err != nil || mode != outputcap.ExecutionResumable {
		t.Fatalf("mode=%v err=%v", mode, err)
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
	if got, want := strings.Join(order, ","), "resumable,journal,platform"; got != want {
		t.Fatalf("close order=%q want=%q", got, want)
	}
	if authority.Binding().Valid() || !platform.closed || !journal.closed {
		t.Fatalf("closed binding=%+v platform=%t journal=%t", authority.Binding(), platform.closed, journal.closed)
	}
	if err := authority.Close(); err != nil || len(order) != 3 {
		t.Fatalf("idempotent close err=%v order=%v", err, order)
	}
}

func TestBindDestinationReadsExistingIdentityAndRejectsInvalidConfiguration(t *testing.T) {
	platform := newDestinationPlatform()
	first := bindFakeDestination(t, platform, &destinationJournal{})
	firstID := first.Binding().ID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := bindFakeDestination(t, platform, &destinationJournal{})
	defer second.Close()
	if second.Binding().ID() != firstID || platform.capCalls != 2 {
		t.Fatalf("reopened id=%v want=%v capability calls=%d", second.Binding().ID(), firstID, platform.capCalls)
	}

	if _, err := BindDestination(BindConfig{}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("empty bind error=%v", err)
	}
	if _, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: ".", OpenLiveCleanupJournal: fakeJournalOpener(&destinationJournal{}),
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("relative display path error=%v", err)
	}
	withoutSources := struct{ outputcap.Platform }{Platform: platform}
	if _, err := BindDestination(BindConfig{
		Platform: withoutSources, DisplayPath: filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(&destinationJournal{}),
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("missing capability source error=%v", err)
	}
}

func TestBindDestinationChecksOptionalNativeMethodSetsOnlyWhenClaimed(t *testing.T) {
	platform := &destinationPlatformWithoutNative{destinationPlatform: newDestinationPlatform()}
	if _, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(&destinationJournal{}),
	}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("claimed native methods error=%v", err)
	}

	unsafe, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnsafePublication)
	cleanup, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonCleanupOwnershipUnknown)
	supported := outputcap.SupportedCapability()
	platform.capabilities, _ = outputcap.NewDestinationCapabilities(unsafe, supported, supported, cleanup)
	authority, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(&destinationJournal{}),
	})
	if err != nil {
		t.Fatalf("unsupported optional methods must not be asserted: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBoundDestinationRejectsReplacedRootAndLiveOnlyRegistry(t *testing.T) {
	platform := newDestinationPlatform()
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)
	supported := outputcap.SupportedCapability()
	platform.capabilities, _ = outputcap.NewDestinationCapabilities(supported, unsupported, supported, supported)
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	defer authority.Close()

	called := false
	if err := authority.OpenResumableState(func(outputcap.Directory) (io.Closer, error) {
		called = true
		return orderedCloser{order: &[]string{}}, nil
	}); !errors.Is(err, ErrInvalidConfiguration) || called {
		t.Fatalf("live-only registry err=%v called=%t", err, called)
	}
	platform.guardRoot = &destinationNode{id: 99, identity: []byte("foreign-root"), entries: map[string]*destinationNode{}}
	_, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}))
	if !errors.Is(err, ErrRetainedRootChanged) {
		t.Fatalf("replaced root error=%v", err)
	}
}

func TestBindDestinationDowngradesOnlyCleanupOnBoundedNegativeProof(t *testing.T) {
	for _, state := range []LiveCleanupScanState{LiveCleanupScanOverflow, LiveCleanupScanUnknown} {
		platform := newDestinationPlatform()
		authority := bindFakeDestination(t, platform, &destinationJournal{snapshot: LiveCleanupSnapshot{State: state}})
		facts := authority.Binding().Capabilities()
		if !facts.SafePublish().Supported() || !facts.OperationRecovery().Supported() ||
			!facts.RangeRecovery().Supported() || facts.CrashCleanup().Supported() {
			t.Fatalf("state=%v facts=%+v", state, facts)
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReserveSingleFileClaimsMetadataWhilePublicNameStaysAbsent(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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
	authority := bindFakeDestination(t, platform, &destinationJournal{})
	defer authority.Close()
	claimer := &reservationClaimer{collisions: 1, collisionHandle: true}
	if _, err := authority.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), claimer)); !errors.Is(err, ErrReservationIndeterminate) || len(claimer.claims) != 1 || !claimer.claims[0].closed {
		t.Fatalf("contradictory claim error=%v claims=%+v", err, claimer.claims)
	}
}

func TestReserveDirectoryCreatesOrdinaryRootAndReopensExactIdentity(t *testing.T) {
	platform := newDestinationPlatform()
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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
	authority := bindFakeDestination(t, platform, &destinationJournal{})
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

func TestPublicationAndCleanupStageRespectRecordCuts(t *testing.T) {
	platform := newDestinationPlatform()
	journal := &destinationJournal{}
	authority := bindFakeDestination(t, platform, journal)
	defer authority.Close()
	source := &destinationFile{}
	outcome, err := authority.PublishFileNoReplace(source, "result.bin")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish outcome=%v err=%v", outcome, err)
	}
	ticket := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 1)
	parent := destinationRootLiveStageParent(platform)
	stage, created, err := authority.CreateLiveCleanupStage(context.Background(), parent, ticket)
	if err != nil || stage == nil || created.State() != checkpointmodel.LiveCleanupStageCreated ||
		journal.tickets[ticket.StageName()].State() != checkpointmodel.LiveCleanupStageCreated {
		t.Fatalf("stage=%v created=%+v err=%v journal=%+v", stage, created, err, journal.tickets)
	}
	if err := authority.RemoveLiveCleanupStage(created, stage); err != nil {
		t.Fatal(err)
	}
	if _, present := journal.tickets[ticket.StageName()]; present {
		t.Fatalf("ticket still present: %+v", journal.tickets)
	}
	if _, present := authority.proof.(*destinationDirectory).node.entries[ticket.StageName()]; present {
		t.Fatalf("cleanup stage still present")
	}
	_ = stage.Close()

	platform.createStageErr = errDestinationFake
	failed := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 2)
	if _, _, err := authority.CreateLiveCleanupStage(context.Background(), parent, failed); !errors.Is(err, errDestinationFake) {
		t.Fatalf("create failure=%v", err)
	}
	if recorded := journal.tickets[failed.StageName()]; recorded.State() != checkpointmodel.LiveCleanupTicketCommitted {
		t.Fatalf("record-before-stage evidence=%+v", recorded)
	}
	platform.fileOutcome, platform.fileErr = outputcap.PublishNoReplaceCommitted, errDestinationFake
	if outcome, err := authority.PublishFileNoReplace(source, "uncertain.bin"); outcome != outputcap.PublishNoReplaceIndeterminate || !errors.Is(err, ErrReservationIndeterminate) {
		t.Fatalf("contradictory publish outcome=%v err=%v", outcome, err)
	}
}

func TestLiveCleanupStageUsesExactParentAndReconcilesInterruptedCuts(t *testing.T) {
	t.Run("exact nested parent", func(t *testing.T) {
		platform := newDestinationPlatform()
		journal := &destinationJournal{}
		authority := bindFakeDestination(t, platform, journal)
		defer authority.Close()

		root := &destinationDirectory{platform: platform, node: platform.guardRoot}
		nestedValue, err := root.CreateDirectory("nested", false)
		if err != nil {
			t.Fatal(err)
		}
		nested := nestedValue.(*destinationDirectory)
		parent := &destinationLiveStageParent{directory: nested}
		ticket := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 11)
		stage, created, err := authority.CreateLiveCleanupStage(
			context.Background(), parent, ticket,
		)
		if err != nil || parent.calls != 1 || platform.stageParent != nested.node || stage == nil {
			t.Fatalf("nested stage = (calls %d parent %p want %p stage %T, %v)",
				parent.calls, platform.stageParent, nested.node, stage, err)
		}
		if err := authority.RemoveLiveCleanupStage(created, stage); err != nil {
			t.Fatal(err)
		}
		_ = stage.Close()
		_ = nested.Close()
	})

	for name, configure := range map[string]func(*destinationLiveStageParent, error){
		"record before native create": func(parent *destinationLiveStageParent, crash error) {
			parent.before = crash
		},
		"native create before reducer promotion": func(parent *destinationLiveStageParent, crash error) {
			parent.after = crash
		},
	} {
		t.Run(name, func(t *testing.T) {
			platform := newDestinationPlatform()
			journal := &destinationJournal{}
			authority := bindFakeDestination(t, platform, journal)
			defer authority.Close()
			parent := destinationRootLiveStageParent(platform)
			crash := errors.New("simulated process cut")
			configure(parent, crash)
			ticket := cleanupTicket(t, checkpointmodel.LiveCleanupTicketCommitted, 12)

			stage, _, err := authority.CreateLiveCleanupStage(
				context.Background(), parent, ticket,
			)
			if stage != nil || !errors.Is(err, crash) ||
				journal.tickets[ticket.StageName()].State() != checkpointmodel.LiveCleanupTicketCommitted {
				t.Fatalf("interrupted create = (stage %T ticket %+v, %v)",
					stage, journal.tickets[ticket.StageName()], err)
			}
			journal.snapshot = LiveCleanupSnapshot{
				State: LiveCleanupScanComplete, Tickets: []checkpointmodel.LiveCleanupTicket{ticket},
			}
			capabilities, err := reconcileLiveCleanup(
				journal, authority.proof, platform.capabilities, platform.profile,
			)
			if err != nil || !capabilities.CrashCleanup().Supported() {
				t.Fatalf("interrupted create reconciliation = (%+v, %v)", capabilities, err)
			}
			if _, present := journal.tickets[ticket.StageName()]; present {
				t.Fatal("reconciliation retained an owned crash ticket")
			}
			if kind, _, err := authority.proof.ClassifyExactEntry(ticket.StageName()); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("reconciliation stage = (%d, %v)", kind, err)
			}
		})
	}
}

func bindFakeDestination(
	t *testing.T,
	platform outputcap.Platform,
	journal *destinationJournal,
) *BoundDestination {
	t.Helper()
	if journal.snapshot.State == 0 {
		journal.snapshot.State = LiveCleanupScanComplete
	}
	authority, err := BindDestination(BindConfig{
		Platform: platform, DisplayPath: filepath.Clean(t.TempDir()),
		OpenLiveCleanupJournal: fakeJournalOpener(journal),
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func fakeJournalOpener(journal LiveCleanupJournal) LiveCleanupJournalOpener {
	return func(control outputcap.Directory) (LiveCleanupJournalHandle, error) {
		kind, exact, err := control.ClassifyExactEntry(checkpointmodel.LiveCleanupNamespaceV1)
		if err != nil || !exact {
			return LiveCleanupJournalHandle{}, errors.Join(errDestinationFake, err)
		}
		if kind == outputcap.EntryAbsent {
			proof, createErr := control.CreateDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
			if createErr != nil {
				return LiveCleanupJournalHandle{}, createErr
			}
			if syncErr := errors.Join(proof.Sync(), control.Sync(), proof.Close()); syncErr != nil {
				return LiveCleanupJournalHandle{}, syncErr
			}
		}
		return NewLiveCleanupJournalHandle(journal)
	}
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

func (directory identityOnlyDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	return directory.Directory.(outputcap.PersistentDirectoryIdentity).PersistentDirectoryIdentityClaim()
}

func (directory identityOnlyDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return directory.Directory.(outputcap.PersistentDirectoryIdentityPreparer).PreparePersistentDirectoryIdentityClaim()
}

type destinationPlatformWithoutNative struct{ *destinationPlatform }

func (platform *destinationPlatformWithoutNative) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	platform.guardCalls++
	return &destinationGuard{root: identityOnlyDirectory{Directory: platform.Root()}}, nil
}
