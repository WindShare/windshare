//go:build windows

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"golang.org/x/sys/windows"
)

func openWithoutDeleteShare(t *testing.T, path string) windows.Handle {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Omit FILE_SHARE_DELETE to reproduce Windows scanners/tools blocking unlink.
	h, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q) without FILE_SHARE_DELETE: %v", path, err)
	}
	return h
}

func openDirWithoutDeleteShare(t *testing.T, path string) windows.Handle {
	t.Helper()
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Hold a directory handle without FILE_SHARE_DELETE so children (including
	// state.log) unlink normally while the final rmdir fails with SHARING_VIOLATION.
	h, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile dir(%q) without FILE_SHARE_DELETE: %v", path, err)
	}
	return h
}

func findSinglePartFile(t *testing.T, opDir string) string {
	t.Helper()
	entries, err := os.ReadDir(opDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".part") {
			return filepath.Join(opDir, entry.Name())
		}
	}
	t.Fatalf("no .part file found in %q", opDir)
	return ""
}

func TestWindowsDiscardPreservesJournalWhenPartLockedWithoutDeleteShare(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0xa1)
	_, intentB, _ := createPausedSingleFileOp(t, root, 0xa5)
	opIDA := intentA.OperationID()
	opIDB := intentB.OperationID()
	opDirA := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()))
	partPath := findSinglePartFile(t, opDirA)

	lockHandle := openWithoutDeleteShare(t, partPath)
	closed := false
	closeLock := func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(lockHandle)
		}
	}
	t.Cleanup(closeLock)

	summary, err := DiscardOperation(ctx, root, opIDA)
	if err == nil || summary.State() != RecoveryOperationCleanupPending ||
		summary.NeedsAttentionReason() != AttentionCleanup {
		t.Fatalf("DiscardOperation while .part locked = (%+v, %v), want cleanup-pending with error", summary, err)
	}
	if _, statErr := os.Stat(filepath.Join(opDirA, journalFileName)); statErr != nil {
		t.Fatalf("state.log was deleted while .part remained locked: %v", statErr)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatalf("ListOperations = %v", err)
	}
	if inv.Status() != RecoveryListReady || inv.UnknownEntries() || len(inv.Summaries()) != 2 {
		t.Fatalf("inventory while locked = (status=%v unknown=%t count=%d)",
			inv.Status(), inv.UnknownEntries(), len(inv.Summaries()))
	}
	var genAfterFirst uint64
	for _, s := range inv.Summaries() {
		if s.OperationID() == opIDA {
			if s.State() != RecoveryOperationCleanupPending ||
				s.NeedsAttentionReason() != AttentionCleanup || len(s.Items()) != 0 {
				t.Fatalf("opA summary = %+v, want cleanup-pending", s)
			}
			genAfterFirst = s.StateGeneration()
		}
	}
	if genAfterFirst == 0 {
		t.Fatal("opA disappeared from ListOperations after failed cleanup")
	}

	// Sibling operations in the same destination remain discardable.
	if sibSummary, sibErr := DiscardOperation(ctx, root, opIDB); sibErr != nil ||
		sibSummary.State() != RecoveryOperationDiscarded {
		t.Fatalf("sibling DiscardOperation = (%+v, %v), want discarded", sibSummary, sibErr)
	}

	// Reopening the half-discarded task must fail closed rather than resurrect it.
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopen.BindDestination(ctx); err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	lookup, lookupErr := reopen.SelectOperation(ctx, selA, receivecontract.AutomaticOperation())
	_ = reopen.Close()
	if lookupErr != nil || lookup.Kind() != LookupNeedsAttention || lookup.Attention() != AttentionCleanup {
		t.Fatalf("SelectOperation on cleanup-pending opA = (kind=%v attn=%q err=%v)",
			lookup.Kind(), lookup.Attention(), lookupErr)
	}

	// Retrying discard while still locked stays cleanup-pending without churning StateGen.
	if retrySummary, retryErr := DiscardOperation(ctx, root, opIDA); retryErr == nil ||
		retrySummary.State() != RecoveryOperationCleanupPending {
		t.Fatalf("second DiscardOperation while still locked = (%+v, %v)", retrySummary, retryErr)
	}
	inv, err = ListOperations(ctx, root)
	if err != nil || inv.UnknownEntries() || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].StateGeneration() != genAfterFirst {
		t.Fatalf("inventory after second locked attempt = (%+v, %v), want stable gen=%d", inv, err, genAfterFirst)
	}

	// Release the handle: task stays listable and retry discard finishes cleanly.
	closeLock()
	inv, err = ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() ||
		len(inv.Summaries()) != 1 || inv.Summaries()[0].OperationID() != opIDA ||
		inv.Summaries()[0].State() != RecoveryOperationCleanupPending {
		t.Fatalf("inventory after handle release = (%+v, %v)", inv, err)
	}
	finalSummary, finalErr := DiscardOperation(ctx, root, opIDA)
	if finalErr != nil || finalSummary.State() != RecoveryOperationDiscarded {
		t.Fatalf("retry DiscardOperation after handle release = (%+v, %v)", finalSummary, finalErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived retry discard: %v", statErr)
	}
}

func TestWindowsInteractiveDiscardAndFinalizePreserveJournalWhenLocked(t *testing.T) {
	ctx := context.Background()

	t.Run("OperationDiscard", func(t *testing.T) {
		root := t.TempDir()
		auth, op, intent := openActiveSingleFileOp(t, root, 0xb1)
		defer auth.Close()
		session, err := auth.OpenOperation(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
		fileID := testID16[catalog.FileID](0xb3)
		f := testSingleFileMaterialization(t, session, intent, fileID, 0xb4, retirementFileName, uint64(len(retirementFullPayload)))
		start, err := session.BeginFile(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected transaction")
		}
		if err := tx.WriteRange(ctx, 0, []byte(retirementFirstHalf)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Pause(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}

		opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
		partPath := findSinglePartFile(t, opDir)
		lockHandle := openWithoutDeleteShare(t, partPath)
		closed := false
		closeLock := func() {
			if !closed {
				closed = true
				_ = windows.CloseHandle(lockHandle)
			}
		}
		t.Cleanup(closeLock)

		if err := op.Discard(ctx); err == nil {
			t.Fatal("Operation.Discard succeeded while .part was locked without delete share")
		}
		inv, err := ListOperations(ctx, root)
		if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() ||
			len(inv.Summaries()) != 1 || inv.Summaries()[0].State() != RecoveryOperationCleanupPending {
			t.Fatalf("ListOperations after locked Operation.Discard = (%+v, %v)", inv, err)
		}
		closeLock()
		if summary, err := DiscardOperation(ctx, root, intent.OperationID()); err != nil ||
			summary.State() != RecoveryOperationDiscarded {
			t.Fatalf("retry DiscardOperation = (%+v, %v)", summary, err)
		}
	})

	t.Run("FinalizeTreeSuccess", func(t *testing.T) {
		root := t.TempDir()
		auth, op, intent := openActiveSingleFileOp(t, root, 0xb8)
		defer auth.Close()
		session, err := auth.OpenOperation(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
		fileID := testID16[catalog.FileID](0xba)
		f := testSingleFileMaterialization(t, session, intent, fileID, 0xbb, retirementFileName, uint64(len(retirementFullPayload)))
		start, err := session.BeginFile(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected transaction")
		}
		if err := tx.WriteRange(ctx, 0, []byte(retirementFullPayload)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}

		opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
		extra := filepath.Join(opDir, "leftover.part")
		if err := os.WriteFile(extra, []byte("scan-lock"), 0o600); err != nil {
			t.Fatal(err)
		}
		lockHandle := openWithoutDeleteShare(t, extra)
		closed := false
		closeLock := func() {
			if !closed {
				closed = true
				_ = windows.CloseHandle(lockHandle)
			}
		}
		t.Cleanup(closeLock)

		if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err == nil {
			t.Fatal("FinalizeTree(Success) succeeded while scratch file was locked")
		}
		inv, err := ListOperations(ctx, root)
		if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() ||
			len(inv.Summaries()) != 1 || inv.Summaries()[0].State() != RecoveryOperationCleanupPending {
			t.Fatalf("ListOperations after locked FinalizeTree = (%+v, %v)", inv, err)
		}
		closeLock()
		if summary, err := DiscardOperation(ctx, root, intent.OperationID()); err != nil ||
			summary.State() != RecoveryOperationDiscarded {
			t.Fatalf("retry DiscardOperation after FinalizeTree = (%+v, %v)", summary, err)
		}
		got, err := os.ReadFile(filepath.Join(root, retirementFileName))
		if err != nil || string(got) != retirementFullPayload {
			t.Fatalf("published file after cleanup retry = (%q, %v)", got, err)
		}
	})
}

func TestWindowsDiscardWhenJournalLockedPreservesIdentityForRetry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0xc2)
	opID := intent.OperationID()
	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))
	journalPath := filepath.Join(opDir, journalFileName)

	lockHandle := openWithoutDeleteShare(t, journalPath)
	closed := false
	closeLock := func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(lockHandle)
		}
	}
	t.Cleanup(closeLock)

	summary, err := DiscardOperation(ctx, root, opID)
	if err == nil || summary.State() != RecoveryOperationCleanupPending {
		t.Fatalf("DiscardOperation while state.log locked = (%+v, %v)", summary, err)
	}
	closeLock()

	inv, err := ListOperations(ctx, root)
	if err != nil || inv.UnknownEntries() || len(inv.Summaries()) != 1 || inv.Summaries()[0].OperationID() != opID {
		t.Fatalf("ListOperations after state.log unlocked = (%+v, %v)", inv, err)
	}
	finalSummary, finalErr := DiscardOperation(ctx, root, opID)
	if finalErr != nil || finalSummary.State() != RecoveryOperationDiscarded {
		t.Fatalf("retry DiscardOperation after state.log unlocked = (%+v, %v)", finalSummary, finalErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived retry discard: %v", statErr)
	}
}

func TestWindowsDiscardWhenOpDirLockedWithoutDeleteShareRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0xc8)
	_, intentB, _ := createPausedSingleFileOp(t, root, 0xcc)
	opIDA := intentA.OperationID()
	opIDB := intentB.OperationID()
	opDirA := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()))

	lockHandle := openDirWithoutDeleteShare(t, opDirA)
	closed := false
	closeLock := func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(lockHandle)
		}
	}
	t.Cleanup(closeLock)

	summary, err := DiscardOperation(ctx, root, opIDA)
	if err == nil || summary.State() != RecoveryOperationCleanupPending ||
		summary.NeedsAttentionReason() != AttentionCleanup {
		t.Fatalf("DiscardOperation while opDir locked = (%+v, %v), want cleanup-pending with error", summary, err)
	}
	entries, readErr := os.ReadDir(opDirA)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("locked opDirA after failed rmdir = (entries=%v err=%v), want empty dir", entries, readErr)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatalf("ListOperations = %v", err)
	}
	if inv.Status() != RecoveryListReady || inv.UnknownEntries() || len(inv.Summaries()) != 2 {
		t.Fatalf("inventory while opDir locked = (status=%v unknown=%t count=%d)",
			inv.Status(), inv.UnknownEntries(), len(inv.Summaries()))
	}
	foundA := false
	for _, s := range inv.Summaries() {
		if s.OperationID() == opIDA {
			foundA = true
			if s.State() != RecoveryOperationCleanupPending ||
				s.NeedsAttentionReason() != AttentionCleanup || len(s.Items()) != 0 {
				t.Fatalf("opA summary = %+v, want cleanup-pending", s)
			}
		}
	}
	if !foundA {
		t.Fatal("opA disappeared from ListOperations after locked rmdir")
	}

	// Sibling operations in the same destination stay discardable.
	if sibSummary, sibErr := DiscardOperation(ctx, root, opIDB); sibErr != nil ||
		sibSummary.State() != RecoveryOperationDiscarded {
		t.Fatalf("sibling DiscardOperation = (%+v, %v), want discarded", sibSummary, sibErr)
	}

	// Explicit reopen by ID must fail closed as cleanup-pending without recreating task.lock.
	byID, err := receivecontract.SelectOperationID(opIDA)
	if err != nil {
		t.Fatal(err)
	}
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopen.BindDestination(ctx); err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	lookup, lookupErr := reopen.SelectOperation(ctx, selA, byID)
	_ = reopen.Close()
	if lookupErr != nil || lookup.Kind() != LookupNeedsAttention || lookup.Attention() != AttentionCleanup {
		t.Fatalf("SelectOperation(by_id) on empty locked opDir = (kind=%v attn=%q err=%v)",
			lookup.Kind(), lookup.Attention(), lookupErr)
	}

	// Retrying discard while still locked stays cleanup-pending and non-unknown.
	if retrySummary, retryErr := DiscardOperation(ctx, root, opIDA); retryErr == nil ||
		retrySummary.State() != RecoveryOperationCleanupPending {
		t.Fatalf("second DiscardOperation while opDir locked = (%+v, %v)", retrySummary, retryErr)
	}
	inv, err = ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() || len(inv.Summaries()) != 1 {
		t.Fatalf("inventory after second locked attempt = (%+v, %v)", inv, err)
	}

	closeLock()
	finalSummary, finalErr := DiscardOperation(ctx, root, opIDA)
	if finalErr != nil || finalSummary.State() != RecoveryOperationDiscarded {
		t.Fatalf("retry DiscardOperation after opDir unlocked = (%+v, %v)", finalSummary, finalErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived retry discard: %v", statErr)
	}
}

func TestWindowsFinalizeTreeSuccessWhenOpDirLockedRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xd1)
	defer auth.Close()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	fileID := testID16[catalog.FileID](0xd3)
	f := testSingleFileMaterialization(t, session, intent, fileID, 0xd4, retirementFileName, uint64(len(retirementFullPayload)))
	start, err := session.BeginFile(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected transaction")
	}
	if err := tx.WriteRange(ctx, 0, []byte(retirementFullPayload)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	lockHandle := openDirWithoutDeleteShare(t, opDir)
	closed := false
	closeLock := func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(lockHandle)
		}
	}
	t.Cleanup(closeLock)

	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err == nil {
		t.Fatal("FinalizeTree(Success) succeeded while opDir was locked without delete share")
	}
	inv, err := ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() ||
		len(inv.Summaries()) != 1 || inv.Summaries()[0].State() != RecoveryOperationCleanupPending {
		t.Fatalf("ListOperations after locked FinalizeTree opDir = (%+v, %v)", inv, err)
	}
	closeLock()
	if summary, err := DiscardOperation(ctx, root, intent.OperationID()); err != nil ||
		summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("retry DiscardOperation after locked FinalizeTree = (%+v, %v)", summary, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived retry discard: %v", statErr)
	}
	got, err := os.ReadFile(filepath.Join(root, retirementFileName))
	if err != nil || string(got) != retirementFullPayload {
		t.Fatalf("published file after cleanup retry = (%q, %v)", got, err)
	}
}
