package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3RejectsRangeOverlapBeforeMutatingCheckpointAuthority(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 4)
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, 4)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)

	if err := transaction.WriteRange(context.Background(), 0, []byte("ab")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantPrefix := []byte{'a', 'b', 0, 0}
	requireOutputV3TransactionBytes(t, transaction, wantPrefix)
	requireOutputV3RangeOverlap(t, transaction.WriteRange(context.Background(), 0, []byte("XY")))
	requireOutputV3TransactionBytes(t, transaction, wantPrefix)
	requireOutputV3RangeOverlap(t, transaction.WriteRange(context.Background(), 1, []byte("XYZ")))
	requireOutputV3TransactionBytes(t, transaction, wantPrefix)

	paused, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted)
	if err != nil || paused.Kind() != transfer.FilePaused {
		t.Fatalf("pause checkpointed prefix = (kind=%v, err=%v)", paused.Kind(), err)
	}
	jobSettlement, err := opened.Session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
	if err != nil || jobSettlement.Kind() != transfer.JobPaused {
		t.Fatalf("pause output session = (kind=%v, err=%v)", jobSettlement.Kind(), err)
	}

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	resumedFile := v3RecoveryOutputFile(t, reopened.Session, selection, 4)
	start, err := reopened.Session.BeginFile(context.Background(), resumedFile)
	if err != nil {
		t.Fatal(err)
	}
	resumed, durable, ok := start.Transaction()
	if !ok {
		t.Fatal("checkpointed file did not resume as a transaction")
	}
	ranges := durable.Ranges().Ranges()
	if len(ranges) != 1 || ranges[0].Offset != 0 || ranges[0].End != 2 {
		t.Fatalf("resumed durable ranges = %v, want [0,2)", ranges)
	}
	concrete := resumed.(*filesystemFileTransaction)
	if err := concrete.WriteRange(context.Background(), 2, []byte("c")); err != nil {
		t.Fatal(err)
	}
	requireOutputV3RangeOverlap(t, concrete.WriteRange(context.Background(), 2, []byte("X")))
	requireOutputV3TransactionBytes(t, concrete, []byte{'a', 'b', 'c', 0})
	if err := concrete.WriteRange(context.Background(), 3, []byte("d")); err != nil {
		t.Fatal(err)
	}
	settlement, err := concrete.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish resumed suffix = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	jobSettlement, err = reopened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || jobSettlement.Kind() != transfer.JobClosed {
		t.Fatalf("complete resumed output session = (kind=%v, err=%v)", jobSettlement.Kind(), err)
	}
	actual, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if err != nil || !bytes.Equal(actual, []byte("abcd")) {
		t.Fatalf("published bytes = %q, err=%v, want %q", actual, err, "abcd")
	}
}

func TestOutputV3CommitReturnsTransactionBoundQuarantine(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("owned")
	foreign := []byte("other")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	binding := transaction.Binding()
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}

	originalReducer := transaction.reduceFile
	transaction.reduceFile = func(
		ctx context.Context,
		output transfer.OutputFile,
		resumable resumestate.ResumableFileAuthority,
		recordDir outputV3Directory,
		recordName string,
	) (transfer.FileStart, error) {
		finalPath := filepath.Join(root, v3RecoveryFilePath)
		if err := os.Remove(finalPath); err != nil {
			return transfer.FileStart{}, err
		}
		if err := os.WriteFile(finalPath, foreign, 0o600); err != nil {
			return transfer.FileStart{}, err
		}
		return originalReducer(ctx, output, resumable, recordDir, recordName)
	}

	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("commit publication race = (kind=%v, err=%v)", settlement.Kind(), err)
	}
	settledBinding, bound := settlement.OutputBinding()
	if !bound || settledBinding != binding {
		t.Fatalf("quarantine binding = (%+v, %t), want exact transaction binding", settledBinding, bound)
	}
	reference, reason, ok := settlement.Quarantine()
	if !ok || reference.OutputSessionID() != binding.OutputSessionID() ||
		reference.LocatorDigest() != binding.Locator().Digest() || reason != transfer.QuarantinePublicationAmbiguous {
		t.Fatalf("quarantine authority = (ref=%+v, reason=%v, ok=%t)", reference, reason, ok)
	}
	actual, readErr := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if readErr != nil || !bytes.Equal(actual, foreign) {
		t.Fatalf("foreign final = %q, err=%v, want %q", actual, readErr, foreign)
	}
	jobSettlement, completeErr := opened.Session.CompleteJob(context.Background(), transfer.JobCompletedWithErrors)
	if completeErr != nil || jobSettlement.Kind() != transfer.JobPausedNeedsAttention {
		t.Fatalf("complete quarantined session = (kind=%v, err=%v)", jobSettlement.Kind(), completeErr)
	}
}

func TestOutputV3PublishingTransactionHasExactlyOneTerminalClaimant(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("publish")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}

	reducerEntered := make(chan struct{})
	releaseReducer := make(chan struct{})
	originalReducer := transaction.reduceFile
	transaction.reduceFile = func(
		ctx context.Context,
		output transfer.OutputFile,
		resumable resumestate.ResumableFileAuthority,
		recordDir outputV3Directory,
		recordName string,
	) (transfer.FileStart, error) {
		close(reducerEntered)
		<-releaseReducer
		return originalReducer(ctx, output, resumable, recordDir, recordName)
	}
	type commitResult struct {
		settlement transfer.FileSettlement
		err        error
	}
	committed := make(chan commitResult, 1)
	go func() {
		settlement, err := transaction.Commit(context.Background())
		committed <- commitResult{settlement: settlement, err: err}
	}()
	select {
	case <-reducerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("commit did not reach the durable publishing reducer")
	}
	transaction.mu.Lock()
	phase := transaction.resumable.Bound().Record().Phase()
	lifecycle := transaction.lifecycle
	transaction.mu.Unlock()
	if phase != resumestate.FilePublishing || lifecycle != filesystemFileTransactionSettling {
		t.Fatalf("blocked commit state = (phase=%v, lifecycle=%v)", phase, lifecycle)
	}
	if _, err := transaction.Checkpoint(context.Background()); !errors.Is(err, errOutputSessionClosed) {
		t.Fatalf("checkpoint during publishing error = %v, want transaction ownership rejection", err)
	}
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); !errors.Is(err, errOutputSessionClosed) {
		t.Fatalf("competing pause error = %v, want transaction ownership rejection", err)
	}
	if _, err := transaction.Commit(context.Background()); !errors.Is(err, errOutputSessionClosed) {
		t.Fatalf("competing commit error = %v, want transaction ownership rejection", err)
	}
	close(releaseReducer)
	select {
	case result := <-committed:
		if result.err != nil || result.settlement.Kind() != transfer.FilePublished {
			t.Fatalf("winning commit = (kind=%v, err=%v)", result.settlement.Kind(), result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("winning commit did not finish")
	}
	transaction.mu.Lock()
	lifecycle = transaction.lifecycle
	transaction.mu.Unlock()
	opened.Session.mu.Lock()
	active := len(opened.Session.active)
	opened.Session.mu.Unlock()
	if lifecycle != filesystemFileTransactionClosed || active != 0 {
		t.Fatalf("settled transaction = (lifecycle=%v, active=%d)", lifecycle, active)
	}
	jobSettlement, err := opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err != nil || jobSettlement.Kind() != transfer.JobClosed {
		t.Fatalf("complete published session = (kind=%v, err=%v)", jobSettlement.Kind(), err)
	}
}

func requireOutputV3RangeOverlap(t *testing.T, err error) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.Is(err, errOutputV3RangeOverlap) || !errors.As(err, &fault) ||
		fault.Scope() != transfer.OutputFaultFile || fault.Code() != transfer.OutputFaultContract {
		t.Fatalf("overlap error = %v, want file-scoped output contract fault", err)
	}
}

func requireOutputV3TransactionBytes(
	t *testing.T,
	transaction *filesystemFileTransaction,
	want []byte,
) {
	t.Helper()
	actual := make([]byte, len(want))
	read, err := transaction.data.ReadAt(actual, 0)
	if err != nil || read != len(actual) || !bytes.Equal(actual, want) {
		t.Fatalf("transaction bytes = %q (%d, %v), want %q", actual, read, err, want)
	}
}
