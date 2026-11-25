//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCopiedScratchDirCannotBypassActiveTaskLock(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	liveAuth, _, intentA := openActiveResultRootOp(t, root, 0x81, "photos")
	closedLive := false
	t.Cleanup(func() {
		if !closedLive {
			_ = liveAuth.Close()
		}
	})
	opIDA := intentA.OperationID()
	opIDB := distinctOperationID(opIDA)

	srcJournal := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()), journalFileName)
	rawJournal, err := os.ReadFile(srcJournal)
	if err != nil {
		t.Fatal(err)
	}
	// Append an unterminated trailing fragment so any premature O_RDWR truncate
	// or compaction during rejection is caught immediately.
	mismatchedBytes := append(slices.Clone(rawJournal), []byte("unterminated-torn-tail")...)
	dstDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDB.Bytes()))
	if err := os.Mkdir(dstDir, privateDirMode); err != nil {
		t.Fatal(err)
	}
	dstJournal := filepath.Join(dstDir, journalFileName)
	if err := os.WriteFile(dstJournal, mismatchedBytes, privateFileMode); err != nil {
		t.Fatal(err)
	}

	// Explicit selection by the copied directory's ID must refuse reopening
	// without touching state.log or opening ID_A's live resultDir.
	stream, err := diagnostics.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	byIDAuth, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
	if err != nil {
		t.Fatal(err)
	}
	if err := byIDAuth.BindDestination(ctx); err != nil {
		_ = byIDAuth.Close()
		t.Fatal(err)
	}
	byIDSel, err := receivecontract.SelectOperationID(opIDB)
	if err != nil {
		_ = byIDAuth.Close()
		t.Fatal(err)
	}
	byIDLookup, err := byIDAuth.SelectOperation(ctx, intentA.SelectionSpec(), byIDSel)
	_ = byIDAuth.Close()
	if err != nil || byIDLookup.Kind() != LookupNeedsAttention || byIDLookup.Attention() != AttentionOperation {
		t.Fatalf("SelectOperation(by_id copied dir) = (kind=%v attn=%q err=%v), want LookupNeedsAttention(%q)",
			byIDLookup.Kind(), byIDLookup.Attention(), err, AttentionOperation)
	}
	afterBytes, err := os.ReadFile(dstJournal)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, mismatchedBytes) {
		t.Fatalf("rejected mismatched journal was mutated: got len=%d, want len=%d", len(afterBytes), len(mismatchedBytes))
	}
	assertOperationIDMismatchTrace(t, stream, opIDB)

	// Automatic lookup ignores the mismatched copy and observes the live lock
	// on ID_A rather than locking ID_B or failing ambiguously.
	autoAuth := newBoundAuthority(t, root)
	autoLookup, err := autoAuth.SelectOperation(ctx, intentA.SelectionSpec(), receivecontract.AutomaticOperation())
	_ = autoAuth.Close()
	if err != nil || autoLookup.Kind() != LookupAlreadyRunning {
		t.Fatalf("SelectOperation(auto with live ID_A and copied ID_B) = (kind=%v err=%v), want LookupAlreadyRunning",
			autoLookup.Kind(), err)
	}

	// Once ID_A pauses, automatic lookup cleanly reopens valid ID_A despite
	// the stray mismatched copy.
	if err := liveAuth.Close(); err != nil {
		t.Fatal(err)
	}
	closedLive = true

	resumeAuth := newBoundAuthority(t, root)
	defer resumeAuth.Close()
	resumeLookup, err := resumeAuth.SelectOperation(ctx, intentA.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || resumeLookup.Kind() != LookupReopened || resumeLookup.Operation() == nil {
		t.Fatalf("SelectOperation(auto after ID_A paused) = (kind=%v err=%v), want LookupReopened",
			resumeLookup.Kind(), err)
	}
	reopenedIntent, ok := resumeLookup.Operation().ReceiveIntent()
	if !ok || reopenedIntent.OperationID() != opIDA {
		t.Fatalf("reopened operation ID = (%x, %v), want %x", reopenedIntent.OperationID(), ok, opIDA)
	}
}

func TestRenamedMismatchedScratchDirDoesNotBlockFreshOrUnrelatedDownloads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	selA, intentA, fileIDA := createPausedNamedSingleFileOp(t, root, 0x91, "movie.bin")
	opIDA := intentA.OperationID()
	opIDB := distinctOperationID(opIDA)

	oldDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()))
	newDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDB.Bytes()))
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(newDir, journalFileName)
	beforeBytes, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	// Explicit reopen by the renamed directory's ID fails closed without
	// modifying state.log.
	byIDAuth := newBoundAuthority(t, root)
	byIDSel, err := receivecontract.SelectOperationID(opIDB)
	if err != nil {
		_ = byIDAuth.Close()
		t.Fatal(err)
	}
	byIDLookup, err := byIDAuth.SelectOperation(ctx, selA, byIDSel)
	_ = byIDAuth.Close()
	if err != nil || byIDLookup.Kind() != LookupNeedsAttention || byIDLookup.Attention() != AttentionOperation {
		t.Fatalf("SelectOperation(by_id renamed dir) = (kind=%v attn=%q err=%v), want LookupNeedsAttention(%q)",
			byIDLookup.Kind(), byIDLookup.Attention(), err, AttentionOperation)
	}
	afterBytes, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(afterBytes, beforeBytes) {
		t.Fatalf("renamed mismatched journal altered: err=%v same=%v", err, bytes.Equal(afterBytes, beforeBytes))
	}

	// Automatic lookup ignores the unresumable mismatched directory and lets a
	// fresh download claim the unmaterialized filename rather than squatting it.
	freshAuth := newBoundAuthority(t, root)
	freshLookup, err := freshAuth.SelectOperation(ctx, selA, receivecontract.AutomaticOperation())
	if err != nil || freshLookup.Kind() != LookupCreateReady {
		_ = freshAuth.Close()
		t.Fatalf("SelectOperation(auto with renamed mismatched dir) = (kind=%v err=%v), want LookupCreateReady",
			freshLookup.Kind(), err)
	}
	artA, err := receivecontract.NewSingleFileDirectoryTree(fileIDA, "movie.bin", "movie.bin")
	if err != nil {
		_ = freshAuth.Close()
		t.Fatal(err)
	}
	freshOp, err := freshAuth.CreateOperation(ctx, freshLookup, artA)
	if err != nil {
		_ = freshAuth.Close()
		t.Fatalf("CreateOperation(fresh after renamed mismatch) = %v", err)
	}
	freshIntent, _ := freshOp.ReceiveIntent()
	freshRes, _ := freshIntent.MaterializationPlan().DestinationReservation()
	if freshRes.PhysicalName() != "movie.bin" {
		_ = freshAuth.Close()
		t.Fatalf("mismatched dir squatted top-level name: got %q, want %q", freshRes.PhysicalName(), "movie.bin")
	}
	if err := freshAuth.Close(); err != nil {
		t.Fatal(err)
	}

	// An unrelated download into the same destination stays isolated and succeeds.
	otherAuth, _, otherIntent, _ := openActiveNamedSingleFileOp(t, root, 0xa5, "notes.txt")
	if err := otherAuth.Close(); err != nil {
		t.Fatal(err)
	}
	if otherRes, _ := otherIntent.MaterializationPlan().DestinationReservation(); otherRes.PhysicalName() != "notes.txt" {
		t.Fatalf("unrelated download physical name = %q, want %q", otherRes.PhysicalName(), "notes.txt")
	}

	// Recovery inventory stays consistent and flags the stray mismatched dir.
	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status() != RecoveryListNeedsAttention || !inv.UnknownEntries() || len(inv.Summaries()) != 2 {
		t.Fatalf("ListOperations = (status=%v unknown=%t count=%d), want needs-attention with unknown entry and 2 valid ops",
			inv.Status(), inv.UnknownEntries(), len(inv.Summaries()))
	}
}

func TestJournalRejectsDuplicateMetaIntentRebindingOperationID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	_, intentA, _ := createPausedNamedSingleFileOp(t, root, 0xb1, "alpha.bin")
	selB, intentB, _ := createPausedNamedSingleFileOp(t, root, 0xb5, "beta.bin")
	opIDA := intentA.OperationID()
	opIDB := intentB.OperationID()

	journalAPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()), journalFileName)
	journalBDirPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDB.Bytes()))
	journalBPath := filepath.Join(journalBDirPath, journalFileName)

	metaLineA, err := os.ReadFile(journalAPath)
	if err != nil {
		t.Fatal(err)
	}
	firstNewline := bytes.IndexByte(metaLineA, '\n')
	if firstNewline < 0 {
		t.Fatal("expected newline-terminated meta line in journal A")
	}
	metaLineA = metaLineA[:firstNewline+1]

	// Append ID_A's intent-bearing meta record after ID_B's valid header so a
	// header-only scan sees ID_B while a naive full replay would rebind to ID_A.
	f, err := os.OpenFile(journalBPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(metaLineA); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dirB, err := openDirHandle(journalBDirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dirB.Close()
	if _, _, err := readJournalState(dirB); !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalState on duplicate meta intent = %v, want errJournalCorrupt", err)
	}

	auth := newBoundAuthority(t, root)
	defer auth.Close()
	byIDB, err := receivecontract.SelectOperationID(opIDB)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := auth.SelectOperation(ctx, selB, byIDB)
	if err != nil || lookup.Kind() != LookupNeedsAttention || lookup.Attention() != AttentionOperation {
		t.Fatalf("SelectOperation(by_id duplicate meta intent) = (kind=%v attn=%q err=%v), want LookupNeedsAttention(%q)",
			lookup.Kind(), lookup.Attention(), err, AttentionOperation)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Status() != RecoveryListNeedsAttention || !inv.UnknownEntries() || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].OperationID() != opIDA {
		t.Fatalf("ListOperations = (status=%v unknown=%t summaries=%+v), want unknown entry for ID_B and valid ID_A",
			inv.Status(), inv.UnknownEntries(), inv.Summaries())
	}
}

func distinctOperationID(id receivecontract.OperationID) receivecontract.OperationID {
	raw := slices.Clone(id.Bytes())
	raw[0] ^= 0xff
	if raw[0] == 0 {
		raw[0] = 1
	}
	other, err := receivecontract.OperationIDFromBytes(raw)
	if err != nil {
		panic(err)
	}
	return other
}

func assertOperationIDMismatchTrace(t *testing.T, stream *diagnostics.Stream, wantOpID receivecontract.OperationID) {
	t.Helper()
	wantIDText := diagnostics.Identity(wantOpID.Bytes()).Text()
	for {
		select {
		case rec := <-stream.Records():
			p := rec.Payload()
			if p.Get("runtime_operation").Text() == "select_operation" &&
				p.Get("runtime_decision").Text() == "operation_id_mismatch" {
				if got := rec.Correlation().Get("receive_operation_id").Text(); got != wantIDText {
					t.Fatalf("operation_id_mismatch trace operation_id = %q, want %q", got, wantIDText)
				}
				return
			}
		default:
			t.Fatal("expected filesystem_output trace with runtime_decision=operation_id_mismatch")
		}
	}
}
