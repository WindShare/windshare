package outputruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3FilePauseWithExpiredDeadlineClosesAndReturnsLastVerifiedCheckpoint(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 2)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, 2)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
	lastVerified, err := transaction.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteRange(context.Background(), 1, []byte{2}); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	settlement, pauseErr := transaction.Pause(expired, transfer.FilePauseInterrupted)
	checkpoint, ok := settlement.VerifiedCheckpoint()
	if !errors.Is(pauseErr, context.DeadlineExceeded) || settlement.Kind() != transfer.FilePaused || !ok ||
		checkpoint.CheckpointGeneration() != lastVerified.CheckpointGeneration() ||
		!slices.Equal(checkpoint.Ranges().Ranges(), lastVerified.Ranges().Ranges()) {
		t.Fatalf("expired pause = settlement %v checkpoint=%v error=%v; want last verified checkpoint %v",
			settlement.Kind(), checkpoint.Ranges().Ranges(), pauseErr, lastVerified.Ranges().Ranges())
	}
	if err := transaction.WriteRange(context.Background(), 0, []byte{9}); !errors.Is(err, outputfault.ErrSessionClosed) {
		t.Fatalf("expired pause left transaction writable: %v", err)
	}

	resumed, err := opened.Session.BeginFile(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	resumedTransaction, durable, ok := resumed.Transaction()
	if !ok || durable.CheckpointGeneration() != lastVerified.CheckpointGeneration() ||
		!slices.Equal(durable.Ranges().Ranges(), lastVerified.Ranges().Ranges()) {
		t.Fatalf("same-session resume durable ranges = %v, want %v",
			durable.Ranges().Ranges(), lastVerified.Ranges().Ranges())
	}
	if _, err := resumedTransaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
		t.Fatal(err)
	}
	completed, err := opened.Session.CompleteJob(context.Background(), transfer.JobCompletedWithErrors)
	if err != nil || completed.Kind() != transfer.JobClosed {
		t.Fatalf("complete transaction-level deadline fixture = (%v, %v)", completed.Kind(), err)
	}
}

func TestOutputV3JobPauseDeadlineExpiryClosesEveryActiveFileAndReopens(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	paths := make([]string, maxFilesystemOutputTransactions)
	for index := range paths {
		paths[index] = fmt.Sprintf("file-%02d.bin", index)
	}
	selection := v3RecoverySelectionPaths(t, paths, 1)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	transactions := make([]transfer.FileTransaction, 0, len(paths))
	for index := range paths {
		start, err := opened.Session.BeginFile(
			context.Background(), v3RecoveryOutputFileAt(t, opened.Session, selection, index),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("file %d did not begin an active transaction", index)
		}
		transactions = append(transactions, transaction)
	}

	expiresDuringSettlement := newV3RecoveryExpiresAfterAdmissionContext()
	_, pauseErr := opened.Session.PauseJob(expiresDuringSettlement, transfer.JobPauseInterrupted)
	if !errors.Is(pauseErr, context.DeadlineExceeded) {
		t.Fatalf("mid-settlement deadline error = %v, want deadline exceeded", pauseErr)
	}
	opened.Session.mu.Lock()
	active, closed := len(opened.Session.active), opened.Session.closed
	opened.Session.mu.Unlock()
	if active != 0 || !closed {
		t.Fatalf("deadline pause left session active=%d closed=%v", active, closed)
	}
	for index, transaction := range transactions {
		if err := transaction.WriteRange(context.Background(), 0, []byte{1}); !errors.Is(err, outputfault.ErrSessionClosed) {
			t.Fatalf("transaction %d survived deadline pause: %v", index, err)
		}
	}

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	for index := range paths {
		start, err := reopened.Session.BeginFile(
			context.Background(), v3RecoveryOutputFileAt(t, reopened.Session, selection, index),
		)
		if err != nil {
			t.Fatal(err)
		}
		transaction, _, ok := start.Transaction()
		if !ok {
			t.Fatalf("reopened file %d did not recover its transaction", index)
		}
		if _, err := transaction.Retire(context.Background(), transfer.FileRetireExplicitPolicySkip); err != nil {
			t.Fatal(err)
		}
	}
	completed, err := reopened.Session.CompleteJob(context.Background(), transfer.JobCompletedWithErrors)
	if err != nil || completed.Kind() != transfer.JobClosed {
		t.Fatalf("complete reopened deadline fixture = (%v, %v)", completed.Kind(), err)
	}
}

type v3RecoveryExpiresAfterAdmissionContext struct {
	mu      sync.Mutex
	done    chan struct{}
	expired bool
}

func newV3RecoveryExpiresAfterAdmissionContext() *v3RecoveryExpiresAfterAdmissionContext {
	return &v3RecoveryExpiresAfterAdmissionContext{done: make(chan struct{})}
}

func (ctx *v3RecoveryExpiresAfterAdmissionContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (ctx *v3RecoveryExpiresAfterAdmissionContext) Done() <-chan struct{} { return ctx.done }

func (ctx *v3RecoveryExpiresAfterAdmissionContext) Err() error {
	ctx.mu.Lock()
	defer ctx.mu.Unlock()
	if !ctx.expired {
		ctx.expired = true
		close(ctx.done)
		return nil
	}
	return context.DeadlineExceeded
}

func (*v3RecoveryExpiresAfterAdmissionContext) Value(any) any { return nil }
