package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
)

func TestOutputSettlementSumTypesRejectMalformedStates(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)

	if _, err := NewVerifiedFileSettlement(FilePublished, VerifiedDurableRanges{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("published settlement without a binding error = %v", err)
	}
	if _, err := NewMaterializationStateRef(OutputSessionID{}, binding.Locator().Digest()); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a session error = %v", err)
	}
	if _, err := NewMaterializationStateRef(binding.OutputSessionID(), MaterializationLocatorDigest{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a locator error = %v", err)
	}

	reference, err := NewMaterializationStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewImmediateItemBlockedFileSettlement(
		FileMaterializationTarget{}, reference, ItemBlockOwnershipUnknown,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound immediate quarantine error = %v", err)
	}
	var foreignLocator MaterializationLocatorDigest
	foreignLocator[0] = 99
	foreignReference, err := NewMaterializationStateRef(binding.OutputSessionID(), foreignLocator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTransactionItemBlockedFileSettlement(
		binding, foreignReference, ItemBlockOwnershipUnknown,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("foreign transaction quarantine error = %v", err)
	}

	if FileSettlementKind(0).valid() || !FilePublished.valid() || (FileFailed + 1).valid() {
		t.Fatal("file settlement kind admitted a value outside its closed domain")
	}
	malformedQuarantine := FileSettlement{kind: FileItemBlocked, target: binding.Target()}
	if malformedQuarantine.valid() {
		t.Fatal("quarantine without durable state evidence was valid")
	}
	if (FileSettlement{kind: FileSettlementKind(255)}).valid() {
		t.Fatal("unknown settlement kind was valid")
	}
	if (FileSettlement{}).matchesBinding(binding) {
		t.Fatal("zero settlement matched an owned binding")
	}

	if _, err := NewFileTransactionStart(nil, checkpoint); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("nil transaction start error = %v", err)
	}
	if _, err := NewFileSettlementStart(FileSettlement{kind: FilePublished}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("malformed immediate publication error = %v", err)
	}
	if (FileStart{}).valid() {
		t.Fatal("zero file start was valid")
	}
}

func TestOutputLifecycleReasonsAndAuthorityFunctionAreClosedDomains(t *testing.T) {
	if FilePauseReason(0).valid() || !FilePauseInterrupted.valid() || (FilePauseDependencyContract + 1).valid() {
		t.Fatal("file pause reason admitted a value outside its closed domain")
	}
	if JobPauseReason(0).valid() || !JobPauseInterrupted.valid() || (JobPauseDependencyContract + 1).valid() {
		t.Fatal("job pause reason admitted a value outside its closed domain")
	}
	if _, err := NewDirectTreeSettlement(DirectTreeSettlementKind(0)); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unknown job settlement error = %v", err)
	}

	var nilAuthority DirectTreeMaterializerFunc
	if _, err := nilAuthority.OpenDirectTree(context.Background(), ReceiveIntent{}); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("nil output authority error = %v", err)
	}
	want := errors.New("authority invoked")
	called := false
	authority := DirectTreeMaterializerFunc(func(context.Context, ReceiveIntent) (DirectTreeSession, error) {
		called = true
		return nil, want
	})
	if _, err := authority.OpenDirectTree(context.Background(), ReceiveIntent{}); !called || !errors.Is(err, want) {
		t.Fatalf("authority call = (%v, %v), want delegated error", called, err)
	}
}

func TestPublishedSettlementWarningsProvenanceAndDestinationCoordinatesAreExplicit(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)

	collision, err := NewTransactionCollisionFileSettlement(binding)
	if err != nil || collision.Kind() != FileCollision {
		t.Fatalf("transaction collision = (%d, %v)", collision.Kind(), err)
	}
	if observed, ok := collision.MaterializedBinding(); !ok || observed != binding {
		t.Fatalf("collision binding = (%+v, %t)", observed, ok)
	}
	if _, ok := collision.PublicationProvenance(); ok {
		t.Fatal("collision exposed publication provenance")
	}
	if _, err := NewTransactionCollisionFileSettlement(MaterializedFileBinding{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("invalid transaction collision error = %v", err)
	}

	published, err := NewVerifiedFileSettlement(FilePublished, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if provenance, ok := published.PublicationProvenance(); !ok || provenance != FileDownloaded {
		t.Fatalf("default provenance = (%d, %t)", provenance, ok)
	}
	published, err = published.WithPublicationProvenance(FileResumed)
	if err != nil {
		t.Fatal(err)
	}
	if provenance, ok := published.PublicationProvenance(); !ok || provenance != FileResumed {
		t.Fatalf("resumed provenance = (%d, %t)", provenance, ok)
	}
	if _, err := published.WithPublicationProvenance(0); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unknown provenance error = %v", err)
	}
	if _, err := collision.WithPublicationProvenance(FileDownloaded); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("collision provenance error = %v", err)
	}

	published, err = published.WithMetadataWarnings([]FileMetadataWarning{
		FileMetadataModifiedTime,
		FileMetadataModifiedTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	warnings := published.MetadataWarnings()
	if len(warnings) != 1 || warnings[0] != FileMetadataModifiedTime {
		t.Fatalf("metadata warnings = %+v", warnings)
	}
	warnings[0] = 0
	if published.MetadataWarnings()[0] != FileMetadataModifiedTime {
		t.Fatal("metadata warning accessor aliased settlement state")
	}
	if _, err := published.WithMetadataWarnings([]FileMetadataWarning{0}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unknown metadata warning error = %v", err)
	}
	if _, err := (FileSettlement{}).WithMetadataWarnings(nil); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("zero settlement warning error = %v", err)
	}
	if !published.valid() || FileMetadataWarning(0).valid() ||
		!FileMetadataModifiedTime.valid() || FileMetadataWarning(2).valid() {
		t.Fatal("metadata warning domain changed")
	}

	transient, err := NewTransientPublishedFileSettlement(binding)
	if err != nil {
		t.Fatal(err)
	}
	if provenance, ok := transient.PublicationProvenance(); !ok || provenance != FileDownloaded {
		t.Fatalf("transient provenance = (%d, %t)", provenance, ok)
	}
	if _, err := NewTransientPublishedFileSettlement(MaterializedFileBinding{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("invalid transient publication error = %v", err)
	}

	path, err := NewOutputDestinationPath("folder/file.bin")
	if err != nil || !path.Valid() || path.IsSessionRoot() || path.String() != "folder/file.bin" {
		t.Fatalf("destination path = (%+v, %v)", path, err)
	}
	root := OutputDestinationSessionRoot()
	if !root.Valid() || !root.IsSessionRoot() || root.String() != "" {
		t.Fatalf("destination root = %+v", root)
	}
	for _, invalid := range []string{"", "/absolute", "folder//file"} {
		if _, err := NewOutputDestinationPath(invalid); !errors.Is(err, ErrInvalidOutputBinding) {
			t.Fatalf("invalid destination %q error = %v", invalid, err)
		}
	}

	rootID := admissionTestDirectoryID(t, 0x71)
	intent := admissionTestIntent(t, rootID, 0x72)
	projector, err := OrdinaryOutputArtifactPathProjector(intent)
	if err != nil {
		t.Fatal(err)
	}
	scope := admissionTestScope(t, intent)
	secret := admissionTestSequence(0x73, directoryAdmissionSecretBytes)
	rootSource := admissionTestDirectory(
		t, rootID, admissionTestGeneration(t, 0x74), DirectoryAdmission{}, "", catalog.ModifiedTime{},
	)
	rootAdmission, err := NewDirectoryAdmissionWithSecret(secret, scope, rootSource)
	if err != nil {
		t.Fatal(err)
	}
	directory := admissionTestDirectory(
		t, admissionTestDirectoryID(t, 0x75), admissionTestGeneration(t, 0x76),
		rootAdmission, "folder", catalog.ModifiedTime{},
	)
	request, err := NewDirectoryMaterializationRequest(
		projector, directory, ordinaryoutput.SourceNodeSelected, MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := NewDirectoryAdmissionWithSecret(secret, scope, directory)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := NewMaterializedDirectoryClaim(admission, request)
	if err != nil || !claim.Valid() {
		t.Fatalf("materialized directory claim = (%+v, %v)", claim, err)
	}
	if _, err := NewMaterializedDirectoryClaim(DirectoryAdmission{}, request); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("zero admission claim error = %v", err)
	}
	if (MaterializedDirectoryClaim{}).Valid() {
		t.Fatal("zero materialized directory claim became valid")
	}
}

func outputLifecycleFixture(t *testing.T) (MaterializedFileBinding, VerifiedDurableRanges) {
	t.Helper()
	descriptor := transferDescriptor(t, 1)
	locator, err := NewPathMaterializationLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var objectIdentity OwnedObjectID
	objectIdentity[0] = 32
	binding, err := NewMaterializedFileBinding(
		transferID[OutputSessionID](31),
		descriptor,
		locator,
		objectIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize()}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := VerifyDurableRanges(binding, 1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	return binding, checkpoint
}
