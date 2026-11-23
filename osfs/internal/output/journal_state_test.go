//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReopenTruncatesValidTrailingRecordMissingNewline(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const (
		seed      byte   = 0xc3
		fileName         = "tail.bin"
		partSize  uint64 = 4096
		totalSize uint64 = 3 * LargeCheckpointBytes
	)
	chunk := bytes.Repeat([]byte{0x3c}, int(partSize))

	auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)
	intent, _ := op.ReceiveIntent()
	if err := tx.WriteRange(ctx, 0, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.WriteRange(ctx, partSize, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Checkpoint(ctx); err != nil {
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

	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	journalPath := filepath.Join(opPath, journalFileName)
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || before[len(before)-1] != '\n' {
		t.Fatalf("expected newline-terminated journal, got len=%d", len(before))
	}

	// Simulate a crash that dropped only the final '\n' of the latest valid record.
	if err := os.Truncate(journalPath, int64(len(before)-1)); err != nil {
		t.Fatal(err)
	}

	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	state, validBytes, err := readJournalState(opDir)
	_ = opDir.Close()
	if err != nil {
		t.Fatalf("readJournalState on missing trailing newline = %v", err)
	}
	if !state.tornTail {
		t.Fatal("expected tornTail=true when final record lacks trailing newline")
	}
	if validBytes == 0 || before[validBytes-1] != '\n' {
		t.Fatalf("validBytes=%d does not end on a newline boundary", validBytes)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].State() != RecoveryOperationResumable {
		t.Fatalf("inventory before first reopen = (%+v, %v), want ready+resumable", inv, err)
	}

	// First reopen truncates the uncommitted tail, then appends a new record on normal pause.
	firstReopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstReopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup1, err := firstReopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup1.Kind() != LookupReopened {
		t.Fatalf("first reopen = (%v, %v), want LookupReopened", lookup1.Kind(), err)
	}
	reIntent, _ := lookup1.Operation().ReceiveIntent()
	reSession, err := firstReopen.OpenOperation(ctx, lookup1.Operation())
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, reSession, reIntent, seed+1)
	file := testMaterializationFile(t, reSession, reIntent, seed+2, fileName, totalSize, rootAdm)
	start, err := reSession.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active transaction on first reopen")
	}
	if got := rangeTotalBytes(durable.Ranges().Ranges()); got != partSize {
		t.Fatalf("reusable bytes after truncating unterminated tail = %d, want %d", got, partSize)
	}
	if err := reTx.WriteRange(ctx, partSize, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := reTx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reSession.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := firstReopen.Close(); err != nil {
		t.Fatal(err)
	}

	// Second reopen must stay healthy and preserve the newly persisted progress instead of NeedsAttention.
	inv2, err := ListOperations(ctx, root)
	if err != nil || inv2.Status() != RecoveryListReady || len(inv2.Summaries()) != 1 ||
		inv2.Summaries()[0].State() != RecoveryOperationResumable {
		t.Fatalf("inventory after first reopen pause = (%+v, %v), want ready+resumable", inv2, err)
	}
	reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
	if got := rangeTotalBytes(reopened); got != 2*partSize {
		t.Fatalf("second reopen reusable bytes = %d, want %d", got, 2*partSize)
	}
}

func TestReadJournalStateRejectsUnterminatedSoleMetaHeader(t *testing.T) {
	root := t.TempDir()
	auth, _, intent := openActiveSingleFileOp(t, root, 0xc9)
	opID := intent.OperationID()
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
	journalPath := filepath.Join(opPath, journalFileName)
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := bytes.TrimSuffix(raw, []byte("\n"))
	if len(trimmed) == len(raw) {
		t.Fatal("expected single-line meta journal to end with newline")
	}
	if err := os.WriteFile(journalPath, trimmed, privateFileMode); err != nil {
		t.Fatal(err)
	}

	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opDir.Close()

	if _, _, err := readJournalState(opDir); !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalState on unterminated sole meta = %v, want errJournalCorrupt", err)
	}
	if _, err := readJournalHeader(opDir); !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalHeader on unterminated sole meta = %v, want errJournalCorrupt", err)
	}

	// Reopening via explicit ID surfaces NeedsAttention rather than silently appending to an incomplete header.
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(context.Background()); err != nil {
		t.Fatal(err)
	}
	byID, err := receivecontract.SelectOperationID(opID)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := reopen.SelectOperation(context.Background(), intent.SelectionSpec(), byID)
	if err != nil || lookup.Kind() != LookupNeedsAttention || lookup.Attention() != AttentionOperation {
		t.Fatalf("select unterminated meta op = (kind=%v, attn=%q, err=%v), want LookupNeedsAttention(%q)",
			lookup.Kind(), lookup.Attention(), err, AttentionOperation)
	}
}
