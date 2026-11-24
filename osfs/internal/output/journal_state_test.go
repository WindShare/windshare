//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
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

type faultyJournalFile struct {
	base         journalFile
	writeAtHook  func(base journalFile, b []byte, off int64) (int, error)
	truncateHook func(base journalFile, size int64) error
	writeCalls   int
	truncateArgs []int64
}

func (f *faultyJournalFile) WriteAt(b []byte, off int64) (int, error) {
	f.writeCalls++
	if f.writeAtHook != nil {
		return f.writeAtHook(f.base, b, off)
	}
	return f.base.WriteAt(b, off)
}

func (f *faultyJournalFile) Truncate(size int64) error {
	f.truncateArgs = append(f.truncateArgs, size)
	if f.truncateHook != nil {
		return f.truncateHook(f.base, size)
	}
	return f.base.Truncate(size)
}

func (f *faultyJournalFile) Sync() error  { return f.base.Sync() }
func (f *faultyJournalFile) Close() error { return f.base.Close() }

func TestJournalShortWriteRollbackAllowsRetryAndReopen(t *testing.T) {
	ctx := context.Background()
	const (
		partSize  uint64 = 4096
		totalSize uint64 = 3 * LargeCheckpointBytes
	)
	chunk := bytes.Repeat([]byte{0x6b}, int(partSize))

	t.Run("rolls back partial write so retry append and reopen preserve progress", func(t *testing.T) {
		root := t.TempDir()
		const (
			seed     byte = 0xd3
			fileName      = "short-write-retry.bin"
		)
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)
		intent, _ := op.ReceiveIntent()
		journalPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()), journalFileName)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		committedBefore, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}

		if err := tx.WriteRange(ctx, partSize, chunk); err != nil {
			t.Fatal(err)
		}

		errDiskFull := errors.New("simulated no space left on device")
		shortWritten := false
		op.journal.mu.Lock()
		wrapper := &faultyJournalFile{
			base: op.journal.file,
			writeAtHook: func(base journalFile, b []byte, off int64) (int, error) {
				if !shortWritten {
					shortWritten = true
					half := len(b) / 2
					n, writeErr := base.WriteAt(b[:half], off)
					if writeErr != nil {
						return n, writeErr
					}
					return n, errDiskFull
				}
				return base.WriteAt(b, off)
			},
		}
		op.journal.file = wrapper
		op.journal.mu.Unlock()

		if _, err := tx.Checkpoint(ctx); !errors.Is(err, errDiskFull) {
			t.Fatalf("short-write Checkpoint err = %v, want %v", err, errDiskFull)
		}
		if len(wrapper.truncateArgs) != 1 || wrapper.truncateArgs[0] != int64(len(committedBefore)) {
			t.Fatalf("rollback truncate args = %v, want [%d]", wrapper.truncateArgs, len(committedBefore))
		}
		afterRollback, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterRollback, committedBefore) {
			t.Fatalf("journal after rollback len=%d, want exact committed prefix len=%d", len(afterRollback), len(committedBefore))
		}

		// Retry appending on the same open journal (mirrors Pause/RecoveryRetry after freeing space).
		paused, err := tx.Pause(ctx)
		if err != nil || paused.Kind() != transfer.FilePaused {
			t.Fatalf("Pause retry after short-write rollback = (%v, %v), want FilePaused", paused.Kind(), err)
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatal(err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		inv, err := ListOperations(ctx, root)
		if err != nil || inv.Status() != RecoveryListReady || len(inv.Summaries()) != 1 ||
			inv.Summaries()[0].State() != RecoveryOperationResumable {
			t.Fatalf("inventory after retry = (%+v, %v), want ready+resumable", inv, err)
		}
		reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
		if got := rangeTotalBytes(reopened); got != 2*partSize {
			t.Fatalf("reopened reusable bytes = %d, want %d", got, 2*partSize)
		}
	})

	t.Run("silent short write without error is rolled back and retried cleanly", func(t *testing.T) {
		root := t.TempDir()
		const (
			seed     byte = 0xd7
			fileName      = "silent-short-write.bin"
		)
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		// Force compaction first to verify j.offset tracks the compacted file length.
		op.journal.mu.Lock()
		compactErr := op.journal.compactLocked()
		op.journal.mu.Unlock()
		if compactErr != nil {
			t.Fatal(compactErr)
		}

		if err := tx.WriteRange(ctx, partSize, chunk); err != nil {
			t.Fatal(err)
		}
		shortWritten := false
		op.journal.mu.Lock()
		op.journal.file = &faultyJournalFile{
			base: op.journal.file,
			writeAtHook: func(base journalFile, b []byte, off int64) (int, error) {
				if !shortWritten {
					shortWritten = true
					half := len(b) / 2
					return base.WriteAt(b[:half], off)
				}
				return base.WriteAt(b, off)
			},
		}
		op.journal.mu.Unlock()

		if _, err := tx.Checkpoint(ctx); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("silent short-write Checkpoint err = %v, want io.ErrShortWrite", err)
		}
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatalf("retry Checkpoint after silent short write = %v", err)
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

		reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
		if got := rangeTotalBytes(reopened); got != 2*partSize {
			t.Fatalf("reopened reusable bytes after compaction+short-write retry = %d, want %d", got, 2*partSize)
		}
	})

	t.Run("failed rollback blocks later appends so torn tail stays recoverable on reopen", func(t *testing.T) {
		root := t.TempDir()
		const (
			seed     byte = 0xdb
			fileName      = "rollback-fail.bin"
		)
		auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)
		intent, _ := op.ReceiveIntent()
		journalPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()), journalFileName)

		if err := tx.WriteRange(ctx, 0, chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		committedBefore, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}

		if err := tx.WriteRange(ctx, partSize, chunk); err != nil {
			t.Fatal(err)
		}

		errDiskFull := errors.New("simulated no space left on device")
		errTruncate := errors.New("simulated truncate io error")
		shortWritten := false
		op.journal.mu.Lock()
		wrapper := &faultyJournalFile{
			base: op.journal.file,
			writeAtHook: func(base journalFile, b []byte, off int64) (int, error) {
				if !shortWritten {
					shortWritten = true
					half := len(b) / 2
					n, writeErr := base.WriteAt(b[:half], off)
					if writeErr != nil {
						return n, writeErr
					}
					return n, errDiskFull
				}
				return base.WriteAt(b, off)
			},
			truncateHook: func(_ journalFile, _ int64) error {
				return errTruncate
			},
		}
		op.journal.file = wrapper
		op.journal.mu.Unlock()

		if _, err := tx.Checkpoint(ctx); !errors.Is(err, errDiskFull) || !errors.Is(err, errTruncate) {
			t.Fatalf("Checkpoint err = %v, want joined (%v, %v)", err, errDiskFull, errTruncate)
		}
		if wrapper.writeCalls != 1 {
			t.Fatalf("writeCalls after initial short write = %d, want 1", wrapper.writeCalls)
		}

		// Subsequent Pause retry must be refused without touching the file so
		// the torn prefix stays strictly at EOF rather than getting buried mid-log.
		if _, err := tx.Pause(ctx); !errors.Is(err, errJournalTailDirty) {
			t.Fatalf("Pause after failed rollback err = %v, want %v", err, errJournalTailDirty)
		}
		if wrapper.writeCalls != 1 {
			t.Fatalf("writeCalls after blocked Pause retry = %d, want 1", wrapper.writeCalls)
		}
		if _, err := session.PauseTree(ctx); err != nil {
			t.Fatalf("PauseTree after failed Pause = %v, want nil", err)
		}
		if err := auth.Close(); err != nil {
			t.Fatal(err)
		}

		onDisk, err := os.ReadFile(journalPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(onDisk) <= len(committedBefore) || onDisk[len(onDisk)-1] == '\n' {
			t.Fatalf("expected unterminated torn tail on disk after failed rollback: committed=%d, actual=%d",
				len(committedBefore), len(onDisk))
		}

		inv, err := ListOperations(ctx, root)
		if err != nil || inv.Status() != RecoveryListReady || len(inv.Summaries()) != 1 ||
			inv.Summaries()[0].State() != RecoveryOperationResumable {
			t.Fatalf("inventory with isolated torn tail = (%+v, %v), want ready+resumable", inv, err)
		}
		reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
		if got := rangeTotalBytes(reopened); got != partSize {
			t.Fatalf("reopened reusable bytes = %d, want committed checkpoint %d", got, partSize)
		}
	})
}
