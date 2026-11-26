//go:build windows || linux || darwin

package output

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func openLargeFileTransactionForTest(
	t *testing.T,
	root string,
	seed byte,
	fileName string,
	exactSize uint64,
) (*Authority, *Operation, transfer.DirectTreeSession, transfer.SelectionSpec, transfer.FileTransaction) {
	t.Helper()
	return openLargeFileTransactionWithClockForTest(t, root, seed, fileName, exactSize, nil)
}

func openLargeFileTransactionWithClockForTest(
	t *testing.T,
	root string,
	seed byte,
	fileName string,
	exactSize uint64,
	now func() time.Time,
) (*Authority, *Operation, transfer.DirectTreeSession, transfer.SelectionSpec, transfer.FileTransaction) {
	t.Helper()
	ctx := context.Background()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if now != nil {
		auth.now = now
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
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	file := testMaterializationFile(t, session, intent, seed+2, fileName, exactSize, rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		_ = auth.Close()
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		_ = auth.Close()
		t.Fatal("expected active file transaction")
	}
	return auth, op, session, sel, tx
}

func reopenReusableRangesForTest(
	t *testing.T,
	root string,
	sel transfer.SelectionSpec,
	seed byte,
	fileName string,
	exactSize uint64,
) []content.Range {
	t.Helper()
	ctx := context.Background()
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupReopened {
		t.Fatalf("reopen select = (%v, %v)", lookup.Kind(), err)
	}
	intent, _ := lookup.Operation().ReceiveIntent()
	session, err := reopen.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	file := testMaterializationFile(t, session, intent, seed+2, fileName, exactSize, rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	_, durable, ok := start.Transaction()
	if !ok {
		t.Fatal("expected reopened file transaction")
	}
	ranges := durable.Ranges().Ranges()
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	return ranges
}

func TestLargeFileCheckpointFailureAllowsPauseRetryAndMatchesReopen(t *testing.T) {
	ctx := context.Background()
	const (
		seed      byte = 0xb1
		fileName       = "large.bin"
		totalSize      = 2 * LargeCheckpointBytes
	)
	chunk := make([]byte, LargeCheckpointBytes)
	chunk[0] = 0x5a
	chunk[len(chunk)-1] = 0xa5

	t.Run("transient partial journal error is retried by Pause and reused on reopen", func(t *testing.T) {
		root := t.TempDir()
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		op.journal.appendHook = func(kind string) error {
			if kind == "partial" {
				attempts++
				if attempts == 1 {
					return errors.New("simulated transient partial journal error")
				}
			}
			return nil
		}
		if _, err := tx.Accept(ctx); err == nil {
			t.Fatal("expected Accept checkpoint to fail on first partial journal write")
		}
		paused, err := tx.Pause(ctx)
		if err != nil || paused.Kind() != transfer.FilePaused {
			t.Fatalf("Pause retry = (%v, %v), want FilePaused without error", paused.Kind(), err)
		}
		if attempts != 2 {
			t.Fatalf("partial journal attempts = %d, want 2 (Accept + Pause retry)", attempts)
		}
		verified, ok := paused.VerifiedProgress()
		if !ok || rangeTotalBytes(verified.Ranges().Ranges()) != LargeCheckpointBytes {
			t.Fatalf("paused verified progress = (%v, %v), want %d bytes", ok, verified.Ranges().Ranges(), LargeCheckpointBytes)
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
		if rangeTotalBytes(reopened) != LargeCheckpointBytes {
			t.Fatalf("reopened reusable ranges = %v (%d bytes), want %d bytes", reopened, rangeTotalBytes(reopened), LargeCheckpointBytes)
		}
	})

	t.Run("persistent partial journal error fails Pause instead of reporting fake progress", func(t *testing.T) {
		root := t.TempDir()
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed+8, fileName, totalSize)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		op.journal.appendHook = func(kind string) error {
			if kind == "partial" {
				attempts++
				return errors.New("simulated persistent partial journal error")
			}
			return nil
		}
		if _, err := tx.Accept(ctx); err == nil {
			t.Fatal("expected Accept checkpoint to fail")
		}
		if _, err := tx.Pause(ctx); err == nil {
			t.Fatal("expected Pause to fail when partial journal write keeps failing")
		} else if diag, ok := DiagnosticFor(err); !ok || diag.Stage != FailureDurability {
			t.Fatalf("Pause diagnostic = (%+v, %v), want FailureDurability", diag, ok)
		}
		if attempts != 2 {
			t.Fatalf("partial journal attempts = %d, want 2", attempts)
		}
		op.journal.appendHook = nil
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatalf("PauseTree after failed file Pause = %v, want nil", err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		reopened := reopenReusableRangesForTest(t, root, sel, seed+8, fileName, totalSize)
		if len(reopened) != 0 {
			t.Fatalf("reopened reusable ranges = %v, want empty", reopened)
		}
	})

	t.Run("failed explicit Checkpoint does not suppress a subsequent Checkpoint retry", func(t *testing.T) {
		root := t.TempDir()
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed+16, fileName, totalSize)

		if err := tx.WriteRange(ctx, 0, chunk[:4096]); err != nil {
			t.Fatal(err)
		}
		attempts := 0
		op.journal.appendHook = func(kind string) error {
			if kind == "partial" {
				attempts++
				if attempts == 1 {
					return errors.New("simulated first checkpoint failure")
				}
			}
			return nil
		}
		if _, err := tx.Checkpoint(ctx); err == nil {
			t.Fatal("expected first Checkpoint to fail")
		}
		verified, err := tx.Checkpoint(ctx)
		if err != nil {
			t.Fatalf("second Checkpoint retry failed: %v", err)
		}
		if attempts != 2 {
			t.Fatalf("partial journal attempts = %d, want 2", attempts)
		}
		if verified.CheckpointGeneration() != 1 || rangeTotalBytes(verified.Ranges().Ranges()) != 4096 {
			t.Fatalf("checkpoint = (gen=%d, ranges=%v), want gen=1 and 4096 bytes", verified.CheckpointGeneration(), verified.Ranges().Ranges())
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		reopened := reopenReusableRangesForTest(t, root, sel, seed+16, fileName, totalSize)
		if rangeTotalBytes(reopened) != 4096 {
			t.Fatalf("reopened reusable ranges = %v, want 4096 bytes", reopened)
		}
	})

	t.Run("failed second checkpoint preserves prior persisted progress and retries on Pause", func(t *testing.T) {
		const threeChunks = 3 * LargeCheckpointBytes
		root := t.TempDir()
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed+24, fileName, threeChunks)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Accept(ctx); err != nil {
			t.Fatalf("first 8 MiB Accept failed: %v", err)
		}
		if err := tx.WriteRange(ctx, LargeCheckpointBytes, chunk); err != nil {
			t.Fatal(err)
		}
		attemptsAfterFirst := 0
		op.journal.appendHook = func(kind string) error {
			if kind == "partial" {
				attemptsAfterFirst++
				if attemptsAfterFirst == 1 {
					return errors.New("simulated transient second checkpoint failure")
				}
			}
			return nil
		}
		if _, err := tx.Accept(ctx); err == nil {
			t.Fatal("expected second 8 MiB Accept checkpoint to fail")
		}
		paused, err := tx.Pause(ctx)
		if err != nil || paused.Kind() != transfer.FilePaused {
			t.Fatalf("Pause retry = (%v, %v), want FilePaused without error", paused.Kind(), err)
		}
		verified, ok := paused.VerifiedProgress()
		if !ok || verified.CheckpointGeneration() != 2 || rangeTotalBytes(verified.Ranges().Ranges()) != 2*LargeCheckpointBytes {
			t.Fatalf("paused verified = (%v, gen=%d, bytes=%d), want gen=2 and %d bytes", ok, verified.CheckpointGeneration(), rangeTotalBytes(verified.Ranges().Ranges()), 2*LargeCheckpointBytes)
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		reopened := reopenReusableRangesForTest(t, root, sel, seed+24, fileName, threeChunks)
		if rangeTotalBytes(reopened) != 2*LargeCheckpointBytes {
			t.Fatalf("reopened reusable ranges = %v (%d bytes), want %d bytes", reopened, rangeTotalBytes(reopened), 2*LargeCheckpointBytes)
		}
	})
}

func TestLargeFileIntervalCheckpointTriggersPassivelyOnAccept(t *testing.T) {
	ctx := context.Background()
	const (
		seed      byte   = 0xc4
		fileName         = "trickle.bin"
		totalSize uint64 = 2 * LargeCheckpointBytes
		sliceSize uint64 = 4096
	)
	clock := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	auth, op, session, sel, tx := openLargeFileTransactionWithClockForTest(
		t, root, seed, fileName, totalSize, func() time.Time { return clock },
	)
	canonicalPath := tx.Binding().Locator().CanonicalPath()
	slice := make([]byte, sliceSize)
	slice[0] = 0x3c
	slice[len(slice)-1] = 0xc3

	// A sub-threshold write before LargeCheckpointInterval stays accepted in
	// memory without flushing a durable partial checkpoint.
	if err := tx.WriteRange(ctx, 0, slice); err != nil {
		t.Fatal(err)
	}
	progress, err := tx.Accept(ctx)
	if err != nil {
		t.Fatalf("first Accept failed: %v", err)
	}
	if persisted, ok := progress.Persisted(); !ok || persisted.CheckpointGeneration() != 0 || !persisted.Ranges().IsEmpty() {
		t.Fatalf("first Accept persisted = (%v, gen=%d, ranges=%v), want empty gen=0", ok, persisted.CheckpointGeneration(), persisted.Ranges().Ranges())
	}

	// Advancing the clock while stalled does not flush asynchronously.
	clock = clock.Add(LargeCheckpointInterval)
	if partial, exists := op.journal.lookupPartial(canonicalPath); exists {
		t.Fatalf("unexpected background partial checkpoint while idle: %+v", partial)
	}

	// Once LargeCheckpointInterval has elapsed, the next Accept persists all
	// accumulated ranges and resets the interval baseline.
	if err := tx.WriteRange(ctx, sliceSize, slice); err != nil {
		t.Fatal(err)
	}
	progress, err = tx.Accept(ctx)
	if err != nil {
		t.Fatalf("second Accept failed: %v", err)
	}
	if persisted, ok := progress.Persisted(); !ok || persisted.CheckpointGeneration() != 1 || rangeTotalBytes(persisted.Ranges().Ranges()) != 2*sliceSize {
		t.Fatalf("second Accept persisted = (%v, gen=%d, bytes=%d), want gen=1 and %d bytes", ok, persisted.CheckpointGeneration(), rangeTotalBytes(persisted.Ranges().Ranges()), 2*sliceSize)
	}

	// A subsequent sub-threshold write within LargeCheckpointInterval does not
	// checkpoint again until the interval elapses or Pause flushes it.
	clock = clock.Add(LargeCheckpointInterval / 2)
	if err := tx.WriteRange(ctx, 2*sliceSize, slice); err != nil {
		t.Fatal(err)
	}
	progress, err = tx.Accept(ctx)
	if err != nil {
		t.Fatalf("third Accept failed: %v", err)
	}
	if persisted, ok := progress.Persisted(); !ok || persisted.CheckpointGeneration() != 1 || rangeTotalBytes(persisted.Ranges().Ranges()) != 2*sliceSize {
		t.Fatalf("third Accept persisted = (%v, gen=%d, bytes=%d), want unchanged gen=1 and %d bytes", ok, persisted.CheckpointGeneration(), rangeTotalBytes(persisted.Ranges().Ranges()), 2*sliceSize)
	}

	// Pause flushes the remaining unpersisted bytes for restart recovery.
	paused, err := tx.Pause(ctx)
	if err != nil || paused.Kind() != transfer.FilePaused {
		t.Fatalf("Pause = (%v, %v), want FilePaused without error", paused.Kind(), err)
	}
	if verified, ok := paused.VerifiedProgress(); !ok || verified.CheckpointGeneration() != 2 || rangeTotalBytes(verified.Ranges().Ranges()) != 3*sliceSize {
		t.Fatalf("paused verified = (%v, gen=%d, bytes=%d), want gen=2 and %d bytes", ok, verified.CheckpointGeneration(), rangeTotalBytes(verified.Ranges().Ranges()), 3*sliceSize)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
	if rangeTotalBytes(reopened) != 3*sliceSize {
		t.Fatalf("reopened reusable ranges = %v (%d bytes), want %d bytes", reopened, rangeTotalBytes(reopened), 3*sliceSize)
	}
}

func assertFileLocalOutputStateIOFault(t *testing.T, err error, wantCause error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected non-nil file settlement error")
	}
	if !errors.Is(err, wantCause) {
		t.Fatalf("error %v does not wrap %v", err, wantCause)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v unexpectedly carries caller cancellation sentinel", err)
	}
	normalized := transferfault.Normalize(context.Background(), err, nil)
	code, isOutput := normalized.Fault().OutputCode()
	if !isOutput || code != transferfault.OutputStateIO || normalized.Fault().Scope() != transferfault.ScopeFileLocal {
		t.Fatalf("normalized fault = %v, want ScopeFileLocal/OutputStateIO", normalized.Fault())
	}
}

func TestFileTransactionSettlementPreservesIOFaultUnderCanceledContext(t *testing.T) {
	ctx := context.Background()
	ioErr := &fs.PathError{Op: "write", Path: "state.log", Err: fs.ErrPermission}

	pauseCases := []struct {
		name      string
		seed      byte
		cancelCtx func() (context.Context, context.CancelFunc)
	}{
		{
			name: "Pause preserves FailureDurability under canceled context",
			seed: 0xd1,
			cancelCtx: func() (context.Context, context.CancelFunc) {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				return canceled, cancel
			},
		},
		{
			name: "Pause preserves FailureDurability under deadline-exceeded context",
			seed: 0xd5,
			cancelCtx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(ctx, time.Now().Add(-time.Second))
			},
		},
	}

	for _, tc := range pauseCases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			auth, op, session, _, tx := openLargeFileTransactionForTest(t, root, tc.seed, "dirty.bin", 64)
			defer auth.Close()

			if err := tx.WriteRange(ctx, 0, []byte("unpersisted-bytes")); err != nil {
				t.Fatal(err)
			}
			op.journal.appendHook = func(kind string) error {
				if kind == "partial" {
					return ioErr
				}
				return nil
			}

			settledCtx, cancel := tc.cancelCtx()
			defer cancel()

			settlement, err := tx.Pause(settledCtx)
			if settlement.Kind() != 0 {
				t.Fatalf("failed Pause returned non-zero settlement: %+v", settlement)
			}
			assertFileLocalOutputStateIOFault(t, err, ioErr)
			diag, ok := DiagnosticFor(err)
			if !ok || diag.Stage != FailureDurability || diag.NativeErrorClass != NativeErrorAccessDenied {
				t.Fatalf("DiagnosticFor = (%+v, %t), want stage=FailureDurability native=access_denied", diag, ok)
			}
			code, isOutput := diag.Fault.OutputCode()
			if !isOutput || code != transferfault.OutputStateIO || diag.Fault.Scope() != transferfault.ScopeFileLocal {
				t.Fatalf("diagnostic fault = %v, want ScopeFileLocal/OutputStateIO", diag.Fault)
			}

			op.journal.appendHook = nil
			if _, err := session.PauseTree(ctx); err != nil {
				t.Fatalf("PauseTree after failed Pause = %v, want nil", err)
			}
			if _, repeatErr := tx.Pause(ctx); !errors.Is(repeatErr, transfer.ErrInvalidOutputBinding) {
				t.Fatalf("repeat tx.Pause = %v, want ErrInvalidOutputBinding", repeatErr)
			}
		})
	}

	t.Run("Retire preserves ScopeFileLocal OutputStateIO under canceled context", func(t *testing.T) {
		root := t.TempDir()
		auth, op, session, _, tx := openLargeFileTransactionForTest(t, root, 0xd9, "retire.bin", 64)
		defer auth.Close()

		if err := tx.WriteRange(ctx, 0, []byte("staged-bytes")); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		op.journal.appendHook = func(kind string) error {
			if kind == "retired" {
				return ioErr
			}
			return nil
		}

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		settlement, err := tx.Retire(canceledCtx)
		if settlement.Kind() != 0 {
			t.Fatalf("failed Retire returned non-zero settlement: %+v", settlement)
		}
		assertFileLocalOutputStateIOFault(t, err, ioErr)

		op.journal.appendHook = nil
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatalf("PauseTree after failed Retire = %v, want nil", err)
		}
	})

	t.Run("wrapFileFault keeps caller cancellation priority for active operations", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()

		if err := wrapFileFault(canceledCtx, transferfault.OutputStateIO, "write staged file range", ioErr); !errors.Is(err, context.Canceled) {
			t.Fatalf("wrapFileFault under canceled ctx = %v, want context.Canceled", err)
		}
		if err := wrapFileFaultError(transferfault.OutputStateIO, "pause staged file", context.Canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("wrapFileFaultError with cancellation cause = %v, want context.Canceled", err)
		}
	})
}
