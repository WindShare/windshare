package desktop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

type fakeShare struct {
	mu       sync.Mutex
	state    engine.ShareSnapshot
	events   chan engine.Observation
	changed  chan struct{}
	reason   engine.StopReason
	finished bool
}

func (f *fakeShare) ID() engine.TaskID                       { return "engine:1" }
func (f *fakeShare) Snapshot() engine.ShareSnapshot          { f.mu.Lock(); defer f.mu.Unlock(); return f.state }
func (f *fakeShare) Changes() <-chan struct{}                { f.mu.Lock(); defer f.mu.Unlock(); return f.changed }
func (f *fakeShare) Observations() <-chan engine.Observation { return f.events }
func (f *fakeShare) Cancel()                                 { f.stop(engine.Cancelled) }
func (f *fakeShare) StopShare()                              { f.stop(engine.ShareStopped) }
func (f *fakeShare) signal()                                 { close(f.changed); f.changed = make(chan struct{}) }
func (f *fakeShare) stop(reason engine.StopReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reason == engine.NoStop && !f.finished {
		f.reason, f.state.State = reason, engine.TaskStopping
		f.signal()
	}
}
func (f *fakeShare) ready() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Startup = engine.ShareReadyReached
	f.state.Ready.Capability = link.Link{}
	f.signal()
}
func (f *fakeShare) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finished {
		return
	}
	f.finished = true
	if f.reason == engine.NoStop {
		f.reason = engine.ApplicationClosed
	}
	f.state.State = engine.TaskFinished
	outcome := engine.OutcomeCancelled
	if f.reason == engine.ShareStopped {
		outcome = engine.OutcomeStopped
	}
	f.state.Result = &engine.TaskResult[engine.ShareResult]{StopReason: f.reason, Outcome: outcome}
	close(f.events)
	f.signal()
}

func configured() relayconfig.Config {
	return relayconfig.Config{FrontURL: "https://front.example", ActiveRelays: []string{"https://relay.example"},
		Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}}}
}
func fixture(t *testing.T) (*Application, *fakeShare, *atomic.Int32) {
	t.Helper()
	task := &fakeShare{state: engine.ShareSnapshot{State: engine.TaskRunning}, events: make(chan engine.Observation, 1), changed: make(chan struct{})}
	starts := &atomic.Int32{}
	a := newApplication("session", dependencies{
		start:       func(context.Context, engine.ShareRequest) (shareTask, error) { starts.Add(1); return task, nil },
		close:       func(context.Context) error { task.finish(); return nil },
		configure:   func(turn.Settings) error { return nil },
		load:        func() (relayconfig.Config, error) { return configured(), nil },
		log:         func(string, any) {},
		cancelSetup: func(context.Context, string) error { return nil },
	})
	t.Cleanup(func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	})
	return a, task, starts
}
func createRequest() Request {
	return Request{ID: "request", Command: "create", OperationID: "operation", Paths: []string{"chosen.txt"}}
}
func awaitShare(t *testing.T, a *Application, id string, predicate func(Share) bool) Share {
	t.Helper()
	queue, unsubscribe := a.subscribe()
	defer unsubscribe()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		for _, share := range a.snapshot().Shares {
			if share.OperationID == id && predicate(share) {
				return share
			}
		}
		select {
		case <-queue.snapshots:
		case <-deadline.C:
			t.Fatal("share transition not published")
		}
	}
}
func admitted(t *testing.T, a *Application) Share {
	t.Helper()
	return awaitShare(t, a, "operation", func(s Share) bool { return s.TaskID != "" })
}
func TestCreationIsRetainedBeforeReadinessAndIdempotent(t *testing.T) {
	a, _, starts := fixture(t)
	reply := a.execute(createRequest())
	if reply.Error != nil || reply.Snapshot.Shares[0].State != Admitting {
		t.Fatalf("creation: %+v", reply)
	}
	admitted(t, a)
	before := a.snapshot()
	retry := a.execute(createRequest())
	if retry.Error != nil || starts.Load() != 1 || retry.Snapshot.Revision != before.Revision {
		t.Fatal("replay mutated intent or revision")
	}
	conflict := createRequest()
	conflict.Paths = []string{"different.txt"}
	if a.execute(conflict).Error == nil {
		t.Fatal("identity accepted different intent")
	}
	if a.execute(Request{Command: "query", OperationID: "operation"}).Error != nil {
		t.Fatal("lost reply is not recoverable")
	}
}
func TestCancellationCoversBothSidesOfReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "preparing", true: "ready"}[ready], func(t *testing.T) {
			a, task, starts := fixture(t)
			a.execute(createRequest())
			admitted(t, a)
			if ready {
				task.ready()
			}
			a.execute(Request{Command: "cancel", OperationID: "operation"})
			a.execute(createRequest())
			if task.Snapshot().State != engine.TaskStopping || starts.Load() != 1 {
				t.Fatal("cancellation lost to readiness or replay")
			}
		})
	}
}
func TestCancellationBeforeCreationCannotResurrectShare(t *testing.T) {
	a, _, starts := fixture(t)
	a.execute(Request{Command: "cancel", OperationID: "operation"})
	reply := a.execute(createRequest())
	if starts.Load() != 0 || reply.Snapshot.Shares[0].Result.Outcome != "cancelled" {
		t.Fatal("late creation resurrected intent")
	}
}
func TestJoinedCompletionAndReadinessNotifyWithoutObservations(t *testing.T) {
	a, task, _ := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	queue, unsubscribe := a.subscribe()
	defer unsubscribe()
	task.ready()
	select {
	case snapshot := <-queue.snapshots:
		if snapshot.Shares[0].Startup != "ready" {
			t.Fatal("notification preceded commit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readiness required polling")
	}
	reply := a.execute(Request{Command: "stop", TaskID: "engine:1"})
	if reply.Snapshot.Shares[0].State != Stopping || reply.Snapshot.Shares[0].URL != "" {
		t.Fatal("stopping exposed a link")
	}
	task.finish()
	view := awaitShare(t, a, "operation", func(s Share) bool { return s.Result != nil })
	if view.Result.Outcome != "stopped" || view.Result.StopReason != "share_stopped" {
		t.Fatalf("%+v", view)
	}
}
func TestConfigurationWaitsAndTerminalFailureIsRetained(t *testing.T) {
	for _, configuration := range []bool{true, false} {
		t.Run(map[bool]string{true: "configuration", false: "admission"}[configuration], func(t *testing.T) {
			a, _, starts := fixture(t)
			if configuration {
				a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, nil }
				a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
					return nil, engine.ErrNetworkUnavailable
				}
			} else {
				a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
					return nil, errors.New("admission failed")
				}
			}
			a.execute(createRequest())
			view := awaitShare(t, a, "operation", func(s Share) bool { return s.Error != nil })
			expected := Finished
			if configuration {
				expected = AwaitingNetwork
			}
			if view.State != expected || starts.Load() != 0 {
				t.Fatalf("%+v", view)
			}
			a.execute(createRequest())
			if a.snapshot().Shares[0].State != expected {
				t.Fatal("replay retried failure")
			}
		})
	}
}
func TestClosingRejectsCreationAndBadCommands(t *testing.T) {
	a, _, _ := fixture(t)
	a.closing = true
	for _, req := range []Request{createRequest(), {Command: "unknown"}, {Command: "query", OperationID: "missing"}, {Command: "stop", TaskID: "missing"}, {Command: "cancel"}} {
		if a.execute(req).Error == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}
