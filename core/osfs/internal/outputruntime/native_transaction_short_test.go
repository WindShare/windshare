package outputruntime

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type liveTransactionFixture struct {
	authority *Authority
	session   transfer.DirectTreeSession
	start     transfer.FileStart
	finalPath string
}

func openLiveTransactionFixture(
	t *testing.T,
	root string,
	seed byte,
	collision bool,
) liveTransactionFixture {
	t.Helper()
	ctx := context.Background()
	selection := nativeReservationTestSelection(t, seed)
	fileID := incrementalTestIdentity16[catalog.FileID](seed + 2)
	artifact, err := receivecontract.NewSingleFileDirectoryTree(fileID, "live.bin", "live.bin")
	if err != nil {
		t.Fatal(err)
	}
	authority, err := New(Config{
		RootPath: root,
		PlatformFactory: func(path string, create bool) (outputcap.Platform, error) {
			base, openErr := openOutputRuntimeTestPlatform(path, create)
			if openErr != nil {
				return nil, openErr
			}
			return &liveOnlyRuntimePlatform{Platform: base}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	mode, err := authority.BindDestination(ctx)
	if err != nil || !mode.LiveOnly() {
		t.Fatalf("live bind = (%+v, %v)", mode, err)
	}
	lookup, err := authority.LookupActive(ctx, selection)
	if err != nil || lookup.Kind() != ActiveLookupMiss {
		t.Fatalf("live lookup = (%d, %v)", lookup.Kind(), err)
	}
	operation, err := authority.CreateOperation(ctx, lookup, artifact)
	if err != nil {
		t.Fatal(err)
	}
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("live operation omitted intent")
	}
	reservation, _ := intent.MaterializationPlan().DestinationReservation()
	session, err := authority.OpenOperation(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = session.PauseTree(context.Background(), transfer.JobPauseInterrupted)
	})
	rootSource := transfer.AuthenticatedSourceDirectory{
		DirectoryID: selection.SyntheticRoot(),
		Generation:  incrementalTestIdentity16[catalog.DirectoryGeneration](seed + 3),
		SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
	}
	finalPath := filepath.Join(root, reservation.PhysicalName())
	if collision {
		if err := os.WriteFile(finalPath, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	geometry, err := content.NewFileGeometry(4, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID,
		incrementalTestIdentity16[content.FileRevision](seed+4),
		geometry, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	start, err := session.BeginFile(
		ctx,
		ordinaryResumeMaterializationFile(
			t, session, intent, descriptor, "live.bin", rootSource, transfer.DirectoryAdmission{}, transfer.MaterializedDirectoryClaim{},
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return liveTransactionFixture{
		authority: authority, session: session,
		start: start, finalPath: finalPath,
	}
}

func TestLiveOnlyFileTransactionPublishesWithoutPersistentInventory(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	fixture := openLiveTransactionFixture(t, root, 0x81, false)
	transaction, _, ok := fixture.start.Transaction()
	if !ok {
		t.Fatal("live file settled before content")
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("live commit = (%d, %v)", settlement.Kind(), err)
	}
	tree, err := fixture.session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess)
	if err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("live tree = (%d, %v)", tree.Kind(), err)
	}
	if err := fixture.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fixture.finalPath); err != nil || string(data) != "data" {
		t.Fatalf("live final = (%q, %v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(
		root, checkpointstore.ControlDirectory, checkpointstore.OrdinaryRegistryDirectoryV1,
	)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("live transfer created resumable inventory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("successful live transfer retained private control state: %v", err)
	}
}

func TestLiveOnlyFileTransactionPauseAndCollisionPreserveFinalAuthority(t *testing.T) {
	t.Run("pause removes private stage", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		fixture := openLiveTransactionFixture(t, root, 0x91, false)
		transaction, _, ok := fixture.start.Transaction()
		if !ok {
			t.Fatal("live file settled before content")
		}
		if err := transaction.WriteRange(context.Background(), 0, []byte("da")); err != nil {
			t.Fatal(err)
		}
		settlement, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
		if err != nil || settlement.Kind() != transfer.FilePaused {
			t.Fatalf("live pause = (%d, %v)", settlement.Kind(), err)
		}
		if _, err := fixture.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
			t.Fatal(err)
		}
		if err := fixture.authority.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(fixture.finalPath); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("live pause changed final authority: %v", err)
		}
	})

	t.Run("collision preserves foreign final", func(t *testing.T) {
		root := newRuntimeTestRootSpec(t).path
		fixture := openLiveTransactionFixture(t, root, 0xa1, true)
		settlement, ok := fixture.start.ImmediateSettlement()
		if !ok || settlement.Kind() != transfer.FileCollision {
			t.Fatalf("live collision = (%d, %t)", settlement.Kind(), ok)
		}
		if _, err := fixture.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
			t.Fatal(err)
		}
		if err := fixture.authority.Close(); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(fixture.finalPath); err != nil || string(data) != "foreign" {
			t.Fatalf("foreign final = (%q, %v)", data, err)
		}
	})
}

func TestPublishedOrdinaryFileReopensAsVerifiedSiblingThenRetiresPrivateState(t *testing.T) {
	root := newRuntimeTestRootSpec(t).path
	first := openOrdinaryResumeSession(t, root, 0xb1, 4)
	if err := first.transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settlement, err := first.transaction.Commit(context.Background()); err != nil ||
		settlement.Kind() != transfer.FilePublished {
		t.Fatalf("first commit = (%d, %v)", settlement.Kind(), err)
	}
	if _, err := first.session.PauseTree(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if err := first.authority.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopenOrdinaryResumeFile(t, root, 0xb1, 4)
	if reopened.transaction != nil || reopened.settlement.Kind() != transfer.FilePublished {
		t.Fatalf("reopened published file = transaction %T settlement %d", reopened.transaction, reopened.settlement.Kind())
	}
	tree, err := reopened.session.FinalizeTree(context.Background(), transfer.DirectTreeOutcomeSuccess)
	if err != nil || tree.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("reopened tree = (%d, %v)", tree.Kind(), err)
	}
	if err := reopened.authority.Close(); err != nil {
		t.Fatal(err)
	}
	repository, err := NewNativeResumeRepository(root, openOutputRuntimeTestPlatform, nil)
	if err != nil {
		t.Fatal(err)
	}
	page, err := repository.Page(context.Background(), resumeauthority.PageCursor{}, 8)
	if err != nil || len(page.Headers()) != 0 {
		t.Fatalf("completed operation inventory = (%d, %v)", len(page.Headers()), err)
	}
	if data, err := os.ReadFile(reopened.finalPath); err != nil || string(data) != "data" {
		t.Fatalf("published sibling = (%q, %v)", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, checkpointstore.ControlDirectory)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("successful resumable transfer retained private control state: %v", err)
	}
}
