package v2route

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/link"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestFileTombstoneStorePersistsAndRejectsCorruption(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "relay", "stopped.bin")
	store, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := validFileTombstone(t)
	if outcome, err := store.Commit(context.Background(), tombstone); err != nil || outcome != CommitCommitted {
		t.Fatal(err)
	}
	reopened, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Load(context.Background())
	if err != nil || len(loaded) != 1 || loaded[0] != tombstone {
		t.Fatalf("loaded tombstones = %+v, %v", loaded, err)
	}
	encoded, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 1
	if err := os.WriteFile(filename, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Load(context.Background()); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("corrupt tombstone error = %v", err)
	}
}

func TestFileTombstoneStoreHonorsContextAndInputValidation(t *testing.T) {
	store, err := NewFileTombstoneStore(filepath.Join(t.TempDir(), "stopped.bin"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load error = %v", err)
	}
	if _, err := store.Commit(context.Background(), Tombstone{}); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("invalid put error = %v", err)
	}
}

func TestFileTombstoneCommitRollsBackDefiniteAppendFailures(t *testing.T) {
	for _, operation := range []string{"write", "sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "stopped.bin")
			store, err := NewFileTombstoneStore(filename)
			if err != nil {
				t.Fatal(err)
			}
			before := fileSize(t, filename)
			plan := &faultStopFileOpener{phase: 1, operation: operation}
			store.open = plan.open
			outcome, err := store.Commit(context.Background(), validFileTombstone(t))
			if outcome != CommitNotCommitted || err == nil {
				t.Fatalf("Commit outcome = %d, %v", outcome, err)
			}
			if size := fileSize(t, filename); size != before {
				t.Fatalf("rollback size = %d, want %d", size, before)
			}
			loaded, loadErr := store.Load(context.Background())
			if loadErr != nil || len(loaded) != 0 {
				t.Fatalf("rollback records = %+v, %v", loaded, loadErr)
			}
		})
	}
}

func TestFileTombstoneCommitReportsUnknownWhenRollbackFails(t *testing.T) {
	for _, operation := range []string{"truncate", "sync", "close"} {
		t.Run(operation, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "stopped.bin")
			store, err := NewFileTombstoneStore(filename)
			if err != nil {
				t.Fatal(err)
			}
			plan := &faultStopFileOpener{
				phase: 1, operation: "write", rollbackPhase: 2, rollbackOperation: operation,
			}
			store.open = plan.open
			outcome, err := store.Commit(context.Background(), validFileTombstone(t))
			if outcome != CommitUnknown || !errors.Is(err, ErrCommitUncertain) {
				t.Fatalf("Commit outcome = %d, %v", outcome, err)
			}
		})
	}
}

func TestFileTombstoneCommitIsIdempotentAndRejectsShareConflict(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "stopped.bin")
	store, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	record := validFileTombstone(t)
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("initial Commit = %d, %v", outcome, err)
	}
	committedSize := fileSize(t, filename)
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("idempotent Commit = %d, %v", outcome, err)
	}
	if size := fileSize(t, filename); size != committedSize {
		t.Fatalf("idempotent Commit appended bytes: %d != %d", size, committedSize)
	}
	conflict := record
	conflict.StopID[0] ^= 0x80
	if outcome, err := store.Commit(context.Background(), conflict); outcome != CommitUnknown ||
		!errors.Is(err, ErrTombstoneConflict) {
		t.Fatalf("conflicting Commit = %d, %v", outcome, err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil || len(loaded) != 1 || loaded[0] != record {
		t.Fatalf("records after conflict = %+v, %v", loaded, err)
	}
}

func TestFileTombstoneExactRetryRequiresCleanResync(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "stopped.bin")
	store, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	record := validFileTombstone(t)
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("initial Commit = %d, %v", outcome, err)
	}
	store.open = (&faultStopFileOpener{phase: 1, operation: "sync"}).open
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitUnknown ||
		!errors.Is(err, ErrCommitUncertain) {
		t.Fatalf("ambiguous exact retry = %d, %v", outcome, err)
	}
	store.open = openProductionStopFile
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("resolved exact retry = %d, %v", outcome, err)
	}
}

func TestFileTombstoneStartupRecoversOnlyPartialTail(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "stopped.bin")
	store, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatal(err)
	}
	record := validFileTombstone(t)
	if outcome, err := store.Commit(context.Background(), record); outcome != CommitCommitted || err != nil {
		t.Fatalf("initial Commit = %d, %v", outcome, err)
	}
	durableSize := fileSize(t, filename)
	appendRaw(t, filename, make([]byte, tombstoneRecordSize/2))
	recovered, err := NewFileTombstoneStore(filename)
	if err != nil {
		t.Fatalf("recover partial tail: %v", err)
	}
	if size := fileSize(t, filename); size != durableSize {
		t.Fatalf("partial recovery size = %d, want %d", size, durableSize)
	}
	loaded, err := recovered.Load(context.Background())
	if err != nil || len(loaded) != 1 || loaded[0] != record {
		t.Fatalf("durable record after partial recovery = %+v, %v", loaded, err)
	}

	appendRaw(t, filename, make([]byte, tombstoneRecordSize))
	if _, err := NewFileTombstoneStore(filename); !errors.Is(err, ErrTombstoneFile) {
		t.Fatalf("full corrupt record startup error = %v", err)
	}
}

var errInjectedStopFile = errors.New("injected STOP file failure")

type faultStopFileOpener struct {
	mu                sync.Mutex
	opens             int
	phase             int
	operation         string
	rollbackPhase     int
	rollbackOperation string
}

func (opener *faultStopFileOpener) open(path string, flag int, perm os.FileMode) (stopFile, error) {
	file, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}
	opener.mu.Lock()
	opener.opens++
	phase := opener.opens
	operation := ""
	if phase == opener.phase {
		operation = opener.operation
	}
	if phase == opener.rollbackPhase {
		operation = opener.rollbackOperation
	}
	opener.mu.Unlock()
	return &faultStopFile{File: file, operation: operation}, nil
}

type faultStopFile struct {
	*os.File
	operation string
	failed    bool
}

func (file *faultStopFile) Write(value []byte) (int, error) {
	if file.operation != "write" || file.failed {
		return file.File.Write(value)
	}
	file.failed = true
	written, _ := file.File.Write(value[:len(value)/2])
	return written, errInjectedStopFile
}

func (file *faultStopFile) Sync() error {
	if file.operation != "sync" || file.failed {
		return file.File.Sync()
	}
	file.failed = true
	return errors.Join(file.File.Sync(), errInjectedStopFile)
}

func (file *faultStopFile) Truncate(size int64) error {
	if file.operation != "truncate" || file.failed {
		return file.File.Truncate(size)
	}
	file.failed = true
	return errInjectedStopFile
}

func (file *faultStopFile) Close() error {
	if file.operation != "close" || file.failed {
		return file.File.Close()
	}
	file.failed = true
	return errors.Join(file.File.Close(), errInjectedStopFile)
}

func fileSize(t *testing.T, filename string) int64 {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func appendRaw(t *testing.T, filename string, value []byte) {
	t.Helper()
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(file, bytes.NewReader(value)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func validFileTombstone(t *testing.T) Tombstone {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkHashRaw, err := link.SenderKeyHash(public)
	if err != nil {
		t.Fatal(err)
	}
	shareIDText, err := link.ShareIDForSenderKeyHash(pkHashRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(shareIDText)
	if err != nil {
		t.Fatal(err)
	}
	shareID, _ := v2.ShareIDFromBytes(shareIDRaw)
	pkHash, _ := v2.PKHashFromBytes(pkHashRaw[:])
	shareInstance, _ := v2.ShareInstanceFromBytes([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	return Tombstone{
		ShareID: shareID, ShareInstance: shareInstance, PKHash: pkHash,
		StopID: v2.StopID{1},
	}
}
