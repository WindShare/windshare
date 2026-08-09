package outputruntime

import (
	"context"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestNativeDirectTreeReservationCreatesAndExactlyReopensOneOperation(t *testing.T) {
	root := t.TempDir()
	authority := newNativeReservationTestAuthority(t, root)
	selection := nativeReservationTestSelection(t, 0x61)
	artifact := receivecontract.NewCatalogRootDirectoryTree()

	created, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || created.Kind() != NativeDirectTreeReserved {
		t.Fatalf("created reservation = (%d, %v)", created.Kind(), err)
	}
	createdIntent, ok := created.ReceiveIntent()
	if !ok || createdIntent.OperationID().IsZero() || createdIntent.BindingDigest().IsZero() {
		t.Fatalf("created receive intent = %+v", createdIntent)
	}
	reopened, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || reopened.Kind() != NativeDirectTreeReopened {
		t.Fatalf("reopened reservation = (%d, %v)", reopened.Kind(), err)
	}
	reopenedIntent, ok := reopened.ReceiveIntent()
	if !ok || !reopenedIntent.EqualCanonical(createdIntent) {
		t.Fatal("compatible reservation did not reopen the exact immutable intent")
	}

	platform, namespace, lease := holdNativeReservationOperation(t, root, createdIntent)
	defer platform.Close()
	defer namespace.Close()
	defer lease.Close()
	uncertain, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || uncertain.Kind() != NativeDirectTreeNeedsAttention {
		t.Fatalf("busy compatible ownership = (%d, %v)", uncertain.Kind(), err)
	}
	if intent, ok := uncertain.ReceiveIntent(); ok || !intent.IsZero() {
		t.Fatal("uncertain ownership exposed a mergeable receive intent")
	}
}

func TestNativeDirectTreeReservationNeverMergesMultipleCompatibleOperations(t *testing.T) {
	root := t.TempDir()
	authority := newNativeReservationTestAuthority(t, root)
	selection := nativeReservationTestSelection(t, 0x71)
	artifact := receivecontract.NewCatalogRootDirectoryTree()
	first, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil {
		t.Fatal(err)
	}
	firstIntent, ok := first.ReceiveIntent()
	if !ok {
		t.Fatalf("first reservation kind = %d", first.Kind())
	}

	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	binding, err := platform.RootBinding()
	if err != nil {
		t.Fatal(err)
	}
	authorityRef, err := receivecontract.AuthorityRefFromBytes(binding.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	namespace, _, err := openNativeCheckpointNamespace(platform, authorityRef)
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	compatible, err := checkpointmodel.NewCLICompatibleOperationKey(selection, artifact, authorityRef)
	if err != nil {
		t.Fatal(err)
	}
	secondIntent, operation, err := authority.newNativeDirectTreeIntent(
		selection, artifact, compatible, authorityRef,
	)
	if err != nil || secondIntent.OperationID() == firstIntent.OperationID() {
		t.Fatalf("second compatible operation = (%v, %v)", secondIntent.OperationID(), err)
	}
	lease, err := namespace.AcquireOperation(
		secondIntent.OperationID(), secondIntent.Digest(), secondIntent.BindingDigest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := lease.OpenOrCreateRepository()
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	reservation, _ := secondIntent.MaterializationPlan().DestinationReservation()
	if err := repository.InstallOperation(operation, reservation.CanonicalBytes()); err != nil {
		_ = repository.Close()
		_ = lease.Close()
		t.Fatal(err)
	}
	frozen, err := checkpointmodel.NewReceiveLifecycleState(checkpointmodel.LifecycleStateSpec{
		OperationID: secondIntent.OperationID(), ReceiveIntent: secondIntent.Digest(),
		StateGeneration: 1, Phase: checkpointmodel.LifecycleIntentFrozen,
	})
	if err != nil {
		_ = repository.Close()
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := repository.CreateLifecycleState(frozen); err != nil {
		_ = repository.Close()
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := lease.RegisterLookup(operation); err != nil {
		_ = repository.Close()
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := repository.Close(); err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	ambiguous, err := authority.ReserveDirectTree(context.Background(), selection, artifact)
	if err != nil || ambiguous.Kind() != NativeDirectTreeNeedsAttention {
		t.Fatalf("multiple compatible operations = (%d, %v)", ambiguous.Kind(), err)
	}
	if intent, ok := ambiguous.ReceiveIntent(); ok || !intent.IsZero() {
		t.Fatal("multiple compatible operations exposed an arbitrary merge target")
	}
}

func TestNativeDirectTreeReservationStopsAtForeignCheckpointOwnership(t *testing.T) {
	root := t.TempDir()
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := platform.RootBinding()
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	foreignBytes := binding.Bytes()
	foreignBytes[len(foreignBytes)-1] ^= 0xff
	foreignAuthority, err := receivecontract.AuthorityRefFromBytes(foreignBytes)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	foreignOwnership, err := checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:  checkpointmodel.MaterializerNativeTree,
		Certification: platform.Certification(), AuthorityRef: foreignAuthority.Bytes(),
		RootOpenDisposition: platform.RootOpenDisposition(),
	})
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	namespace, err := checkpointstore.Initialize(checkpointstore.CertifiedConfig{
		Root: platform.Root(), Ownership: foreignOwnership,
	})
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if err := namespace.Close(); err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}

	authority := newNativeReservationTestAuthority(t, root)
	reservation, err := authority.ReserveDirectTree(
		context.Background(), nativeReservationTestSelection(t, 0x81),
		receivecontract.NewCatalogRootDirectoryTree(),
	)
	if err != nil || reservation.Kind() != NativeDirectTreeNeedsAttention {
		t.Fatalf("foreign checkpoint ownership = (%d, %v)", reservation.Kind(), err)
	}
	if intent, ok := reservation.ReceiveIntent(); ok || !intent.IsZero() {
		t.Fatal("foreign checkpoint ownership exposed a mergeable receive intent")
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

func holdNativeReservationOperation(
	t *testing.T,
	root string,
	intent transfer.ReceiveIntent,
) (outputcap.Platform, checkpointstore.Namespace, checkpointstore.OperationLease) {
	t.Helper()
	platform, err := openOutputRuntimeTestPlatform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := platform.RootBinding()
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	authorityRef, err := receivecontract.AuthorityRefFromBytes(binding.Bytes())
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	namespace, _, err := openNativeCheckpointNamespace(platform, authorityRef)
	if err != nil {
		_ = platform.Close()
		t.Fatal(err)
	}
	lease, err := namespace.AcquireOperation(intent.OperationID(), intent.Digest(), intent.BindingDigest())
	if err != nil {
		_ = namespace.Close()
		_ = platform.Close()
		t.Fatal(err)
	}
	return platform, namespace, lease
}
