package output

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestSessionDeliveredFilesReuseJournalWithoutRetainingSettledOutcomes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0x81)
	defer auth.Close()

	payload := []byte("published-bytes")
	file := testMaterializationFile(t, session, intent, 0x83, "done.bin", uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected initial file transaction")
	}
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	committed, err := tx.Commit(ctx)
	if err != nil || committed.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%v, %v)", committed.Kind(), err)
	}

	session.mu.Lock()
	settledCount := len(session.settledOutcomes)
	activeCount := len(session.activeFiles)
	session.mu.Unlock()
	if settledCount != 0 || activeCount != 0 {
		t.Fatalf("post-commit session maps = (settled=%d, active=%d), want (0, 0)", settledCount, activeCount)
	}

	// Same-session BeginFile reuses op.journal's delivery receipt without
	// allocating any entry in session.settledOutcomes.
	replayStart, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	replayed, immediate := replayStart.ImmediateSettlement()
	if !immediate || replayed.Kind() != transfer.FilePreviouslyPublished {
		t.Fatalf("same-session replay = (immediate=%t, kind=%v), want FilePreviouslyPublished", immediate, replayed.Kind())
	}
	if replayed.Target() != file.Target() {
		t.Fatal("same-session delivery replay target mismatch")
	}

	session.mu.Lock()
	settledCount = len(session.settledOutcomes)
	session.mu.Unlock()
	if settledCount != 0 {
		t.Fatalf("post-replay settledOutcomes = %d, want 0", settledCount)
	}
}

func TestSessionNonDeliveredOutcomesReplayAndRejectForeignIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("preflight collision replays unbound and rejects foreign revision", func(t *testing.T) {
		root := t.TempDir()
		auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0x91)
		defer auth.Close()

		res, _ := intent.MaterializationPlan().DestinationReservation()
		if err := os.WriteFile(filepath.Join(root, res.PhysicalName(), "collide.bin"), []byte("existing"), 0o600); err != nil {
			t.Fatal(err)
		}

		file := testMaterializationFileWithIDs(t, session, intent, testID16[catalog.FileID](0x93), testID16[content.FileRevision](0x94), "collide.bin", 4, rootAdm)
		start1, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		s1, ok := start1.ImmediateSettlement()
		if !ok || s1.Kind() != transfer.FileCollision {
			t.Fatalf("first collision = (%v, ok=%t)", s1.Kind(), ok)
		}
		if _, bound := s1.MaterializedBinding(); bound {
			t.Fatal("preflight collision unexpectedly carried a binding")
		}

		start2, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		s2, ok := start2.ImmediateSettlement()
		if !ok || s2.Kind() != transfer.FileCollision || s2.Target() != file.Target() {
			t.Fatalf("replayed collision = (%v, ok=%t)", s2.Kind(), ok)
		}
		if _, bound := s2.MaterializedBinding(); bound {
			t.Fatal("replayed preflight collision unexpectedly carried a binding")
		}

		otherRev := testMaterializationFileWithIDs(t, session, intent, testID16[catalog.FileID](0x93), testID16[content.FileRevision](0x95), "collide.bin", 4, rootAdm)
		if _, err := session.BeginFile(ctx, otherRev); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("foreign revision on settled collision = %v, want ErrInvalidOutputBinding", err)
		}
	})

	t.Run("commit collision replays with binding", func(t *testing.T) {
		root := t.TempDir()
		auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0xa1)
		defer auth.Close()

		file := testMaterializationFileWithIDs(t, session, intent, testID16[catalog.FileID](0xa3), testID16[content.FileRevision](0xa4), "race.bin", 4, rootAdm)
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected active file transaction")
		}
		if err := tx.WriteRange(ctx, 0, []byte("data")); err != nil {
			t.Fatal(err)
		}
		res, _ := intent.MaterializationPlan().DestinationReservation()
		if err := os.WriteFile(filepath.Join(root, res.PhysicalName(), "race.bin"), []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
		committed, err := tx.Commit(ctx)
		if err != nil || committed.Kind() != transfer.FileCollision {
			t.Fatalf("commit collision = (%v, %v)", committed.Kind(), err)
		}
		origBinding, hasOrigBinding := committed.MaterializedBinding()
		if !hasOrigBinding {
			t.Fatal("commit collision did not carry binding")
		}

		replayStart, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		replayed, ok := replayStart.ImmediateSettlement()
		if !ok || replayed.Kind() != transfer.FileCollision {
			t.Fatalf("replayed commit collision = (%v, ok=%t)", replayed.Kind(), ok)
		}
		replayedBinding, hasReplayedBinding := replayed.MaterializedBinding()
		if !hasReplayedBinding || replayedBinding != origBinding {
			t.Fatalf("replayed commit collision binding = (%+v, %t), want %+v", replayedBinding, hasReplayedBinding, origBinding)
		}
	})

	t.Run("retired file replays FileFailed with binding and rejects foreign file ID", func(t *testing.T) {
		root := t.TempDir()
		auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0xb1)
		defer auth.Close()

		file := testMaterializationFileWithIDs(t, session, intent, testID16[catalog.FileID](0xb3), testID16[content.FileRevision](0xb4), "retired.bin", 4, rootAdm)
		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected active file transaction")
		}
		retired, err := tx.Retire(ctx)
		if err != nil || retired.Kind() != transfer.FileFailed {
			t.Fatalf("retire = (%v, %v)", retired.Kind(), err)
		}
		origBinding, _ := retired.MaterializedBinding()

		replayStart, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		replayed, ok := replayStart.ImmediateSettlement()
		if !ok || replayed.Kind() != transfer.FileFailed {
			t.Fatalf("replayed retired = (%v, ok=%t)", replayed.Kind(), ok)
		}
		if gotBinding, has := replayed.MaterializedBinding(); !has || gotBinding != origBinding {
			t.Fatalf("replayed retired binding = (%+v, %t), want %+v", gotBinding, has, origBinding)
		}

		otherFile := testMaterializationFileWithIDs(t, session, intent, testID16[catalog.FileID](0xb5), testID16[content.FileRevision](0xb4), "retired.bin", 4, rootAdm)
		if _, err := session.BeginFile(ctx, otherFile); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("foreign file ID on retired path = %v, want ErrInvalidOutputBinding", err)
		}
	})

	t.Run("revision conflict replays FileItemBlocked", func(t *testing.T) {
		root := t.TempDir()
		auth, session, intent, rootAdm := openTestResultRootSession(t, root, 0xc1)
		defer auth.Close()

		fileID := testID16[catalog.FileID](0xc3)
		file1 := testMaterializationFileWithIDs(t, session, intent, fileID, testID16[content.FileRevision](0xc4), "drift.bin", 8, rootAdm)
		start, err := session.BeginFile(ctx, file1)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected initial file transaction")
		}
		if err := tx.WriteRange(ctx, 0, []byte("1234")); err != nil {
			t.Fatal(err)
		}
		if paused, err := tx.Pause(ctx); err != nil || paused.Kind() != transfer.FilePaused {
			t.Fatalf("pause = (%v, %v)", paused.Kind(), err)
		}

		// Re-enter with a mismatched size on a new revision to trigger ItemBlockRevisionConflict.
		file2 := testMaterializationFileWithIDs(t, session, intent, fileID, testID16[content.FileRevision](0xc5), "drift.bin", 12, rootAdm)
		blockedStart, err := session.BeginFile(ctx, file2)
		if err != nil {
			t.Fatal(err)
		}
		blocked, ok := blockedStart.ImmediateSettlement()
		if !ok || blocked.Kind() != transfer.FileItemBlocked {
			t.Fatalf("blocked = (%v, ok=%t)", blocked.Kind(), ok)
		}
		origRef, origReason, hasOrigBlock := blocked.ItemBlock()
		if !hasOrigBlock || origReason != transfer.ItemBlockRevisionConflict {
			t.Fatalf("orig item block = (%+v, %v, %t)", origRef, origReason, hasOrigBlock)
		}

		replayStart, err := session.BeginFile(ctx, file2)
		if err != nil {
			t.Fatal(err)
		}
		replayed, ok := replayStart.ImmediateSettlement()
		if !ok || replayed.Kind() != transfer.FileItemBlocked {
			t.Fatalf("replayed blocked = (%v, ok=%t)", replayed.Kind(), ok)
		}
		gotRef, gotReason, hasGotBlock := replayed.ItemBlock()
		if !hasGotBlock || gotRef != origRef || gotReason != origReason {
			t.Fatalf("replayed item block = (%+v, %v, %t), want (%+v, %v)", gotRef, gotReason, hasGotBlock, origRef, origReason)
		}
	})
}

func TestSessionRejectsTraverseOnlyDirectoryAdmissionAndZeroEntryKindMapper(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0xd1)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(testID16[catalog.DirectoryID](0xd5), "parent/photos")
	if err != nil {
		t.Fatal(err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	sessionIface, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	session := sessionIface.(*Session)

	traverseReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  testID16[catalog.DirectoryGeneration](0xd3),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeConnectsSelection,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if traverseReq.Projection().Kind() != ordinaryoutput.ArtifactTraverseOnly {
		t.Fatalf("projection kind = %v, want ArtifactTraverseOnly", traverseReq.Projection().Kind())
	}
	if _, err := session.AdmitDirectory(ctx, traverseReq); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("AdmitDirectory(traverse-only) = %v, want ErrInvalidOutputBinding", err)
	}

	if _, err := newNameMapper(intent.OperationID(), receivecontract.DestinationReservation{}, nil, nil, nil); !errors.Is(err, ErrInvalidNameMapping) {
		t.Fatalf("newNameMapper(zero reservation) = %v, want ErrInvalidNameMapping", err)
	}
}

func openTestResultRootSession(
	t *testing.T,
	root string,
	seed byte,
) (*Authority, *Session, transfer.ReceiveIntent, transfer.DirectoryAdmission) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		_ = auth.Close()
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	sessionIface, err := auth.OpenOperation(ctx, op)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	session := sessionIface.(*Session)
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	return auth, session, intent, rootAdm
}

func testMaterializationFileWithIDs(
	t *testing.T,
	session *Session,
	intent transfer.ReceiveIntent,
	fileID catalog.FileID,
	rev content.FileRevision,
	name string,
	size uint64,
	parent transfer.DirectoryAdmission,
) transfer.MaterializationFile {
	t.Helper()
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(intent.ShareInstance(), fileID, rev, geom, catalog.ModifiedTime{})
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	parentReq, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: parent.DirectoryID(), Generation: parent.Generation(),
			SourcePath: ordinaryoutput.EmptySourceCatalogPath(), ModifiedTime: parent.ModifiedTime(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := transfer.NewMaterializedDirectoryClaim(parent, parentReq)
	if err != nil {
		t.Fatal(err)
	}
	parentSrc := parentReq.Source()
	fileParent, err := transfer.NewDirectoryMaterializationFileParent(
		parentSrc.DirectoryID, parentSrc.Generation, parentSrc.SourcePath,
		parent, claim,
	)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := transfer.NewMaterializationRootRelativePath(parent.Path() + "/" + name)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(intent, sourcePath, rel, desc, session.SessionID(), fileParent)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
