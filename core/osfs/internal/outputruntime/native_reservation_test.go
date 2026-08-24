package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const retiredAggregateCheckpointDirectory = "checkpoints-v2"

func TestStagedAuthorityCreatesAndExactlyReopensNamedOperation(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x31)
	resultLayout, err := receivecontract.NewCompleteDirectoryResultRoot(
		incrementalTestIdentity16[catalog.DirectoryID](0x33), "docs",
	)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(resultLayout)
	if err != nil {
		t.Fatal(err)
	}
	first := newNativeReservationTestAuthority(t, root.path)
	mode, err := first.BindDestination(context.Background())
	if err != nil || !mode.Resumable() {
		t.Fatalf("bind mode = (%+v, %v)", mode, err)
	}
	lookup, err := first.LookupActive(context.Background(), selection)
	if err != nil || lookup.Kind() != ActiveLookupMiss {
		t.Fatalf("initial lookup = (%d, %v)", lookup.Kind(), err)
	}
	operation, err := first.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("created operation omitted its frozen intent")
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if reservation.Kind() != receivecontract.ReservationNamedContainerEntry ||
		reservation.LogicalReservedName() != "docs" || reservation.PhysicalName() != "docs" {
		t.Fatalf("named reservation = kind %d logical %q physical %q", reservation.Kind(),
			reservation.LogicalReservedName(), reservation.PhysicalName())
	}
	if info, err := os.Stat(filepath.Join(root.path, reservation.PhysicalName())); err != nil || !info.IsDir() {
		t.Fatalf("direct result root = (%v, %v)", info, err)
	}
	if _, err := os.Stat(filepath.Join(root.path, checkpointstore.ControlDirectory, retiredAggregateCheckpointDirectory)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staged ordinary path touched legacy checkpoint namespace: %v", err)
	}
	session, err := first.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.OpenOperation(context.Background(), operation); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("operation reopened twice: %v", err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newNativeReservationTestAuthority(t, root.path)
	if _, err := second.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := second.LookupActive(context.Background(), selection)
	if err != nil || reopened.Kind() != ActiveLookupReopened {
		t.Fatalf("reopened lookup = (%d, %v)", reopened.Kind(), err)
	}
	reopenedOperation := reopened.Operation()
	reopenedIntent, ok := reopenedOperation.ReceiveIntent()
	if !ok || !reopenedIntent.EqualCanonical(intent) {
		t.Fatal("exact reopen changed the frozen intent")
	}
	reopenedSession, err := second.OpenOperation(context.Background(), reopenedOperation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopenedSession.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestControlNamespaceIsRecycledOnlyAfterLastBoundAuthorityCloses(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	first := newNativeReservationTestAuthority(t, root.path)
	second := newNativeReservationTestAuthority(t, root.path)
	if _, err := first.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := second.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root.path, checkpointstore.ControlDirectory)); err != nil || !info.IsDir() {
		t.Fatalf("live peer lost shared control namespace: (%v, %v)", info, err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root.path, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("last bound authority retained empty control namespace: %v", err)
	}
}

func TestNamedResultRootSessionDoesNotDuplicateReservedRoot(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x34)
	anchor := incrementalTestIdentity16[catalog.DirectoryID](0x37)
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(anchor, "docs")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	authority := newNativeReservationTestAuthority(t, root.path)
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("named operation omitted intent")
	}
	session, err := authority.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	anchorSourcePath, _ := ordinaryoutput.NewSourceCatalogPath("docs")
	anchorSource := transfer.AuthenticatedSourceDirectory{
		DirectoryID: anchor,
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](0x39),
		SourcePath:  anchorSourcePath,
	}
	anchorRequest, err := transfer.NewDirectoryMaterializationRequest(
		intent, anchorSource, ordinaryoutput.SourceNodeSelected, transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	anchorAdmission, err := session.AdmitDirectory(context.Background(), anchorRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, materialized := anchorRequest.Projection().ArtifactPath()
	if !materialized {
		t.Fatal("result-root anchor was not materialized")
	}
	anchorClaim, err := transfer.NewMaterializedDirectoryClaim(anchorAdmission, anchorRequest)
	if err != nil {
		t.Fatal(err)
	}
	childPath, _ := ordinaryoutput.NewSourceCatalogPath("docs/child")
	childSource := transfer.AuthenticatedSourceDirectory{
		DirectoryID:     incrementalTestIdentity16[catalog.DirectoryID](0x3a),
		Generation:      incrementalTestIdentity16[catalog.DirectoryGeneration](0x3b),
		ParentAdmission: anchorAdmission,
		SourcePath:      childPath,
	}
	childRequest, err := transfer.NewDirectoryMaterializationRequest(
		intent, childSource, ordinaryoutput.SourceNodeSelected, anchorClaim,
	)
	if err != nil {
		t.Fatal(err)
	}
	childAdmission, err := session.AdmitDirectory(context.Background(), childRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), childAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeDirectory(context.Background(), anchorAdmission); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root.path, "docs", "child")); err != nil || !info.IsDir() {
		t.Fatalf("named child = (%v, %v)", info, err)
	}
	if _, err := os.Stat(filepath.Join(root.path, "docs", "docs")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("result root was duplicated: %v", err)
	}
}

func TestStagedAuthorityReservedSuffixIsFrozenAcrossReopen(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	if err := os.WriteFile(filepath.Join(root.path, "report.txt"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0x35)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x36), "report.txt", "report.txt",
	)
	if err != nil {
		t.Fatal(err)
	}
	first := newNativeReservationTestAuthority(t, root.path)
	if _, err := first.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := first.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := first.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := operation.ReceiveIntent()
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if reservation.CollisionIndex() != 1 || reservation.LogicalReservedName() == "report.txt" ||
		reservation.PhysicalName() != reservation.LogicalReservedName() {
		t.Fatalf("collision reservation = index %d logical %q physical %q", reservation.CollisionIndex(),
			reservation.LogicalReservedName(), reservation.PhysicalName())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := newNativeReservationTestAuthority(t, root.path)
	if _, err := second.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	reopened, err := second.LookupActive(context.Background(), selection)
	if err != nil || reopened.Kind() != ActiveLookupReopened {
		t.Fatalf("suffix reopen = (%d, %v)", reopened.Kind(), err)
	}
	reopenedIntent, ok := reopened.Operation().ReceiveIntent()
	if !ok || !reopenedIntent.EqualCanonical(intent) {
		t.Fatal("reopen replanned the reserved suffix")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStagedAuthorityBusyLookupStopsBeforeShapeOrReservation(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x37)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x38), "busy.bin", "busy.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	first := newNativeReservationTestAuthority(t, root.path)
	if _, err := first.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	lookup, err := first.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateOperation(context.Background(), lookup, artifact); err != nil {
		t.Fatal(err)
	}

	second := newNativeReservationTestAuthority(t, root.path)
	if _, err := second.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	busy, err := second.LookupActive(context.Background(), selection)
	if err != nil || busy.Kind() != ActiveLookupAlreadyRunning {
		t.Fatalf("busy lookup = (%d, %v)", busy.Kind(), err)
	}
	if _, err := second.CreateOperation(context.Background(), busy, artifact); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("busy lookup reached creation: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStagedAuthorityLeavesSingleFileAbsentUntilPublication(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	authority := newNativeReservationTestAuthority(t, root.path)
	if _, err := authority.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	selection := nativeReservationTestSelection(t, 0x41)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x42), "report.txt", "report.txt",
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := operation.ReceiveIntent()
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	if _, err := os.Stat(filepath.Join(root.path, reservation.PhysicalName())); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("single-file reservation materialized final name: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStagedAuthorityLiveOnlyNeverOpensOperationRegistry(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	factoryCalls := 0
	factory := func(path string, create bool) (outputcap.Platform, error) {
		factoryCalls++
		base, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &liveOnlyRuntimePlatform{Platform: base}, nil
	}
	authority, err := New(Config{RootPath: root.path, PlatformFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	mode, err := authority.BindDestination(context.Background())
	if err != nil || !mode.LiveOnly() {
		t.Fatalf("live-only bind = (%+v, %v)", mode, err)
	}
	selection := nativeReservationTestSelection(t, 0x51)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(
		incrementalTestIdentity16[catalog.FileID](0x53), "live.bin", "live.bin",
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.LookupActive(context.Background(), selection)
	if err != nil || lookup.Kind() != ActiveLookupMiss {
		t.Fatalf("live lookup = (%d, %v)", lookup.Kind(), err)
	}
	operation, err := authority.CreateOperation(context.Background(), lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	session, err := authority.OpenOperation(context.Background(), operation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 {
		t.Fatalf("platform opens = %d, want 1", factoryCalls)
	}
	if _, err := os.Stat(filepath.Join(root.path, ".windshare-output", checkpointstore.OrdinaryRegistryDirectoryV1)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("live-only ordinary registry exists: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
}

type liveOnlyRuntimePlatform struct {
	outputcap.Platform
}

func (*liveOnlyRuntimePlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	supported := outputcap.SupportedCapability()
	unsupported, err := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableOperationRecovery)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return outputcap.NewDestinationCapabilities(supported, unsupported, unsupported, supported)
}

func (platform *liveOnlyRuntimePlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return platform.Platform.(interface {
		LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile
	}).LiveCleanupNativeProfile()
}

func TestCatalogRootFacadeConvergesOnNamedOrdinaryLifecycle(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x59)
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	first := newNativeReservationTestAuthority(t, root.path)
	reserved, err := first.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || reserved.Kind() != NativeDirectTreeReserved {
		t.Fatalf("facade reservation = (%d, %v)", reserved.Kind(), err)
	}
	intent, ok := reserved.ReceiveIntent()
	if !ok {
		t.Fatal("facade reservation omitted its frozen intent")
	}
	layout, named := intent.ArtifactSpec().DirectoryTree()
	if !named || layout.Kind() != receivecontract.DirectoryTreeResultRoot {
		t.Fatalf("facade persisted unbounded artifact kind %d", layout.Kind())
	}
	if first.stage != authorityStageOperationReady || first.destination == nil ||
		first.registry == nil || first.operation == nil {
		t.Fatal("facade bypassed the staged ordinary authority")
	}
	if _, err := os.Stat(filepath.Join(
		root.path, checkpointstore.ControlDirectory, retiredAggregateCheckpointDirectory,
	)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("facade touched legacy checkpoint namespace: %v", err)
	}
	session, err := first.OpenDirectTree(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := newNativeReservationTestAuthority(t, root.path)
	reopened, err := second.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || reopened.Kind() != NativeDirectTreeReopened {
		t.Fatalf("facade reopen = (%d, %v)", reopened.Kind(), err)
	}
	reopenedIntent, ok := reopened.ReceiveIntent()
	if !ok || !reopenedIntent.EqualCanonical(intent) {
		t.Fatal("facade reopen changed its frozen intent or reserved name")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRootFacadeReportsRetainedOperationAsNeedsAttention(t *testing.T) {
	root := newRuntimeTestRootSpec(t)
	selection := nativeReservationTestSelection(t, 0x5a)
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	first := newNativeReservationTestAuthority(t, root.path)
	if reserved, err := first.ReserveDirectTree(context.Background(), selection, artifact); err != nil ||
		reserved.Kind() != NativeDirectTreeReserved {
		t.Fatalf("first facade reservation = (%d, %v)", reserved.Kind(), err)
	}
	second := newNativeReservationTestAuthority(t, root.path)
	busy, err := second.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || busy.Kind() != NativeDirectTreeNeedsAttention {
		t.Fatalf("busy facade reservation = (%d, %v)", busy.Kind(), err)
	}
	if _, ok := busy.ReceiveIntent(); ok {
		t.Fatal("busy facade leaked a mergeable intent")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
}

func newNativeReservationTestAuthority(t *testing.T, root string) *Authority {
	t.Helper()
	authority, err := New(Config{
		RootPath: root, PlatformFactory: openOutputRuntimeTestPlatform,
	})
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

func nativeReservationTestSelection(t *testing.T, seed byte) transfer.SelectionSpec {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		incrementalTestIdentity16[catalog.ShareInstance](seed),
		incrementalTestIdentity16[catalog.DirectoryID](seed+1),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}
