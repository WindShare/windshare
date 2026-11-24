//go:build windows || linux || darwin

package output

import (
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCleanupPendingLifecycleListedWithoutRegistryUnknownAndBlocksReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0x91)
	_, intentB, _ := createPausedSingleFileOp(t, root, 0x95)
	opIDA := intentA.OperationID()
	opIDB := intentB.OperationID()

	rootDir, _, err := OpenContainer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir, err := openChildDir(rootDir, ControlDirName)
	if err != nil {
		_ = rootDir.Close()
		t.Fatal(err)
	}
	markOperationCleanupPending(ctrlDir, hex.EncodeToString(opIDA.Bytes()))
	// Repeated marking must be idempotent without churning state generations.
	markOperationCleanupPending(ctrlDir, hex.EncodeToString(opIDA.Bytes()))
	if err := errors.Join(ctrlDir.Close(), rootDir.Close()); err != nil {
		t.Fatal(err)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatalf("ListOperations = %v", err)
	}
	if inv.Status() != RecoveryListReady || inv.UnknownEntries() || len(inv.Summaries()) != 2 {
		t.Fatalf("inventory = (status=%v unknown=%t count=%d)", inv.Status(), inv.UnknownEntries(), len(inv.Summaries()))
	}
	var gotA, gotB RecoverySummary
	for _, s := range inv.Summaries() {
		switch s.OperationID() {
		case opIDA:
			gotA = s
		case opIDB:
			gotB = s
		}
	}
	if !gotA.Valid() || gotA.State() != RecoveryOperationCleanupPending ||
		gotA.NeedsAttentionReason() != AttentionCleanup || len(gotA.Items()) != 0 {
		t.Fatalf("opA summary = %+v, want cleanup-pending with cleanup-uncertain and no items", gotA)
	}
	if !gotB.Valid() || gotB.State() != RecoveryOperationResumable {
		t.Fatalf("opB summary = %+v, want resumable", gotB)
	}

	// Neither automatic nor explicit-ID lookup may resurrect a cleanup-pending operation.
	byID, err := receivecontract.SelectOperationID(opIDA)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		selector receivecontract.OperationSelection
	}{
		{name: "automatic", selector: receivecontract.AutomaticOperation()},
		{name: "by_id", selector: byID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := New(Config{RootPath: root})
			if err != nil {
				t.Fatal(err)
			}
			defer auth.Close()
			if err := auth.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			lookup, err := auth.SelectOperation(ctx, selA, tc.selector)
			if err != nil || lookup.Kind() != LookupNeedsAttention || lookup.Attention() != AttentionCleanup {
				t.Fatalf("SelectOperation(%s) = (kind=%v attn=%q err=%v), want LookupNeedsAttention(%q)",
					tc.name, lookup.Kind(), lookup.Attention(), err, AttentionCleanup)
			}
		})
	}

	if summary, err := DiscardOperation(ctx, root, opIDB); err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation(opB) = (%+v, %v)", summary, err)
	}
	if summary, err := DiscardOperation(ctx, root, opIDA); err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation(opA) = (%+v, %v)", summary, err)
	}
	if _, err := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("control dir survived final cleanup: %v", err)
	}
}

func TestRemoveOperationDirectoryDefersJournalOnUnixChildFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX read-only directory failure test requires non-root Unix")
	}
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0x99)
	opID := intent.OperationID()
	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))

	// Block child deletion with a read-only nested directory so state.log must survive.
	lockedDir := filepath.Join(opDir, "locked-sub")
	if err := os.Mkdir(lockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockedDir, "stuck.part"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o700) })

	summary, err := DiscardOperation(ctx, root, opID)
	if err == nil || summary.State() != RecoveryOperationCleanupPending || summary.NeedsAttentionReason() != AttentionCleanup {
		t.Fatalf("first DiscardOperation = (%+v, %v), want cleanup-pending with error", summary, err)
	}
	if _, statErr := os.Stat(filepath.Join(opDir, journalFileName)); statErr != nil {
		t.Fatalf("state.log was removed before stuck child cleared: %v", statErr)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil || inv.Status() != RecoveryListReady || inv.UnknownEntries() || len(inv.Summaries()) != 1 ||
		inv.Summaries()[0].State() != RecoveryOperationCleanupPending {
		t.Fatalf("ListOperations after failed cleanup = (%+v, %v)", inv, err)
	}

	if err := os.Chmod(lockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	summary, err = DiscardOperation(ctx, root, opID)
	if err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("retry DiscardOperation = (%+v, %v), want discarded", summary, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived retry discard: %v", statErr)
	}
}
