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

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
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

func TestEmptyOperationDirListedAsCleanupPendingAndStaysRetryable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	selA, intentA, _ := createPausedSingleFileOp(t, root, 0x9a)
	_, intentB, _ := createPausedSingleFileOp(t, root, 0x9e)
	opIDA := intentA.OperationID()
	opIDB := intentB.OperationID()
	opDirA := filepath.Join(root, ControlDirName, hex.EncodeToString(opIDA.Bytes()))

	// Simulate final rmdir failing or crashing right after state.log was unlinked.
	entries, err := os.ReadDir(opDirA)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(opDirA, entry.Name())); err != nil {
			t.Fatal(err)
		}
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
		t.Fatalf("empty opA summary = %+v, want cleanup-pending with cleanup-uncertain", gotA)
	}
	if !gotB.Valid() || gotB.State() != RecoveryOperationResumable {
		t.Fatalf("sibling opB summary = %+v, want resumable", gotB)
	}
	if _, statErr := os.Stat(opDirA); statErr != nil {
		t.Fatalf("read-only ListOperations mutated empty opDirA: %v", statErr)
	}

	// Explicit reopen by ID must refuse resurrection without creating task.lock inside opDirA.
	byID, err := receivecontract.SelectOperationID(opIDA)
	if err != nil {
		t.Fatal(err)
	}
	authByID, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer authByID.Close()
	if err := authByID.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupByID, err := authByID.SelectOperation(ctx, selA, byID)
	if err != nil || lookupByID.Kind() != LookupNeedsAttention || lookupByID.Attention() != AttentionCleanup {
		t.Fatalf("SelectOperation(by_id) = (kind=%v attn=%q err=%v), want LookupNeedsAttention(%q)",
			lookupByID.Kind(), lookupByID.Attention(), err, AttentionCleanup)
	}
	afterEntries, err := os.ReadDir(opDirA)
	if err != nil || len(afterEntries) != 0 {
		t.Fatalf("SelectOperation(by_id) mutated empty opDirA: entries=%v err=%v", afterEntries, err)
	}

	// Automatic selection sees no journal left and stays free to create a fresh download.
	authAuto, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer authAuto.Close()
	if err := authAuto.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookupAuto, err := authAuto.SelectOperation(ctx, selA, receivecontract.AutomaticOperation())
	if err != nil || lookupAuto.Kind() != LookupCreateReady {
		t.Fatalf("SelectOperation(automatic) = (kind=%v err=%v), want LookupCreateReady", lookupAuto.Kind(), err)
	}

	if summary, err := DiscardOperation(ctx, root, opIDB); err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation(opB) = (%+v, %v)", summary, err)
	}
	if summary, err := DiscardOperation(ctx, root, opIDA); err != nil || summary.State() != RecoveryOperationDiscarded {
		t.Fatalf("DiscardOperation(opA) = (%+v, %v)", summary, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ControlDirName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("control dir survived empty opA discard: %v", statErr)
	}
}

func TestNonEmptyOperationDirWithoutJournalRemainsUnknown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	_, intent, _ := createPausedSingleFileOp(t, root, 0x9f)
	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	if err := os.Remove(filepath.Join(opDir, journalFileName)); err != nil {
		t.Fatal(err)
	}

	inv, err := ListOperations(ctx, root)
	if err != nil {
		t.Fatalf("ListOperations = %v", err)
	}
	if inv.Status() != RecoveryListNeedsAttention || !inv.UnknownEntries() || len(inv.Summaries()) != 0 {
		t.Fatalf("inventory for non-empty dir missing state.log = (status=%v unknown=%t count=%d), want unknown",
			inv.Status(), inv.UnknownEntries(), len(inv.Summaries()))
	}
}

func TestSettleCreateRecheckLookupLockedTranslatesClaimAndReopenOutcomes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sel, intent, _ := createPausedSingleFileOp(t, root, 0xa1)
	opID := intent.OperationID()
	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(opID.Bytes()))

	// Empty the operation directory so claimOpUnderDirLock returns LookupNeedsAttention(AttentionCleanup).
	entries, err := os.ReadDir(opDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(opDir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("empty dir claim translates to AttentionCleanup", func(t *testing.T) {
		stream, err := diagnostics.NewStream(16, nil)
		if err != nil {
			t.Fatal(err)
		}
		auth, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
		if err != nil {
			t.Fatal(err)
		}
		defer auth.Close()
		if err := auth.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		auth.admission = &pendingAdmission{selection: sel}
		auth.stage = stageLookupHeld

		ctrlDir, err := openChildDir(auth.root, ControlDirName)
		if err != nil {
			t.Fatal(err)
		}
		defer ctrlDir.Close()

		claimed, early, err := auth.claimOpUnderDirLock(ctx, ctrlDir, opID, false)
		if err != nil || claimed.taskLock != nil || early.kind != LookupNeedsAttention || early.attention != AttentionCleanup {
			t.Fatalf("claimOpUnderDirLock(empty) = (claimed=%+v early=%+v err=%v), want LookupNeedsAttention(%q)",
				claimed, early, err, AttentionCleanup)
		}

		settledErr := auth.settleCreateRecheckLookupLocked(early)
		attn, ok := errors.AsType[*AttentionError](settledErr)
		if !ok || attn == nil || attn.Reason != AttentionCleanup {
			t.Fatalf("settleCreateRecheckLookupLocked = %v, want *AttentionError(%q)", settledErr, AttentionCleanup)
		}
		if !isAdmissionOutcomeError(settledErr) {
			t.Fatalf("AttentionError must bypass FailureCreate diagnostic wrapping")
		}
		if auth.admission != nil || auth.stage != stageStopped {
			t.Fatalf("authority state = (admission=%v stage=%v), want (nil, stageStopped)", auth.admission, auth.stage)
		}
		assertCreateOperationTraceDecision(t, stream, "needs_attention")

		afterEntries, err := os.ReadDir(opDir)
		if err != nil || len(afterEntries) != 0 {
			t.Fatalf("empty op dir was mutated during claim: entries=%v err=%v", afterEntries, err)
		}
	})

	cases := []struct {
		name         string
		lookup       ActiveLookup
		wantDecision string
		checkErr     func(t *testing.T, err error)
	}{
		{
			name:         "needs attention with invalid reason falls back to operation",
			lookup:       ActiveLookup{kind: LookupNeedsAttention, attention: AttentionNone},
			wantDecision: "needs_attention",
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				attn, ok := errors.AsType[*AttentionError](err)
				if !ok || attn == nil || attn.Reason != AttentionOperation {
					t.Fatalf("err = %v, want *AttentionError(%q)", err, AttentionOperation)
				}
			},
		},
		{
			name:         "already running",
			lookup:       ActiveLookup{kind: LookupAlreadyRunning},
			wantDecision: "already_running",
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrAlreadyRunning) {
					t.Fatalf("err = %v, want ErrAlreadyRunning", err)
				}
			},
		},
		{
			name:         "ambiguous",
			lookup:       ActiveLookup{kind: LookupAmbiguous},
			wantDecision: "ambiguous",
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, ErrAmbiguous) {
					t.Fatalf("err = %v, want ErrAmbiguous", err)
				}
			},
		},
		{
			name:   "unexpected lookup kind fails closed",
			lookup: ActiveLookup{kind: LookupNotFound},
			checkErr: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, transfer.ErrInvalidOutputBinding) {
					t.Fatalf("err = %v, want ErrInvalidOutputBinding", err)
				}
				if isAdmissionOutcomeError(err) {
					t.Fatalf("ErrInvalidOutputBinding must not be treated as an admission outcome")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := diagnostics.NewStream(16, nil)
			if err != nil {
				t.Fatal(err)
			}
			auth, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
			if err != nil {
				t.Fatal(err)
			}
			auth.admission = &pendingAdmission{selection: sel}
			auth.stage = stageLookupHeld

			gotErr := auth.settleCreateRecheckLookupLocked(tc.lookup)
			tc.checkErr(t, gotErr)
			if auth.admission != nil || auth.stage != stageStopped {
				t.Fatalf("authority state = (admission=%v stage=%v), want (nil, stageStopped)", auth.admission, auth.stage)
			}
			if tc.wantDecision != "" {
				assertCreateOperationTraceDecision(t, stream, tc.wantDecision)
			}
		})
	}
}

func TestCreateOperationRecheckSurfacesAttentionErrorForCleanupPending(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sel := testSelectionSpec(t, 0xa3)

	stream, err := diagnostics.NewStream(16, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("SelectOperation = (kind=%v err=%v), want LookupCreateReady", lookup.Kind(), err)
	}

	// Another instance creates a matching operation and leaves it cleanup-pending
	// before auth enters CreateOperation's automatic uniqueness re-check.
	_, intent, fileID := createPausedSingleFileOp(t, root, 0xa3)
	rootDir, _, err := OpenContainer(root, false)
	if err != nil {
		t.Fatal(err)
	}
	ctrlDir, err := openChildDir(rootDir, ControlDirName)
	if err != nil {
		_ = rootDir.Close()
		t.Fatal(err)
	}
	markOperationCleanupPending(ctrlDir, hex.EncodeToString(intent.OperationID().Bytes()))
	if err := errors.Join(ctrlDir.Close(), rootDir.Close()); err != nil {
		t.Fatal(err)
	}

	art, err := receivecontract.NewSingleFileDirectoryTree(fileID, "cleanup-recheck.bin", "cleanup-recheck.bin")
	if err != nil {
		t.Fatal(err)
	}
	op, createErr := auth.CreateOperation(ctx, lookup, art)
	if op != nil {
		t.Fatalf("CreateOperation unexpectedly returned operation %+v", op)
	}
	attn, ok := errors.AsType[*AttentionError](createErr)
	if !ok || attn == nil || attn.Reason != AttentionCleanup {
		t.Fatalf("CreateOperation = %v, want *AttentionError(%q)", createErr, AttentionCleanup)
	}
	if _, hasDiag := DiagnosticFor(createErr); hasDiag {
		t.Fatalf("CreateOperation wrapped AttentionError in failure diagnostic: %v", createErr)
	}
	assertCreateOperationTraceDecision(t, stream, "needs_attention")

	if _, retryErr := auth.CreateOperation(ctx, lookup, art); !errors.Is(retryErr, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("retry CreateOperation on stopped authority = %v, want ErrInvalidOutputBinding", retryErr)
	}
}

func assertCreateOperationTraceDecision(t *testing.T, stream *diagnostics.Stream, wantDecision string) {
	t.Helper()
	for {
		select {
		case rec := <-stream.Records():
			p := rec.Payload()
			if p.Get("runtime_operation").Text() == "create_operation" &&
				p.Get("runtime_decision").Text() == wantDecision {
				return
			}
		default:
			t.Fatalf("missing filesystem_output trace with runtime_operation=create_operation runtime_decision=%s", wantDecision)
		}
	}
}
