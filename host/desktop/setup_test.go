package desktop

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func setupRequest() Request {
	return Request{Command: "configure", OperationID: "operation", Setup: &SetupInput{ID: "setup:1", RelayURL: "https://chosen.example", FrontURL: "https://front.example"}}
}

func TestSetupResumesOnlyWaitingIntentAndPreservesExplicitChoice(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "cancelled"}[cancelled], func(t *testing.T) {
			a, _, starts := fixture(t)
			var saved atomic.Bool
			a.deps.load = func() (relayconfig.Config, error) {
				if saved.Load() {
					return configured(), nil
				}
				return relayconfig.Config{}, nil
			}
			entered, release := make(chan struct{}), make(chan struct{})
			a.deps.setup = func(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
				close(entered)
				<-release
				saved.Store(true)
				return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
			}
			start := a.deps.start
			a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
				if !saved.Load() {
					return nil, engine.ErrNetworkUnavailable
				}
				if candidates := request.RelayIntent.Candidates(); len(candidates) != 1 || candidates[0].PublicURL != "https://chosen.example" {
					t.Error("explicit relay lost")
				}
				return start(ctx, request)
			}
			a.execute(createRequest())
			awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
			if reply := a.execute(setupRequest()); reply.Error != nil {
				t.Fatal(reply.Error)
			}
			<-entered
			if reply := a.execute(setupRequest()); reply.Error != nil {
				t.Fatal("setup replay was rejected")
			}
			if cancelled {
				a.execute(Request{Command: "cancel", OperationID: "operation"})
			}
			close(release)
			a.work.Wait()
			if !saved.Load() {
				t.Fatal("share cancellation discarded saved settings")
			}
			view := a.snapshot().Shares[0]
			if cancelled {
				if starts.Load() != 0 || view.Result.Outcome != "cancelled" {
					t.Fatal("late setup resurrected share")
				}
			} else if starts.Load() != 1 || view.OperationID != "operation" || view.TaskID == "" {
				t.Fatal("setup replaced operation identity")
			}
		})
	}
}

func TestCancellationDuringAdmissionRemainsQueryableAndCancelsLateTask(t *testing.T) {
	a, task, starts := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	start := a.deps.start
	a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
		close(entered)
		<-release
		return start(ctx, request)
	}
	a.execute(createRequest())
	<-entered
	reply := a.execute(Request{Command: "cancel", OperationID: "operation"})
	if reply.Snapshot.Shares[0].State != Stopping {
		t.Fatal("admission blocks cancellation or hides joined ownership")
	}
	if a.execute(Request{Command: "list"}).Error != nil {
		t.Fatal("admission blocks queries")
	}
	close(release)
	a.work.Wait()
	if starts.Load() != 1 || task.Snapshot().State != engine.TaskStopping {
		t.Fatal("late task escaped cancellation")
	}
}

func TestShutdownCancelsAndJoinsSetupWithoutLateAdmission(t *testing.T) {
	a, _, starts := fixture(t)
	a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, nil }
	a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
		return nil, engine.ErrNetworkUnavailable
	}
	entered, joined := make(chan struct{}), make(chan struct{})
	a.deps.setup = func(ctx context.Context, _ SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		close(entered)
		<-ctx.Done()
		close(joined)
		return SetupResult{}, nil // A provider can finish successfully concurrently with cancellation.
	}
	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	a.execute(setupRequest())
	<-entered
	if err := a.close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("shutdown returned before setup joined")
	}
	if starts.Load() != 0 {
		t.Fatal("shutdown admitted a late share")
	}
}

func TestSnapshotQueueCoalescesAndReadsDoNotAdvanceRevision(t *testing.T) {
	a, _, _ := fixture(t)
	queue, unsubscribe := a.subscribe()
	defer unsubscribe()
	a.execute(Request{Command: "cancel", OperationID: "first"})
	a.execute(Request{Command: "cancel", OperationID: "second"})
	latest := a.snapshot()
	if len(queue.snapshots) != 1 {
		t.Fatal("snapshots accumulated")
	}
	queued := <-queue.snapshots
	if queued.Revision != latest.Revision || len(queued.Shares) != 2 {
		t.Fatal("coalescing lost state")
	}
	for range 10 {
		if a.snapshot().Revision != latest.Revision {
			t.Fatal("read advanced revision")
		}
	}
	if len(queue.snapshots) != 0 {
		t.Fatal("unchanged state emitted notifications")
	}
}
