package destinationauthority

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type processDestinationPlatform struct{ *destinationPlatform }

func (*processDestinationPlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	panic("process destination requested a restart profile")
}
func (*processDestinationPlatform) Certification() outputcap.CertificationID {
	panic("process destination requested a restart certification")
}
func (platform *processDestinationPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	return &destinationGuard{root: &processDestinationDirectory{destinationDirectory: &destinationDirectory{
		platform: platform.destinationPlatform, node: platform.guardRoot,
	}}}, nil
}

type processDestinationDirectory struct{ *destinationDirectory }

func (*processDestinationDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	panic("process destination enrolled restart identity")
}
func (directory *processDestinationDirectory) Duplicate() (outputcap.Directory, error) {
	return &processDestinationDirectory{destinationDirectory: directory.destinationDirectory}, nil
}
func (directory *processDestinationDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*processDestinationDirectory); ok {
		other = wrapped.destinationDirectory
	}
	return directory.destinationDirectory.SameDirectory(other)
}

func bindTestProcessDestination(t *testing.T, platform *destinationPlatform, nonce byte) *BoundDestination {
	t.Helper()
	supported := outputcap.SupportedCapability()
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableCrashCleanup)
	platform.capabilities, _ = outputcap.NewDestinationCapabilities(supported, unsupported, unsupported, unsupported)
	bound, err := BindDestination(BindConfig{
		Platform: &processDestinationPlatform{platform}, DisplayPath: filepath.Clean(t.TempDir()),
		ProcessNonceSource: bytes.NewReader(bytes.Repeat([]byte{nonce}, outputcap.DestinationAuthorityIDBytes)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bound.Close() })
	return bound
}

func TestProcessDestinationNeverEnrollsOrReopensRestartAuthority(t *testing.T) {
	platform := newDestinationPlatform()
	stale := &destinationNode{id: 800, private: true, entries: map[string]*destinationNode{
		"foreign": {file: &destinationFile{data: []byte("untouched"), size: 9}},
	}}
	platform.root.entries[controlDirectoryName] = stale
	first := bindTestProcessDestination(t, platform, 1)
	if mode, err := first.Binding().ExecutionMode(); err != nil || mode != outputcap.ExecutionLiveOnly {
		t.Fatalf("mode=%v err=%v", mode, err)
	}
	if first.control != nil || first.proof != nil || first.journal.valid() || first.LiveCleanupProfile().Valid() {
		t.Fatal("process binding retained restart authority")
	}
	if _, err := first.FileCheckpointOwnership(outputcap.CallerProvidedContainer); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("checkpoint ownership=%v", err)
	}
	reserved, err := first.ReserveTopLevel(reservationRequest(t, singleFileArtifact(t), &reservationClaimer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Close()
	if _, err := first.ReopenTopLevel(ExpectedReservation{
		Reservation: reserved.CanonicalReservation(), MetadataClaim: reserved.MetadataClaim(),
	}); !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported) {
		t.Fatalf("reopen=%v", err)
	}
	firstID := first.Binding().ID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := bindTestProcessDestination(t, platform, 2)
	if second.Binding().ID() == firstID {
		t.Fatal("fresh process binding reused restart identity")
	}
	if len(platform.root.entries) != 1 || platform.root.entries[controlDirectoryName] != stale || stale.closed != 0 {
		t.Fatal("binding touched an unauthenticated leftover")
	}
}

func TestProcessStageRetainsLiveOwnershipAndSkipsStaleNames(t *testing.T) {
	platform := newDestinationPlatform()
	bound := bindTestProcessDestination(t, platform, 3)
	nonce := bytes.Repeat([]byte{4}, processStageNonceBytes)
	staleName := processStageDirectoryPrefix + hex.EncodeToString(nonce)
	stale := &destinationNode{id: 500, private: true, entries: map[string]*destinationNode{
		processStageFileName: {file: &destinationFile{data: []byte("foreign"), size: 7}},
	}}
	platform.root.entries[staleName] = stale
	random := bytes.NewReader(append(nonce, bytes.Repeat([]byte{5}, processStageNonceBytes)...))
	stage, err := bound.CreateProcessStage(context.Background(), destinationRootLiveStageParent(platform), 4, random)
	if err != nil {
		t.Fatal(err)
	}
	if stage.name == staleName || stage.File() == nil {
		t.Fatal("stage reused stale authority")
	}
	if _, err := stage.File().WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	if err := stage.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Remove(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if len(platform.root.entries) != 1 || platform.root.entries[staleName] != stale || stale.closed != 0 {
		t.Fatal("cleanup modified a stale object")
	}
}

func TestProcessStageAmbiguousCloseAndReplacedEntryNeverDeleteForeignObject(t *testing.T) {
	for _, replace := range []bool{false, true} {
		platform := newDestinationPlatform()
		bound := bindTestProcessDestination(t, platform, 6)
		stage, err := bound.CreateProcessStage(context.Background(), destinationRootLiveStageParent(platform), 4,
			bytes.NewReader(bytes.Repeat([]byte{7}, processStageNonceBytes)))
		if err != nil {
			t.Fatal(err)
		}
		name := stage.name
		container := platform.root.entries[name]
		if replace {
			foreign := &destinationNode{file: &destinationFile{size: 4}}
			container.entries[processStageFileName] = foreign
			if err := stage.Remove(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
				t.Fatalf("foreign removal=%v", err)
			}
		}
		if err := stage.Close(); err != nil {
			t.Fatal(err)
		}
		if platform.root.entries[name] != container || len(container.entries) != 1 {
			t.Fatal("closing ambiguous stage changed the namespace")
		}
		if err := stage.Remove(); !errors.Is(err, ErrAuthorityClosed) {
			t.Fatalf("closed removal=%v", err)
		}
	}
}

func TestProcessStageNativeFailureUsesReturnedWitnessWithoutReopening(t *testing.T) {
	platform := newDestinationPlatform()
	platform.createStageErr = errDestinationFake
	bound := bindTestProcessDestination(t, platform, 8)
	stage, err := bound.CreateProcessStage(context.Background(), destinationRootLiveStageParent(platform), 4,
		bytes.NewReader(bytes.Repeat([]byte{9}, processStageNonceBytes)))
	if stage != nil || !errors.Is(err, errDestinationFake) {
		t.Fatalf("stage=%v err=%v", stage, err)
	}
	if len(platform.root.entries) != 0 {
		t.Fatal("returned native witness was not used to clean exact owned state")
	}
}

func (directory *processDestinationDirectory) ReservePublicDirectoryNoReplace(name string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	created, outcome, err := directory.destinationDirectory.ReservePublicDirectoryNoReplace(name)
	if created == nil {
		return nil, outcome, err
	}
	return &processDestinationDirectory{destinationDirectory: created.(*destinationDirectory)}, outcome, err
}

func TestProcessResultRootRetainsHandleWithoutPersistentIdentity(t *testing.T) {
	platform := newDestinationPlatform()
	bound := bindTestProcessDestination(t, platform, 11)
	reserved, err := bound.ReserveTopLevel(reservationRequest(t, resultRootArtifact(t), &reservationClaimer{}))
	if err != nil {
		t.Fatal(err)
	}
	defer reserved.Close()
	if len(reserved.PersistentIdentityClaim()) != 0 || reserved.directory == nil {
		t.Fatal("process result root confused its live handle with persistent identity")
	}
}

type processCleanupDestinationPlatform struct{ *processDestinationPlatform }

func (*processCleanupDestinationPlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return checkpointmodel.LiveCleanupWindowsNTFSV1
}

func TestCleanupJournalDoesNotRequireOperationIdentityEnrollment(t *testing.T) {
	platform := newDestinationPlatform()
	supported := outputcap.SupportedCapability()
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)
	platform.capabilities, _ = outputcap.NewDestinationCapabilities(supported, unsupported, unsupported, supported)
	journal := &destinationJournal{snapshot: LiveCleanupSnapshot{State: LiveCleanupScanComplete}}
	bound, err := BindDestination(BindConfig{
		Platform:    &processCleanupDestinationPlatform{&processDestinationPlatform{platform}},
		DisplayPath: filepath.Clean(t.TempDir()), OpenLiveCleanupJournal: fakeJournalOpener(journal),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if !bound.Binding().Capabilities().CrashCleanup().Supported() || !bound.journal.valid() || bound.control == nil {
		t.Fatal("missing operation identity erased independently proven cleanup")
	}
	if mode, err := bound.Binding().ExecutionMode(); err != nil || mode != outputcap.ExecutionLiveOnly {
		t.Fatalf("mode=%v %v", mode, err)
	}
}

func TestProcessStageParentFailureReclaimsOnlyEmptyOwnedContainer(t *testing.T) {
	platform := newDestinationPlatform()
	bound := bindTestProcessDestination(t, platform, 12)
	parent := destinationRootLiveStageParent(platform)
	parent.before = errDestinationFake
	stage, err := bound.CreateProcessStage(context.Background(), parent, 4,
		bytes.NewReader(bytes.Repeat([]byte{13}, processStageNonceBytes)))
	if stage != nil || !errors.Is(err, errDestinationFake) {
		t.Fatalf("stage=%v err=%v", stage, err)
	}
	if len(platform.root.entries) != 0 {
		t.Fatal("failed parent binding leaked an empty owned stage container")
	}
}

func TestProcessStageBoundedAllocationNeverOpensAnExistingCandidate(t *testing.T) {
	platform := newDestinationPlatform()
	bound := bindTestProcessDestination(t, platform, 14)
	nonce := bytes.Repeat([]byte{15}, processStageNonceBytes)
	name := processStageDirectoryPrefix + hex.EncodeToString(nonce)
	stale := &destinationNode{id: 600, private: true, entries: map[string]*destinationNode{}}
	platform.root.entries[name] = stale
	stage, err := bound.CreateProcessStage(context.Background(), destinationRootLiveStageParent(platform), 4,
		bytes.NewReader(bytes.Repeat(nonce, maximumProcessStageAttempts)))
	if stage != nil || !errors.Is(err, ErrReservationExhausted) {
		t.Fatalf("stage=%v err=%v", stage, err)
	}
	if platform.root.entries[name] != stale || stale.closed != 0 || len(platform.root.entries) != 1 {
		t.Fatal("exhausted allocation reused or removed stale candidate")
	}
}

type unwitnessedProcessStageCreator struct{ outputcap.Directory }

func (creator unwitnessedProcessStageCreator) CreateProcessStage(directory outputcap.Directory, name string, size int64) (outputcap.MutableFile, error) {
	file, err := directory.CreateFile(name, false, size)
	if err != nil {
		return nil, err
	}
	_, _ = file.WriteAt([]byte("keep"), 0)
	_ = file.Close()
	return nil, errDestinationFake
}

func TestProcessStageFailureCannotRemoveUnwitnessedNativeCreation(t *testing.T) {
	platform := newDestinationPlatform()
	bound := bindTestProcessDestination(t, platform, 16)
	parent := &destinationLiveStageParent{directory: unwitnessedProcessStageCreator{Directory: platform.Root()}}
	stage, err := bound.CreateProcessStage(context.Background(), parent, 4,
		bytes.NewReader(bytes.Repeat([]byte{17}, processStageNonceBytes)))
	if stage != nil || !errors.Is(err, errDestinationFake) {
		t.Fatalf("stage=%v err=%v", stage, err)
	}
	if len(platform.root.entries) != 1 {
		t.Fatal("uncertain native creation lost its container")
	}
	for _, directory := range platform.root.entries {
		file := directory.entries[processStageFileName]
		if file == nil || string(file.file.data) != "keep" {
			t.Fatal("cleanup removed an unwitnessed native file")
		}
	}
}
