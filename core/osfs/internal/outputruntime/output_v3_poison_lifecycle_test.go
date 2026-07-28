package outputruntime

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3SettlementTransitionFailureReleasesOwner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		fault     stateStoreFaultPoint
		pause     bool
		fileCount int
	}{
		{name: "pause-unchanged", fault: stateStoreFaultCreate, pause: true, fileCount: 2},
		{name: "pause-uncertain", fault: stateStoreFaultInstalledReopen, pause: true, fileCount: 2},
		{name: "complete-unchanged", fault: stateStoreFaultCreate},
		{name: "complete-uncertain", fault: stateStoreFaultInstalledReopen},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			paths := make([]string, test.fileCount)
			for index := range paths {
				paths[index] = "pause-file-" + string(rune('a'+index)) + ".bin"
			}
			selection := v3RecoverySelectionPaths(t, paths, 1)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)

			transactions := make([]transfer.FileTransaction, 0, test.fileCount)
			for index := range paths {
				file := v3RecoveryOutputFileAt(t, opened.Session, selection, index)
				transactions = append(transactions, v3RecoveryBeginTransaction(t, opened.Session, file))
			}
			opened.Session.sessionDir = &stateStoreFaultDirectory{
				Directory: opened.Session.sessionDir,
				fault:     test.fault,
				target:    resumestate.HeaderRecordName,
			}

			var settleErr error
			if test.pause {
				_, settleErr = opened.Session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
			} else {
				_, settleErr = opened.Session.CompleteJob(context.Background(), transfer.JobSucceeded)
			}
			if !errors.Is(settleErr, errStateStoreInjected) {
				t.Fatalf("settlement transition error = %v, want injected state-store failure", settleErr)
			}
			requireV3OwnerReleased(t, opened.Session)
			for index, transaction := range transactions {
				if err := transaction.WriteRange(context.Background(), 0, []byte{1}); !errors.Is(err, outputfault.ErrSessionClosed) {
					t.Fatalf("transaction %d remained writable after transition failure: %v", index, err)
				}
			}

			// The failed owner has no close API to retry. Immediate reopen therefore
			// proves that the failure path itself released session.lock.
			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3ConcurrentWriteWaitingAtPoisonCannotWrite(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, []string{"poison.bin", "writer.bin"}, 1)
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	poisoned := v3RecoveryBeginTransaction(
		t, opened.Session, v3RecoveryOutputFileAt(t, opened.Session, selection, 0),
	).(*FileTransaction)
	writer := v3RecoveryBeginTransaction(
		t, opened.Session, v3RecoveryOutputFileAt(t, opened.Session, selection, 1),
	).(*FileTransaction)

	writes := &atomic.Int64{}
	writer.data = stagedData{file: &v3CountingOutputFile{File: writer.data.file, writes: writes}}
	writer.mu.Lock()
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- writer.WriteRange(context.Background(), 0, []byte{9})
	}()
	waitForV3OperationReader(t, opened.Session)

	if err := poisoned.WriteRange(context.Background(), 0, []byte{1}); err != nil {
		writer.mu.Unlock()
		t.Fatal(err)
	}
	poisoned.recordDir = &stateStoreFaultDirectory{
		Directory: poisoned.recordDir,
		fault:     stateStoreFaultInstalledReopen,
		target:    poisoned.recordName,
	}
	if _, err := poisoned.Checkpoint(context.Background()); !errors.Is(err, errStateStoreInjected) {
		writer.mu.Unlock()
		t.Fatalf("poisoning checkpoint error = %v, want injected installed-target uncertainty", err)
	}
	writer.mu.Unlock()

	select {
	case err := <-writeResult:
		if !errors.Is(err, outputfault.ErrSessionClosed) {
			t.Fatalf("write admitted before poison lock completed = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("write waiting at poison did not drain")
	}
	if writes.Load() != 0 {
		t.Fatalf("writes after installed-target uncertainty = %d, want zero", writes.Load())
	}
	requireV3OwnerReleased(t, opened.Session)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	v3RecoveryCloseSession(t, reopened.Session)
}

type v3CountingOutputFile struct {
	outputcap.File
	writes *atomic.Int64
}

func (file *v3CountingOutputFile) WriteAt(data []byte, offset int64) (int, error) {
	file.writes.Add(1)
	return file.File.WriteAt(data, offset)
}

func waitForV3OperationReader(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !session.operationGate.TryLock() {
			return
		}
		session.operationGate.Unlock()
		runtime.Gosched()
	}
	t.Fatal("transaction did not enter the session operation gate")
}

func requireV3OwnerReleased(t *testing.T, session *Session) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		released := session.closed && len(session.active) == 0 && session.sessionLock == nil &&
			session.filesDir == nil && session.anchorsDir == nil && session.stagesDir == nil &&
			session.sessionDir == nil && session.intentDir == nil && session.control == nil && session.platform == nil
		session.mu.Unlock()
		if released {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("session owner did not close every transaction, handle, and lock")
}
