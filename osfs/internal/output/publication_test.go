//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/ordinaryoutput"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestOrdinaryFilesPublishWithoutLinksAndNeverOverwrite(t *testing.T) {
	path := t.TempDir()
	root, _, err := OpenContainer(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	// Create staged file and publish to absent destination.
	staged, err := openChildFile(root, "staged.part", os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.WriteAt([]byte("hello"), 0); err != nil {
		_ = staged.Close()
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if err := renameFileNoReplace(root, "staged.part", root, "final.txt", false); err != nil {
		t.Fatalf("publish to absent target failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "staged.part")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staged source survived publish: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(path, "final.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("published content = %q, %v", got, err)
	}

	// Second publish to existing target must report collision and leave target untouched.
	second, err := openChildFile(root, "second.part", os.O_RDWR|os.O_CREATE|os.O_EXCL, publicFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.WriteAt([]byte("clobber"), 0); err != nil {
		_ = second.Close()
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	collisionErr := renameFileNoReplace(root, "second.part", root, "final.txt", false)
	if !errors.Is(collisionErr, ErrNamespaceCollision) && !errors.Is(collisionErr, fs.ErrExist) {
		t.Fatalf("no-replace publish over existing target = %v, want collision", collisionErr)
	}
	got, err = os.ReadFile(filepath.Join(path, "final.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("collision overwrote target: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(path, "second.part")); err != nil {
		t.Fatalf("collision lost staged source: %v", err)
	}
}

func TestSmallFilesFastPathLeavesNoSidecarArtifacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0x21)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, 0x22)

	const fileCount = 200
	for i := range fileCount {
		name := fmt.Sprintf("file-%03d.txt", i)
		payload := []byte(fmt.Sprintf("payload-%03d", i))
		f := testMaterializationFileSeq(t, session, intent, uint64(i+1), name, uint64(len(payload)), rootAdm)
		start, err := session.BeginFile(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("file %s did not start transaction", name)
		}
		if err := tx.WriteRange(ctx, 0, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Accept(ctx); err != nil {
			t.Fatal(err)
		}
		settled, err := tx.Commit(ctx)
		if err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit %s = (%v, %v)", name, settled.Kind(), err)
		}
	}
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Destination must contain only the result root directory (no .windshare-tmp,
	// no .wsresume-*, no windshare-names-*.json, no .anchor).
	topEntries, err := os.ReadDir(root)
	if err != nil || len(topEntries) != 1 || !topEntries[0].IsDir() {
		t.Fatalf("top entries = %v, %v", topEntries, err)
	}
	resEntries, err := os.ReadDir(filepath.Join(root, topEntries[0].Name()))
	if err != nil || len(resEntries) != fileCount {
		t.Fatalf("result entries = %d, want %d (err=%v)", len(resEntries), fileCount, err)
	}
	for _, entry := range resEntries {
		if strings.HasPrefix(entry.Name(), ".wsresume") || strings.HasSuffix(entry.Name(), ".anchor") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("unexpected sidecar artifact: %s", entry.Name())
		}
	}
}

func TestCompatibleNameRepairReusesMappingAcrossRestartWithoutJSONDump(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0x41)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	// A single-file transfer named .windshare-tmp at the destination container root
	// must receive a compatible spelling rather than colliding with WindShare's
	// private scratch directory, reuse that spelling across restart, and emit no
	// windshare-names-*.json file beside the user's output.
	fileID := testID16[catalog.FileID](0x43)
	art, err := receivecontract.NewSingleFileDirectoryTree(fileID, ".windshare-tmp", ".windshare-tmp")
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	f := testSingleFileMaterialization(t, session, intent, fileID, 0x44, ".windshare-tmp", 8)
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected transaction for repaired file")
	}
	if err := tx.WriteRange(ctx, 0, []byte("part")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reIntent, _ := reLookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	reFile := testSingleFileMaterialization(t, reSession, reIntent, fileID, 0x44, ".windshare-tmp", 8)
	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := reStart.Transaction()
	if !ok || len(durable.Ranges().Ranges()) != 1 || durable.Ranges().Ranges()[0].End != 4 {
		t.Fatalf("reopened ranges = %+v", durable.Ranges().Ranges())
	}
	if err := reTx.WriteRange(ctx, 4, []byte("done")); err != nil {
		t.Fatal(err)
	}
	if _, err := reTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 repaired file and no JSON dump, got %v (err=%v)", entries, err)
	}
	if strings.HasSuffix(entries[0].Name(), ".json") || entries[0].Name() == ControlDirName {
		t.Fatalf("unexpected entry name %q", entries[0].Name())
	}
	data, err := os.ReadFile(filepath.Join(root, entries[0].Name()))
	if err != nil || string(data) != "partdone" {
		t.Fatalf("repaired file content = %q, %v", data, err)
	}
}

func TestJournalCompactionAndRedownloadAndShareStopped(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0x51)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	// Force journal compaction by appending > journalCompactMinLines partial updates.
	fid := testID16[catalog.FileID](0x55)
	rev := testID16[content.FileRevision](0x56)
	for i := uint64(1); i <= uint64(journalCompactMinLines+10); i++ {
		if err := op.journal.recordPartial(partialEntry{
			FileID: fid, Path: "windshare-selection/big.bin", Revision: rev,
			ExactSize: 1000, PartName: "big.part", Ranges: []content.Range{{Offset: 0, End: i}},
			CheckpointGen: i,
		}, false); err != nil {
			t.Fatal(err)
		}
	}
	if op.journal.state.lineCount >= journalCompactMinLines {
		t.Fatalf("journal did not compact: lineCount=%d", op.journal.state.lineCount)
	}
	if err := op.RecordShareStopped(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Automatic lookup must report ErrShareStopped; Redownload must start fresh.
	check, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := check.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := check.SelectOperation(ctx, sel, receivecontract.AutomaticOperation()); !errors.Is(err, ErrShareStopped) {
		t.Fatalf("automatic select on share-stopped = %v, want ErrShareStopped", err)
	}
	_ = check.Close()

	redl, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer redl.Close()
	if err := redl.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	redlLookup, err := redl.SelectOperation(ctx, sel, receivecontract.RedownloadOperation())
	if err != nil || redlLookup.Kind() != LookupCreateReady {
		t.Fatalf("redownload lookup = (%v, %v), want LookupCreateReady", redlLookup.Kind(), err)
	}
}

func TestConcurrentAutomaticCreateRechecksSelectionWhilePreservingRedownload(t *testing.T) {
	ctx := context.Background()
	openBound := func(root string) *Authority {
		t.Helper()
		auth, err := New(Config{RootPath: root, CreateRoot: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.BindDestination(ctx); err != nil {
			_ = auth.Close()
			t.Fatal(err)
		}
		return auth
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("active rival blocks duplicate and keeps automatic lookup unique", func(t *testing.T) {
		root := t.TempDir()
		sel := testSelectionSpec(t, 0x71)
		first := openBound(root)
		second := openBound(root)
		lookupA, err := first.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupA.Kind() != LookupCreateReady {
			t.Fatalf("first select = (%v, %v)", lookupA.Kind(), err)
		}
		lookupB, err := second.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupB.Kind() != LookupCreateReady {
			t.Fatalf("second select = (%v, %v)", lookupB.Kind(), err)
		}
		opA, err := first.CreateOperation(ctx, lookupA, art)
		if err != nil {
			t.Fatal(err)
		}
		intentA, _ := opA.ReceiveIntent()
		if _, err := second.CreateOperation(ctx, lookupB, art); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("contending automatic create = %v, want ErrAlreadyRunning", err)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}

		third := openBound(root)
		defer third.Close()
		lookupC, err := third.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupC.Kind() != LookupReopened {
			t.Fatalf("third select = (%v, %v), want LookupReopened", lookupC.Kind(), err)
		}
		intentC, _ := lookupC.Operation().ReceiveIntent()
		if intentC.OperationID() != intentA.OperationID() {
			t.Fatalf("reopened operation ID = %x, want %x", intentC.OperationID().Bytes(), intentA.OperationID().Bytes())
		}
	})

	t.Run("closed rival is seamlessly reopened without creating duplicate", func(t *testing.T) {
		root := t.TempDir()
		sel := testSelectionSpec(t, 0x75)
		first := openBound(root)
		second := openBound(root)
		defer second.Close()
		lookupA, err := first.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupA.Kind() != LookupCreateReady {
			t.Fatalf("first select = (%v, %v)", lookupA.Kind(), err)
		}
		lookupB, err := second.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupB.Kind() != LookupCreateReady {
			t.Fatalf("second select = (%v, %v)", lookupB.Kind(), err)
		}
		opA, err := first.CreateOperation(ctx, lookupA, art)
		if err != nil {
			t.Fatal(err)
		}
		intentA, _ := opA.ReceiveIntent()
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		opB, err := second.CreateOperation(ctx, lookupB, art)
		if err != nil {
			t.Fatalf("reopen on create = %v", err)
		}
		intentB, _ := opB.ReceiveIntent()
		if intentB.OperationID() != intentA.OperationID() {
			t.Fatalf("create reopened operation ID = %x, want %x", intentB.OperationID().Bytes(), intentA.OperationID().Bytes())
		}
	})

	t.Run("stopped rival surfaces ErrShareStopped on create recheck", func(t *testing.T) {
		root := t.TempDir()
		sel := testSelectionSpec(t, 0x79)
		first := openBound(root)
		second := openBound(root)
		defer second.Close()
		lookupA, err := first.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupA.Kind() != LookupCreateReady {
			t.Fatalf("first select = (%v, %v)", lookupA.Kind(), err)
		}
		lookupB, err := second.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupB.Kind() != LookupCreateReady {
			t.Fatalf("second select = (%v, %v)", lookupB.Kind(), err)
		}
		opA, err := first.CreateOperation(ctx, lookupA, art)
		if err != nil {
			t.Fatal(err)
		}
		if err := opA.RecordShareStopped(ctx); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := second.CreateOperation(ctx, lookupB, art); !errors.Is(err, ErrShareStopped) {
			t.Fatalf("create after stopped rival = %v, want ErrShareStopped", err)
		}
	})

	t.Run("explicit redownload still creates an independent operation", func(t *testing.T) {
		root := t.TempDir()
		sel := testSelectionSpec(t, 0x7d)
		automatic := openBound(root)
		redownload := openBound(root)
		lookupA, err := automatic.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupA.Kind() != LookupCreateReady {
			t.Fatalf("automatic select = (%v, %v)", lookupA.Kind(), err)
		}
		lookupR, err := redownload.SelectOperation(ctx, sel, receivecontract.RedownloadOperation())
		if err != nil || lookupR.Kind() != LookupCreateReady {
			t.Fatalf("redownload select = (%v, %v)", lookupR.Kind(), err)
		}
		opA, err := automatic.CreateOperation(ctx, lookupA, art)
		if err != nil {
			t.Fatal(err)
		}
		opR, err := redownload.CreateOperation(ctx, lookupR, art)
		if err != nil {
			t.Fatalf("redownload create = %v", err)
		}
		intentA, _ := opA.ReceiveIntent()
		intentR, _ := opR.ReceiveIntent()
		if intentR.OperationID() == intentA.OperationID() {
			t.Fatal("redownload reused automatic operation ID")
		}
		if err := redownload.Close(); err != nil {
			t.Fatal(err)
		}
		if err := automatic.Close(); err != nil {
			t.Fatal(err)
		}

		third := openBound(root)
		defer third.Close()
		lookupC, err := third.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookupC.Kind() != LookupReopened {
			t.Fatalf("third automatic select = (%v, %v), want LookupReopened", lookupC.Kind(), err)
		}
		intentC, _ := lookupC.Operation().ReceiveIntent()
		if intentC.OperationID() != intentA.OperationID() {
			t.Fatalf("automatic continuation picked %x, want %x", intentC.OperationID().Bytes(), intentA.OperationID().Bytes())
		}
	})
}

func TestSmallFileAcceptClockAndSameSessionPauseReadmission(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, 0x61)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, 0x62)
	payload := []byte("alpha-beta-gamma-delta")
	file := testMaterializationFile(t, session, intent, 0x63, "piece.bin", uint64(len(payload)), rootAdm)

	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, durable0, ok := start.Transaction()
	if !ok || durable0.CheckpointGeneration() != 0 {
		t.Fatalf("initial start = (%v, gen=%d)", ok, durable0.CheckpointGeneration())
	}
	// Two consecutive small-file accepts must advance in-process generation strictly
	// while keeping persisted VerifiedRanges valid at generation 0 (zero per-chunk fsync).
	if err := tx.WriteRange(ctx, 0, payload[:6]); err != nil {
		t.Fatal(err)
	}
	p1, err := tx.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	persisted1, hasPersisted1 := p1.Persisted()
	if !hasPersisted1 || persisted1.CheckpointGeneration() != 0 || p1.Accepted().CheckpointGeneration() <= durable0.CheckpointGeneration() {
		t.Fatalf("first accept = (accGen=%d, hasPersisted=%v, persGen=%d)", p1.Accepted().CheckpointGeneration(), hasPersisted1, persisted1.CheckpointGeneration())
	}
	if err := tx.WriteRange(ctx, 6, payload[6:11]); err != nil {
		t.Fatal(err)
	}
	p2, err := tx.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Accepted().CheckpointGeneration() <= p1.Accepted().CheckpointGeneration() {
		t.Fatalf("second accept generation did not advance: %d <= %d", p2.Accepted().CheckpointGeneration(), p1.Accepted().CheckpointGeneration())
	}
	// Pause persists unpersisted bytes and keeps the file resumable in the SAME session.
	paused, err := tx.Pause(ctx)
	if err != nil || paused.Kind() != transfer.FilePaused {
		t.Fatalf("pause = (%v, %v)", paused.Kind(), err)
	}
	start2, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx2, durable2, ok := start2.Transaction()
	if !ok || len(durable2.Ranges().Ranges()) != 1 || durable2.Ranges().Ranges()[0] != (content.Range{Offset: 0, End: 11}) {
		t.Fatalf("readmitted start = (%v, ranges=%v)", ok, durable2.Ranges().Ranges())
	}
	if err := tx2.WriteRange(ctx, 11, payload[11:]); err != nil {
		t.Fatal(err)
	}
	p3, err := tx2.Accept(ctx)
	if err != nil || p3.Accepted().CheckpointGeneration() <= durable2.CheckpointGeneration() {
		t.Fatalf("readmitted accept = (%v, gen=%d <= %d)", err, p3.Accepted().CheckpointGeneration(), durable2.CheckpointGeneration())
	}
	settled, err := tx2.Commit(ctx)
	if err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%v, %v)", settled.Kind(), err)
	}
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	got, err := os.ReadFile(filepath.Join(root, res.PhysicalName(), "piece.bin"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published = (%q, %v)", got, err)
	}
}

func testID16[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var id T
	for i := range id {
		id[i] = seed + byte(i)
	}
	return id
}

func testSelectionSpec(t *testing.T, seed byte) transfer.SelectionSpec {
	t.Helper()
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := transfer.NewSelectionSpec(testID16[catalog.ShareInstance](seed), testID16[catalog.DirectoryID](seed+1), rules)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func testAdmitRoot(t *testing.T, session transfer.DirectTreeSession, intent transfer.ReceiveIntent, seed byte) transfer.DirectoryAdmission {
	t.Helper()
	req, err := transfer.NewDirectoryMaterializationRequest(
		intent,
		transfer.AuthenticatedSourceDirectory{
			DirectoryID: intent.SyntheticRoot(),
			Generation:  testID16[catalog.DirectoryGeneration](seed),
			SourcePath:  ordinaryoutput.EmptySourceCatalogPath(),
		},
		ordinaryoutput.SourceNodeSelected,
		transfer.MaterializedDirectoryClaim{},
	)
	if err != nil {
		t.Fatal(err)
	}
	adm, err := session.AdmitDirectory(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return adm
}

func testMaterializationFile(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seed byte,
	name string,
	size uint64,
	parent transfer.DirectoryAdmission,
) transfer.MaterializationFile {
	t.Helper()
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(),
		testID16[catalog.FileID](seed),
		testID16[content.FileRevision](seed+1),
		geom,
		catalog.ModifiedTime{},
	)
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
	relStr := name
	if parent.Path() != "" {
		relStr = parent.Path() + "/" + name
	}
	rel, err := transfer.NewMaterializationRootRelativePath(relStr)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(intent, sourcePath, rel, desc, session.SessionID(), fileParent)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func testMaterializationFileSeq(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	seq uint64,
	name string,
	size uint64,
	parent transfer.DirectoryAdmission,
) transfer.MaterializationFile {
	t.Helper()
	var fid catalog.FileID
	var rev content.FileRevision
	fid[0], fid[14], fid[15] = 0xf1, byte(seq>>8), byte(seq)
	rev[0], rev[14], rev[15] = 0xf2, byte(seq>>8), byte(seq)
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(intent.ShareInstance(), fid, rev, geom, catalog.ModifiedTime{})
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

func testSingleFileMaterialization(
	t *testing.T,
	session transfer.DirectTreeSession,
	intent transfer.ReceiveIntent,
	fileID catalog.FileID,
	revSeed byte,
	name string,
	size uint64,
) transfer.MaterializationFile {
	t.Helper()
	geom, err := content.NewFileGeometry(size, catalog.DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := content.NewFileRevisionDescriptor(
		intent.ShareInstance(), fileID, testID16[content.FileRevision](revSeed), geom, catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sourcePath, err := ordinaryoutput.NewSourceCatalogPath(name)
	if err != nil {
		t.Fatal(err)
	}
	fileParent, err := transfer.NewReferenceMaterializationFileParent(
		intent.SyntheticRoot(), testID16[catalog.DirectoryGeneration](revSeed+1), ordinaryoutput.EmptySourceCatalogPath(),
	)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := transfer.NewMaterializationRootRelativePath(name)
	if err != nil {
		t.Fatal(err)
	}
	file, err := transfer.NewMaterializationFile(intent, sourcePath, rel, desc, session.SessionID(), fileParent)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
