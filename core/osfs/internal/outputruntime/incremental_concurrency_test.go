package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestIncrementalConcurrentCheckpointOwnershipHasSingleWinner(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	intent := currentCoverageIntent(t, rootSpec.path, 0xa1, 0xa2)
	start := make(chan struct{})
	holdWinner := make(chan struct{})
	type result struct {
		session *incrementalOutputSession
		err     error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for range 2 {
		authority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
		workers.Go(func() {
			<-start
			opened, err := authority.OpenOutput(context.Background(), intent)
			if err != nil {
				results <- result{err: err}
				return
			}
			session := opened.(*incrementalOutputSession)
			results <- result{session: session}
			<-holdWinner
			_, _ = session.PauseJob(context.Background(), transfer.JobPauseInterrupted)
		})
	}
	close(start)
	first, second := <-results, <-results
	winners, busy := 0, 0
	for _, current := range []result{first, second} {
		switch {
		case current.err == nil && current.session != nil:
			winners++
		case errors.Is(current.err, outputcap.ErrNamespaceLockBusy):
			busy++
		default:
			t.Fatalf("concurrent checkpoint claim = session:%p err:%v", current.session, current.err)
		}
	}
	if winners != 1 || busy != 1 {
		t.Fatalf("concurrent checkpoint claims = winners:%d busy:%d", winners, busy)
	}
	close(holdWinner)
	workers.Wait()
}

func TestIncrementalDirectoryFinalizationExcludesConcurrentBeginFile(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0xb1, 0xb2)
	parent := currentCoverageRoot(t, session, intent, 0xb3)
	file := currentCoverageFile(t, session, intent, "late-child.bin", parent, 0xb4, 0xb5, 4)
	directory := session.directories[""].directory

	// Hold the inner operation gate so FinalizeDirectory pauses only after taking
	// the wrapper admission mutex. That gives the test a deterministic cut at the
	// exact boundary which used to admit a late child.
	session.inner.operationGate.Lock()
	gateHeld := true
	defer func() {
		if gateHeld {
			session.inner.operationGate.Unlock()
		}
	}()
	finalized := make(chan error, 1)
	go func() { finalized <- session.FinalizeDirectory(context.Background(), directory) }()
	deadline := time.Now().Add(time.Second)
	for session.mu.TryLock() {
		session.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("FinalizeDirectory did not retain the admission mutex")
		}
		runtime.Gosched()
	}

	type beginResult struct {
		start transfer.FileStart
		err   error
	}
	begun := make(chan beginResult, 1)
	go func() {
		start, err := session.BeginFile(context.Background(), file)
		begun <- beginResult{start: start, err: err}
	}()
	select {
	case result := <-begun:
		t.Fatalf("BeginFile crossed an in-progress finalization: %+v, %v", result.start, result.err)
	case <-time.After(25 * time.Millisecond):
	}

	session.inner.operationGate.Unlock()
	gateHeld = false
	if err := <-finalized; err != nil {
		t.Fatal(err)
	}
	result := <-begun
	if !errors.Is(result.err, transfer.ErrDirectoryAdmissionMismatch) {
		t.Fatalf("late BeginFile error = %v", result.err)
	}
	if _, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}
}

func TestIncrementalConcurrentGettersRemainStableDuringPause(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	session, intent := currentCoverageSession(t, rootSpec, 0xb1, 0xb2)
	_ = currentCoverageRoot(t, session, intent, 0xb3)
	wantBackend, wantSession, wantCapabilities := session.BackendID(), session.SessionID(), session.Capabilities()
	start := make(chan struct{})
	failures := make(chan string, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			<-start
			for range 256 {
				if session.BackendID() != wantBackend || session.SessionID() != wantSession ||
					session.Capabilities() != wantCapabilities {
					failures <- "immutable session identity changed during pause"
					return
				}
			}
		})
	}
	workers.Go(func() {
		<-start
		if settlement, err := session.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil ||
			settlement.Kind() != transfer.JobPaused {
			failures <- "pause failed while getters were active"
		}
	})
	close(start)
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}

func TestIncrementalConcurrentPublicationKeepsRecoveredObjectIdentity(t *testing.T) {
	rootSpec := newRuntimeTestRootSpec(t)
	first, intent := currentCoverageSession(t, rootSpec, 0xc1, 0xc2)
	parent := currentCoverageRoot(t, first, intent, 0xc3)
	file := currentCoverageFile(t, first, intent, "concurrent-publish.bin", parent, 0xc4, 0xc5, 4)
	transaction := currentCoverageTransaction(t, first, file)
	if err := transaction.WriteRange(context.Background(), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	checkpointObject := firstIncrementalCheckpoint(first.inner).OwnedOutputObject().Bytes()
	if _, err := transaction.Pause(context.Background(), transfer.FilePauseInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PauseJob(context.Background(), transfer.JobPauseInterrupted); err != nil {
		t.Fatal(err)
	}

	reopenedAuthority := newIncrementalTestAuthority(t, rootSpec.path, openOutputRuntimeTestPlatform)
	opened, err := reopenedAuthority.OpenOutput(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	reopened := opened.(*incrementalOutputSession)
	reopenedParent := currentCoverageRoot(t, reopened, intent, 0xc6)
	reopenedFile := currentCoverageFile(t, reopened, intent, file.Path, reopenedParent, 0xc4, 0xc5, 4)
	recovered := currentCoverageTransaction(t, reopened, reopenedFile)
	wantBinding := recovered.Binding()
	if !bytes.Equal(wantBinding.ObjectIdentity().Bytes(), checkpointObject) {
		t.Fatal("recovery allocated a new output object")
	}

	start := make(chan struct{})
	failures := make(chan string, 8)
	settlements := make(chan transfer.FileSettlement, 1)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			for range 256 {
				binding := recovered.Binding()
				if binding.OutputSessionID() != wantBinding.OutputSessionID() ||
					!bytes.Equal(binding.ObjectIdentity().Bytes(), checkpointObject) {
					failures <- "publication changed the recovered binding"
					return
				}
				_ = reopened.BackendID()
				_ = reopened.SessionID()
			}
		})
	}
	workers.Go(func() {
		<-start
		settlement, commitErr := recovered.Commit(context.Background())
		if commitErr != nil {
			failures <- "concurrent commit failed: " + commitErr.Error()
			return
		}
		settlements <- settlement
	})
	close(start)
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	select {
	case settlement := <-settlements:
		if settlement.Kind() != transfer.FilePublished ||
			!bytes.Equal(settlementBindingObject(t, settlement), checkpointObject) {
			t.Fatalf("concurrent publication settlement = %+v", settlement)
		}
	default:
		t.Fatal("concurrent publication produced no settlement")
	}
	published := firstIncrementalCheckpoint(reopened.inner)
	if published.CommitState() != resumestate.FileCheckpointCommitPublished ||
		!bytes.Equal(published.OwnedOutputObject().Bytes(), checkpointObject) {
		t.Fatalf("published checkpoint lost recovered identity: %+v", published)
	}
	_, _ = reopened.PauseJob(context.Background(), transfer.JobPauseInterrupted)
}

func settlementBindingObject(t *testing.T, settlement transfer.FileSettlement) []byte {
	t.Helper()
	binding, ok := settlement.OutputBinding()
	if !ok {
		t.Fatal("file settlement has no output binding")
	}
	return binding.ObjectIdentity().Bytes()
}
