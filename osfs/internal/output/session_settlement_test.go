//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type settlementFaultJournalFile struct {
	base      journalFile
	syncHook  func(base journalFile) error
	closeHook func(base journalFile) error
}

func (f *settlementFaultJournalFile) WriteAt(b []byte, off int64) (int, error) {
	return f.base.WriteAt(b, off)
}

func (f *settlementFaultJournalFile) Truncate(size int64) error {
	return f.base.Truncate(size)
}

func (f *settlementFaultJournalFile) Sync() error {
	if f.syncHook != nil {
		return f.syncHook(f.base)
	}
	return f.base.Sync()
}

func (f *settlementFaultJournalFile) Close() error {
	if f.closeHook != nil {
		return f.closeHook(f.base)
	}
	return f.base.Close()
}

func openSettlementTestSession(
	t *testing.T,
	root string,
	seed byte,
	sink diagnostics.Sink,
) (*Authority, *Operation, *Session, transfer.ReceiveIntent, transfer.DirectoryAdmission) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root, Diagnostics: sink})
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
	return auth, op, session, intent, rootAdm
}

func commitSmallSettlementFile(
	t *testing.T,
	session *Session,
	intent transfer.ReceiveIntent,
	rootAdm transfer.DirectoryAdmission,
	seed byte,
	name string,
) {
	t.Helper()
	ctx := context.Background()
	payload := []byte("small-payload")
	file := testMaterializationFile(t, session, intent, seed, name, uint64(len(payload)), rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active file transaction")
	}
	if err := tx.WriteRange(ctx, 0, payload); err != nil {
		t.Fatal(err)
	}
	if committed, err := tx.Commit(ctx); err != nil || committed.Kind() != transfer.FilePublished {
		t.Fatalf("commit = (%v, %v)", committed.Kind(), err)
	}
}

func assertOutputStateIODiagnostic(t *testing.T, err error, wantStage FailureStage, wantCause error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected non-nil settlement error")
	}
	if !errors.Is(err, wantCause) {
		t.Fatalf("error %v does not wrap %v", err, wantCause)
	}
	diag, ok := DiagnosticFor(err)
	if !ok || diag.Stage != wantStage {
		t.Fatalf("DiagnosticFor = (%+v, %t), want stage %v", diag, ok, wantStage)
	}
	code, isOutput := diag.Fault.OutputCode()
	if !isOutput || code != transferfault.OutputStateIO || diag.Fault.Scope() != transferfault.ScopeOutputPause {
		t.Fatalf("diagnostic fault = %v, want ScopeOutputPause/OutputStateIO", diag.Fault)
	}
	normalized := transferfault.Normalize(context.Background(), err, nil)
	normCode, normIsOutput := normalized.Fault().OutputCode()
	if !normIsOutput || normCode != transferfault.OutputStateIO {
		t.Fatalf("normalized transfer fault = %v, want OutputStateIO", normalized.Fault())
	}
}

func TestPauseTreeFailureCachesErrorWrapsOutputStateIOAndEmitsFailedTrace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	stream, err := diagnostics.NewStream(32, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth, op, session, intent, rootAdm := openSettlementTestSession(t, root, 0x51, stream.Sink())
	commitSmallSettlementFile(t, session, intent, rootAdm, 0x53, "dirty.bin")

	syncErr := &fs.PathError{Op: "sync", Path: "state.log", Err: fs.ErrPermission}
	wrapper := &settlementFaultJournalFile{
		base:     op.journal.file,
		syncHook: func(journalFile) error { return syncErr },
	}
	op.journal.mu.Lock()
	op.journal.file = wrapper
	op.journal.mu.Unlock()
	t.Cleanup(func() {
		wrapper.syncHook = nil
		_ = auth.Close()
	})

	settlement1, err1 := session.PauseTree(ctx)
	if settlement1.Kind() != 0 {
		t.Fatalf("failed PauseTree returned non-zero settlement: %+v", settlement1)
	}
	assertOutputStateIODiagnostic(t, err1, FailureDurability, syncErr)
	if !op.Idle() {
		t.Fatal("operation was not returned to idle after failed PauseTree")
	}

	// Retry must replay the cached failure instead of fabricating a clean settlement.
	settlement2, err2 := session.PauseTree(ctx)
	if settlement2.Kind() != 0 || err2 == nil || !errors.Is(err2, syncErr) {
		t.Fatalf("retry PauseTree = (%+v, %v), want cached error wrapping %v", settlement2, err2, syncErr)
	}
	assertOutputStateIODiagnostic(t, err2, FailureDurability, syncErr)

	// Cross-kind settlement after a failed PauseTree must fail fatally.
	if _, finErr := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); !errors.Is(finErr, transfer.ErrDirectTreeSessionFatal) {
		t.Fatalf("FinalizeTree after failed PauseTree = %v, want ErrDirectTreeSessionFatal", finErr)
	}

	stream.Close()
	pauseTraces := 0
	for rec := range stream.Records() {
		if rec.Name() != "filesystem_output" {
			continue
		}
		if rec.Payload().Get("runtime_operation").Text() != "pause_tree" {
			continue
		}
		pauseTraces++
		if got := rec.Payload().Get("runtime_decision").Text(); got != "failed" {
			t.Fatalf("pause_tree decision = %q, want failed", got)
		}
		if !rec.Payload().Get("failed").Bool() {
			t.Fatal("pause_tree trace did not mark failed=true")
		}
		if got := rec.Payload().Get("failure_stage").Text(); got != FailureDurability.String() {
			t.Fatalf("pause_tree failure_stage = %q, want %q", got, FailureDurability.String())
		}
		if got := rec.Payload().Get("native_error_class").Text(); got != NativeErrorAccessDenied.String() {
			t.Fatalf("pause_tree native_error_class = %q, want %q", got, NativeErrorAccessDenied.String())
		}
	}
	if pauseTraces != 1 {
		t.Fatalf("pause_tree trace count = %d, want 1", pauseTraces)
	}
}

func TestFinalizeTreePartialFailureCachesErrorAndWrapsOutputStateIO(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, session, intent, rootAdm := openSettlementTestSession(t, root, 0x61, diagnostics.Sink{})
	commitSmallSettlementFile(t, session, intent, rootAdm, 0x63, "partial.bin")
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}

	syncErr := &fs.PathError{Op: "sync", Path: "state.log", Err: fs.ErrPermission}
	wrapper := &settlementFaultJournalFile{
		base:     op.journal.file,
		syncHook: func(journalFile) error { return syncErr },
	}
	op.journal.mu.Lock()
	op.journal.file = wrapper
	op.journal.mu.Unlock()
	t.Cleanup(func() {
		wrapper.syncHook = nil
		_ = auth.Close()
	})

	settlement1, err1 := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial)
	if settlement1.Kind() != 0 {
		t.Fatalf("failed FinalizeTree(Partial) returned non-zero settlement: %+v", settlement1)
	}
	assertOutputStateIODiagnostic(t, err1, FailureDurability, syncErr)
	if !op.Idle() {
		t.Fatal("operation was not returned to idle after failed FinalizeTree(Partial)")
	}

	settlement2, err2 := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial)
	if settlement2.Kind() != 0 || err2 == nil || !errors.Is(err2, syncErr) {
		t.Fatalf("retry FinalizeTree(Partial) = (%+v, %v), want cached error wrapping %v", settlement2, err2, syncErr)
	}
	if _, mismatchErr := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); !errors.Is(mismatchErr, transfer.ErrDirectTreeSessionFatal) {
		t.Fatalf("FinalizeTree(Success) after failed Partial = %v, want ErrDirectTreeSessionFatal", mismatchErr)
	}
	if _, pauseErr := session.PauseTree(ctx); !errors.Is(pauseErr, transfer.ErrDirectTreeSessionFatal) {
		t.Fatalf("PauseTree after failed FinalizeTree = %v, want ErrDirectTreeSessionFatal", pauseErr)
	}
}

func TestFinalizeTreeSuccessCleanupFailureCachesErrorWrapsOutputStateIOAndMarksNonIdle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, session, intent, rootAdm := openSettlementTestSession(t, root, 0x71, diagnostics.Sink{})
	commitSmallSettlementFile(t, session, intent, rootAdm, 0x73, "done.bin")
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}

	closeErr := &fs.PathError{Op: "close", Path: "state.log", Err: fs.ErrPermission}
	wrapper := &settlementFaultJournalFile{
		base: op.journal.file,
		closeHook: func(base journalFile) error {
			_ = base.Close()
			return closeErr
		},
	}
	op.journal.mu.Lock()
	op.journal.file = wrapper
	op.journal.mu.Unlock()
	t.Cleanup(func() {
		wrapper.closeHook = nil
		_ = auth.Close()
	})

	settlement1, err1 := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
	if settlement1.Kind() != 0 {
		t.Fatalf("failed FinalizeTree(Success) returned non-zero settlement: %+v", settlement1)
	}
	assertOutputStateIODiagnostic(t, err1, FailureClose, closeErr)
	if op.Idle() {
		t.Fatal("operation stayed idle after FinalizeTree(Success) entered retirement")
	}

	settlement2, err2 := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
	if settlement2.Kind() != 0 || err2 == nil || !errors.Is(err2, closeErr) {
		t.Fatalf("retry FinalizeTree(Success) = (%+v, %v), want cached error wrapping %v", settlement2, err2, closeErr)
	}
	if _, mismatchErr := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); !errors.Is(mismatchErr, transfer.ErrDirectTreeSessionFatal) {
		t.Fatalf("FinalizeTree(Partial) after failed Success = %v, want ErrDirectTreeSessionFatal", mismatchErr)
	}
}

func TestFinalizeTreeCleanSuccessReplaysExactOutcomeAndRejectsMismatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, _, session, intent, rootAdm := openSettlementTestSession(t, root, 0x79, diagnostics.Sink{})
	defer auth.Close()
	commitSmallSettlementFile(t, session, intent, rootAdm, 0x7b, "ok.bin")
	if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
		t.Fatal(err)
	}

	settled1, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
	if err != nil || settled1.Kind() != transfer.DirectTreeSettlementSuccess {
		t.Fatalf("FinalizeTree(Success) = (%+v, %v)", settled1, err)
	}
	settled2, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess)
	if err != nil || settled2 != settled1 {
		t.Fatalf("repeat FinalizeTree(Success) = (%+v, %v), want %+v", settled2, err, settled1)
	}
	if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomePartial); !errors.Is(err, transfer.ErrDirectTreeSessionFatal) {
		t.Fatalf("FinalizeTree(Partial) after clean Success = %v, want ErrDirectTreeSessionFatal", err)
	}
}

func TestDiagnoseFailureDoesNotFabricateSessionDependencyContractForRawErrors(t *testing.T) {
	rawClose := DiagnoseFailure(FailureClose, errors.New("handle close io error"))
	diag, ok := DiagnosticFor(rawClose)
	if !ok || diag.Stage != FailureClose || !diag.Fault.IsZero() {
		t.Fatalf("raw close diagnostic = (%+v, %t), want stage=FailureClose and zero fault", diag, ok)
	}

	rawBusy := DiagnoseFailure(FailureOpen, ErrBusy)
	busyDiag, ok := DiagnosticFor(rawBusy)
	if !ok || busyDiag.Stage != FailureOpen || !busyDiag.Fault.IsZero() {
		t.Fatalf("raw busy diagnostic = (%+v, %t), want stage=FailureOpen and zero fault", busyDiag, ok)
	}

	wrappedIO := DiagnoseFailure(FailureDurability, wrapOutputFaultError(transferfault.OutputStateIO, "flush", errors.New("disk full")))
	assertOutputStateIODiagnostic(t, wrappedIO, FailureDurability, wrappedIO)
}
