//go:build windows || linux || darwin

package output

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
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
	closeHook    func(base journalFile) error
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

func (f *faultyJournalFile) Sync() error { return f.base.Sync() }
func (f *faultyJournalFile) Close() error {
	if f.closeHook != nil {
		return f.closeHook(f.base)
	}
	return f.base.Close()
}

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

func TestOpenTaskJournalToleratesCompactionFailureWhenFileStaysOpen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const (
		seed      byte   = 0xdf
		fileName         = "compact-fallback.bin"
		partSize  uint64 = 4096
		totalSize uint64 = 3 * LargeCheckpointBytes
	)
	chunk := bytes.Repeat([]byte{0x5a}, int(partSize))

	auth, op, session, sel, tx := openLargeFileTransactionForTest(t, root, seed, fileName, totalSize)
	intent, _ := op.ReceiveIntent()
	if err := tx.WriteRange(ctx, 0, chunk); err != nil {
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
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		t.Fatal(err)
	}
	redundantLine, err := encodeJournalLine(journalRecord{Kind: "retired", Path: "noop.bin"})
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	for range journalCompactMinLines + 4 {
		if _, err := f.Write(redundantLine); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Occupy state.log.compact with a directory so compactLocked fails before
	// closing j.file; openTaskJournal must fall back to the valid open journal.
	blockerDir := filepath.Join(opPath, "state.log.compact")
	if err := os.Mkdir(blockerDir, privateDirMode); err != nil {
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
	lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupReopened || lookup.Operation() == nil {
		_ = reopen.Close()
		t.Fatalf("SelectOperation with failed compaction = (kind=%v, err=%v), want LookupReopened", lookup.Kind(), err)
	}
	reIntent, _ := lookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, reSession, reIntent, seed+1)
	file := testMaterializationFile(t, reSession, reIntent, seed+2, fileName, totalSize, rootAdm)
	start, err := reSession.BeginFile(ctx, file)
	if err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	reTx, durable, ok := start.Transaction()
	if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != partSize {
		_ = reopen.Close()
		t.Fatalf("reopened transaction = (ok=%t, bytes=%d), want (true, %d)", ok, rangeTotalBytes(durable.Ranges().Ranges()), partSize)
	}
	if err := reTx.WriteRange(ctx, partSize, chunk); err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	if _, err := reTx.Pause(ctx); err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	if _, err := reSession.PauseTree(ctx); err != nil {
		_ = reopen.Close()
		t.Fatal(err)
	}
	if err := reopen.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(blockerDir); err != nil {
		t.Fatal(err)
	}
	reopened := reopenReusableRangesForTest(t, root, sel, seed, fileName, totalSize)
	if got := rangeTotalBytes(reopened); got != 2*partSize {
		t.Fatalf("reusable bytes after fallback append = %d, want %d", got, 2*partSize)
	}
}

func TestJournalCompactionUnlinksTempFileOnCloseFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	auth, op, intent := openActiveSingleFileOp(t, root, 0xe0)
	defer auth.Close()

	errClose := errors.New("simulated journal close failure")
	op.journal.mu.Lock()
	op.journal.file = &faultyJournalFile{
		base: op.journal.file,
		closeHook: func(base journalFile) error {
			_ = base.Close()
			return errClose
		},
	}
	compactErr := op.journal.compactLocked()
	fileAfter := op.journal.file
	op.journal.mu.Unlock()

	if !errors.Is(compactErr, errClose) {
		t.Fatalf("compactLocked err = %v, want %v", compactErr, errClose)
	}
	if fileAfter != nil {
		t.Fatal("expected j.file == nil after close failure")
	}

	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	if _, err := os.Stat(filepath.Join(opPath, "state.log.compact")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("state.log.compact survived close failure: %v", err)
	}
	if err := op.RecordShareStopped(ctx); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("append after failed close = %v, want fs.ErrClosed", err)
	}
	if err := auth.Close(); err != nil {
		t.Fatalf("auth.Close after failed compaction close = %v", err)
	}

	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opDir.Close()
	state, _, err := readJournalState(opDir)
	if err != nil || state.meta.Intent.OperationID() != intent.OperationID() {
		t.Fatalf("readJournalState after failed compaction close = (%+v, %v)", state.meta, err)
	}
}

func TestJournalRejectsZeroFingerprintNonEmptyPartialWithoutClobberingPublishing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	const seed byte = 0xe1
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
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
	fid := testID16[catalog.FileID](seed + 2)
	rev := testID16[content.FileRevision](seed + 3)
	pubFP := [stageFingerprintBytes]byte{0xab}
	const filePath = "windshare-selection/guarded.bin"

	if err := op.journal.recordPublishing(publishingEntry{
		FileID: fid, Path: filePath, Revision: rev,
		ExactSize: 1024, PartName: "guarded.part", CheckpointGen: 2, Fingerprint: pubFP,
	}, true); err != nil {
		t.Fatal(err)
	}

	// Non-empty partial without a fingerprint must be rejected, rolled back, and
	// leave the existing publishing record intact.
	err = op.journal.recordPartial(partialEntry{
		FileID: fid, Path: filePath, Revision: rev,
		ExactSize: 1024, PartName: "guarded.part",
		Ranges: []content.Range{{Offset: 0, End: 1024}}, CheckpointGen: 3,
	}, true)
	if !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("zero-fp non-empty recordPartial = %v, want errJournalCorrupt", err)
	}
	if pub, ok := op.journal.lookupPublishing(filePath); !ok || pub.Fingerprint != pubFP {
		t.Fatalf("publishing clobbered after rejected partial: (%+v, %t)", pub, ok)
	}
	if _, hasPartial := op.journal.lookupPartial(filePath); hasPartial {
		t.Fatal("rejected partial was applied to journal state")
	}

	// Empty-range partials (0-byte files / pre-write checkpoints) remain valid with a zero fingerprint.
	if err := op.journal.recordPartial(partialEntry{
		FileID: testID16[catalog.FileID](seed + 4), Path: "windshare-selection/empty.bin", Revision: rev,
		ExactSize: 0, PartName: "empty.part", CheckpointGen: 1,
	}, true); err != nil {
		t.Fatalf("empty-range recordPartial failed: %v", err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the rolled-back state.log must succeed with publishing preserved.
	opPath := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	opDir, err := openDirHandle(opPath)
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := readJournalState(opDir)
	if err != nil {
		_ = opDir.Close()
		t.Fatalf("readJournalState after rejected partial rollback = %v", err)
	}
	if pub, ok := state.publishing[filePath]; !ok || pub.Fingerprint != pubFP {
		_ = opDir.Close()
		t.Fatalf("reopened publishing = (%+v, %t), want preserved fp", pub, ok)
	}

	// A persisted non-empty partial record missing fp must be rejected on replay.
	badLine, err := encodeJournalLine(partialToRecord(partialEntry{
		FileID: fid, Path: filePath, Revision: rev,
		ExactSize: 1024, PartName: "guarded.part",
		Ranges: []content.Range{{Offset: 0, End: 512}}, CheckpointGen: 4,
	}))
	if err != nil {
		_ = opDir.Close()
		t.Fatal(err)
	}
	journalPath := filepath.Join(opPath, journalFileName)
	f, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, privateFileMode)
	if err != nil {
		_ = opDir.Close()
		t.Fatal(err)
	}
	if _, err := f.Write(badLine); err != nil {
		_ = f.Close()
		_ = opDir.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		_ = opDir.Close()
		t.Fatal(err)
	}
	_, _, replayErr := readJournalState(opDir)
	_ = opDir.Close()
	if !errors.Is(replayErr, errJournalCorrupt) {
		t.Fatalf("readJournalState on persisted zero-fp non-empty partial = %v, want errJournalCorrupt", replayErr)
	}
}

type countingRepeatByteReader struct {
	fill      byte
	bytesRead int64
	abortAt   int64
}

func (r *countingRepeatByteReader) Read(p []byte) (int, error) {
	if r.abortAt > 0 && r.bytesRead >= r.abortAt {
		return 0, errors.New("test safety ceiling exceeded: unbounded read")
	}
	for i := range p {
		p[i] = r.fill
	}
	r.bytesRead += int64(len(p))
	return len(p), nil
}

type errAfterBytesReader struct {
	remaining []byte
	err       error
}

func (r *errAfterBytesReader) Read(p []byte) (int, error) {
	if len(r.remaining) == 0 {
		return 0, r.err
	}
	n := copy(p, r.remaining)
	r.remaining = r.remaining[n:]
	return n, nil
}

type repeatedLineReader struct {
	prefix  []byte
	line    []byte
	repeats int
	offset  int
}

func (r *repeatedLineReader) Read(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if len(r.prefix) > 0 {
			n := copy(p[written:], r.prefix)
			r.prefix = r.prefix[n:]
			written += n
			continue
		}
		if r.repeats == 0 {
			if written == 0 {
				return 0, io.EOF
			}
			return written, nil
		}
		n := copy(p[written:], r.line[r.offset:])
		r.offset += n
		written += n
		if r.offset == len(r.line) {
			r.offset = 0
			r.repeats--
		}
	}
	return written, nil
}

func mustEncodeTestMetaLine(t *testing.T, seed byte) []byte {
	t.Helper()
	sel := testSelectionSpec(t, seed)
	opID := testID16[receivecontract.OperationID](seed + 1)
	resID := testID16[receivecontract.DestinationReservationID](seed + 2)
	var authRaw [receivecontract.AuthorityRefBytes]byte
	authRaw[0] = seed + 3
	authRef, err := receivecontract.AuthorityRefFromBytes(authRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	layout, err := receivecontract.NewCompleteDirectoryResultRoot(testID16[catalog.DirectoryID](seed+4), "project")
	if err != nil {
		t.Fatal(err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(layout)
	if err != nil {
		t.Fatal(err)
	}
	res, err := receivecontract.NewNativeNamedEntryReservation(opID, resID, art, authRef, "project", "project", 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := receivecontract.NewDirectTreePlan(art, res)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transfer.NewReceiveIntent(sel, art, plan)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := metaToRecord(taskMeta{
		Intent:          intent,
		SelectionDigest: intent.SelectionSpec().Digest(),
		Lifecycle:       journalLifecycleActive,
		StateGen:        initialStateGeneration,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	line, err := encodeJournalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return line
}

func TestReadBoundedJournalLineBoundsAndErrors(t *testing.T) {
	const (
		bufSize  = 16
		maxBytes = 40
	)

	t.Run("normal and multi-buffer lines up to exact limit stay aligned", func(t *testing.T) {
		exactLine := strings.Repeat("x", maxBytes-1) + "\n"
		shortLine := "ok\n"
		reader := bufio.NewReaderSize(strings.NewReader(exactLine+shortLine), bufSize)

		got1, err := readBoundedJournalLine(reader, maxBytes)
		if err != nil || string(got1) != exactLine {
			t.Fatalf("first line = (%q, %v), want (%q, nil)", got1, err, exactLine)
		}
		got2, err := readBoundedJournalLine(reader, maxBytes)
		if err != nil || string(got2) != shortLine {
			t.Fatalf("second line = (%q, %v), want (%q, nil)", got2, err, shortLine)
		}
		got3, err := readBoundedJournalLine(reader, maxBytes)
		if len(got3) != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("eof = (%q, %v), want (\"\", io.EOF)", got3, err)
		}
	})

	t.Run("rejects line exceeding maxBytes within single buffer or across buffers", func(t *testing.T) {
		readerSmallCap := bufio.NewReaderSize(strings.NewReader("123456789\n"), bufSize)
		if _, err := readBoundedJournalLine(readerSmallCap, 8); !errors.Is(err, errJournalCorrupt) {
			t.Fatalf("single-buffer oversized = %v, want errJournalCorrupt", err)
		}

		readerMulti := bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", maxBytes)+"\n"), bufSize)
		if _, err := readBoundedJournalLine(readerMulti, maxBytes); !errors.Is(err, errJournalCorrupt) {
			t.Fatalf("multi-buffer oversized = %v, want errJournalCorrupt", err)
		}
	})

	t.Run("preserves multi-buffer torn tail at EOF without newline", func(t *testing.T) {
		tail := strings.Repeat("t", maxBytes)
		reader := bufio.NewReaderSize(strings.NewReader(tail), bufSize)
		got, err := readBoundedJournalLine(reader, maxBytes)
		if !errors.Is(err, io.EOF) || string(got) != tail {
			t.Fatalf("torn tail = (%q, %v), want (%q, io.EOF)", got, err, tail)
		}
	})

	t.Run("preserves underlying I/O errors instead of misclassifying as corrupt", func(t *testing.T) {
		errBoom := errors.New("disk read failed")
		r1 := bufio.NewReaderSize(&errAfterBytesReader{remaining: []byte("part"), err: errBoom}, bufSize)
		if _, err := readBoundedJournalLine(r1, maxBytes); !errors.Is(err, errBoom) {
			t.Fatalf("single-buffer io error = %v, want %v", err, errBoom)
		}

		r2 := bufio.NewReaderSize(&errAfterBytesReader{remaining: bytes.Repeat([]byte("b"), bufSize+5), err: errBoom}, bufSize)
		if _, err := readBoundedJournalLine(r2, maxBytes); !errors.Is(err, errBoom) {
			t.Fatalf("multi-buffer io error = %v, want %v", err, errBoom)
		}
	})
}

func TestReadJournalStateBoundsOversizedCorruptLine(t *testing.T) {
	metaLine := mustEncodeTestMetaLine(t, 0xe1)
	tail := &countingRepeatByteReader{
		fill:    'a',
		abortAt: int64(2 * journalMaxLineBytes),
	}
	stream := io.MultiReader(bytes.NewReader(metaLine), tail)

	_, _, err := readJournalStateFromReader(stream)
	if !errors.Is(err, errJournalCorrupt) {
		t.Fatalf("readJournalStateFromReader on oversized corrupt line = %v, want errJournalCorrupt", err)
	}
	maxExpectedRead := int64(journalMaxLineBytes + journalStateReadBufferBytes)
	if tail.bytesRead > maxExpectedRead {
		t.Fatalf("oversized line read %d bytes from tail, want <= %d", tail.bytesRead, maxExpectedRead)
	}
}

func TestReadJournalStateAllowsLargeLogsAndMultiBufferLines(t *testing.T) {
	metaLine := mustEncodeTestMetaLine(t, 0xe7)
	fid := testID16[catalog.FileID](0xe8)
	rev := testID16[content.FileRevision](0xe9)

	t.Run("multi-line journal exceeding journalMaxLineBytes replays without truncation", func(t *testing.T) {
		deliveredLine, err := encodeJournalLine(deliveredToRecord(deliveredEntry{
			FileID:    fid,
			Path:      "project/" + strings.Repeat("segment/", 250) + "file.bin",
			Revision:  rev,
			ExactSize: 4096,
		}))
		if err != nil {
			t.Fatal(err)
		}
		repeats := (journalMaxLineBytes / len(deliveredLine)) + 16
		stream := &repeatedLineReader{
			prefix:  metaLine,
			line:    deliveredLine,
			repeats: repeats,
		}
		state, validBytes, err := readJournalStateFromReader(stream)
		if err != nil {
			t.Fatalf("readJournalStateFromReader on >4MiB multi-line log = %v", err)
		}
		if state.tornTail {
			t.Fatal("expected tornTail=false for newline-terminated >4MiB log")
		}
		wantBytes := int64(len(metaLine)) + int64(repeats)*int64(len(deliveredLine))
		if validBytes <= journalMaxLineBytes || validBytes != wantBytes {
			t.Fatalf("validBytes = %d, want %d (> %d)", validBytes, wantBytes, journalMaxLineBytes)
		}
		if state.lineCount != 1+repeats {
			t.Fatalf("lineCount = %d, want %d", state.lineCount, 1+repeats)
		}
	})

	t.Run("valid multi-buffer line and multi-buffer torn tail", func(t *testing.T) {
		ranges := make([]content.Range, 1500)
		for i := range ranges {
			off := uint64(i * 4096)
			ranges[i] = content.Range{Offset: off, End: off + 2048}
		}
		partialLine, err := encodeJournalLine(partialToRecord(partialEntry{
			FileID:        fid,
			Path:          "project/fragmented.bin",
			Revision:      rev,
			ExactSize:     uint64(len(ranges) * 4096),
			PartName:      "fragmented.part",
			Ranges:        ranges,
			CheckpointGen: 1,
			Fingerprint:   [stageFingerprintBytes]byte{0x42},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if len(partialLine) <= journalStateReadBufferBytes {
			t.Fatalf("test setup expected partialLine > %d bytes, got %d", journalStateReadBufferBytes, len(partialLine))
		}
		deliveredLine, err := encodeJournalLine(deliveredToRecord(deliveredEntry{
			FileID:    testID16[catalog.FileID](0xea),
			Path:      "project/after.bin",
			Revision:  rev,
			ExactSize: 128,
		}))
		if err != nil {
			t.Fatal(err)
		}
		tornTail := bytes.Repeat([]byte("z"), journalStateReadBufferBytes+512)

		stream := io.MultiReader(
			bytes.NewReader(metaLine),
			bytes.NewReader(partialLine),
			bytes.NewReader(deliveredLine),
			bytes.NewReader(tornTail),
		)
		state, validBytes, err := readJournalStateFromReader(stream)
		if err != nil {
			t.Fatalf("readJournalStateFromReader with multi-buffer line and torn tail = %v", err)
		}
		if !state.tornTail {
			t.Fatal("expected tornTail=true for unterminated multi-buffer tail")
		}
		wantValid := int64(len(metaLine) + len(partialLine) + len(deliveredLine))
		if validBytes != wantValid {
			t.Fatalf("validBytes = %d, want %d", validBytes, wantValid)
		}
		if got := len(state.partials["project/fragmented.bin"].Ranges); got != len(ranges) {
			t.Fatalf("partial range count = %d, want %d", got, len(ranges))
		}
		if _, ok := state.delivered["project/after.bin"]; !ok {
			t.Fatal("missing delivered entry after multi-buffer partial line")
		}
	})
}
