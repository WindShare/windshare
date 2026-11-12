package desktop

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/turn"
)

type fakeShare struct {
	mu     sync.Mutex
	state  engine.ShareSnapshot
	events chan engine.Observation
	reason engine.StopReason
}

func (f *fakeShare) ID() engine.TaskID                       { return "engine:1" }
func (f *fakeShare) Snapshot() engine.ShareSnapshot          { f.mu.Lock(); defer f.mu.Unlock(); return f.state }
func (f *fakeShare) Observations() <-chan engine.Observation { return f.events }
func (f *fakeShare) Cancel()                                 { f.stop(engine.Cancelled) }
func (f *fakeShare) StopShare()                              { f.stop(engine.ShareStopped) }
func (f *fakeShare) stop(reason engine.StopReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reason == engine.NoStop {
		f.reason = reason
		f.state.State = engine.TaskStopping
	}
}
func (f *fakeShare) ready() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Startup = engine.ShareReadyReached
	f.state.Ready.Capability = link.Link{}
}
func (f *fakeShare) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reason == engine.NoStop {
		f.reason = engine.ApplicationClosed
	}
	f.state.State = engine.TaskFinished
	outcome := engine.OutcomeCancelled
	if f.reason == engine.ShareStopped {
		outcome = engine.OutcomeStopped
	}
	f.state.Result = &engine.TaskResult[engine.ShareResult]{StopReason: f.reason,
		Completion: engine.TaskCompletion[engine.ShareResult]{Settlement: engine.TaskSettlement{Outcome: outcome}}}
	close(f.events)
}

func fixture(t *testing.T) (*Application, *fakeShare, *int) {
	t.Helper()
	task := &fakeShare{state: engine.ShareSnapshot{State: engine.TaskRunning}, events: make(chan engine.Observation, 1)}
	starts := 0
	a := newApplication("session", dependencies{
		start: func(engine.ShareRequest) (shareTask, error) { starts++; return task, nil },
		close: func(context.Context) error {
			if starts > 0 {
				task.finish()
			}
			return nil
		},
		configure: func(turn.Settings) error { return nil },
		load: func() (relayconfig.Config, error) {
			return relayconfig.Config{FrontURL: "https://front.example", ActiveRelays: []string{"https://relay.example"},
				Profiles: []relayprofile.Candidate{{PublicURL: "https://relay.example"}}}, nil
		},
		log: func(string, any) {},
	})
	t.Cleanup(func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	})
	return a, task, &starts
}

func createRequest() Request {
	return Request{ID: "request", Command: "create", OperationID: "operation", Paths: []string{"chosen.txt"}}
}

func TestCreationIsRetainedBeforeReadinessAndIdempotent(t *testing.T) {
	a, _, starts := fixture(t)
	reply := a.execute(createRequest())
	if reply.Error != nil || len(reply.Snapshot.Shares) != 1 || reply.Snapshot.Shares[0].TaskID != "engine:1" ||
		reply.Snapshot.Shares[0].Startup != "pending" {
		t.Fatalf("creation: %+v", reply)
	}
	retry := a.execute(createRequest())
	if retry.Error != nil || *starts != 1 {
		t.Fatalf("duplicate created %d tasks", *starts)
	}
	conflict := createRequest()
	conflict.Paths = []string{"different.txt"}
	if a.execute(conflict).Error == nil {
		t.Fatal("operation identity accepted different intent")
	}
	if retry.Snapshot.Revision <= reply.Snapshot.Revision {
		t.Fatal("snapshot revision regressed")
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
			if ready {
				task.ready()
			}
			a.execute(Request{Command: "cancel", OperationID: "operation"})
			a.execute(createRequest())
			if task.reason != engine.Cancelled || *starts != 1 {
				t.Fatal("cancellation lost to readiness or retry")
			}
		})
	}
}

func TestCancellationBeforeCreationCannotResurrectShare(t *testing.T) {
	a, _, starts := fixture(t)
	a.execute(Request{Command: "cancel", OperationID: "operation"})
	reply := a.execute(createRequest())
	if *starts != 0 || reply.Snapshot.Shares[0].Result.Outcome != "cancelled" {
		t.Fatal("late creation resurrected cancelled intent")
	}
}

func TestStopIsDistinctFromCancellationAndTerminalResultIsRetained(t *testing.T) {
	a, task, _ := fixture(t)
	a.execute(createRequest())
	reply := a.execute(Request{Command: "stop", TaskID: "engine:1"})
	if task.reason != engine.ShareStopped || reply.Snapshot.Shares[0].State != "stopping" {
		t.Fatal("stop semantics")
	}
	// Resource ownership completion, not an observed event, controls finality.
	task.mu.Lock()
	task.state.Result = &engine.TaskResult[engine.ShareResult]{StopReason: engine.ShareStopped,
		Completion: engine.TaskCompletion[engine.ShareResult]{Settlement: engine.TaskSettlement{Outcome: engine.OutcomeStopped}}}
	task.mu.Unlock()
	view := a.snapshot().Shares[0]
	if view.Result.Outcome != "stopped" || view.Result.StopReason != "share_stopped" {
		t.Fatalf("%+v", view)
	}
}

func TestConfigurationAndCreationFailuresAreActionableAndRetained(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Application)
	}{
		{"missing", func(a *Application) {
			a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, nil }
		}},
		{"unreadable", func(a *Application) {
			a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, errors.New("read failed") }
		}},
		{"turn", func(a *Application) {
			a.deps.configure = func(turn.Settings) error { return errors.New("invalid TURN") }
		}},
		{"start", func(a *Application) {
			a.deps.start = func(engine.ShareRequest) (shareTask, error) { return nil, engine.ErrClosed }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _, starts := fixture(t)
			tc.change(a)
			reply := a.execute(createRequest())
			view := reply.Snapshot.Shares[0]
			if view.Error == nil || view.Error.Message == "" || view.State != "finished" || view.URL != "" || *starts != 0 {
				t.Fatalf("%+v", view)
			}
			if a.execute(createRequest()).Snapshot.Shares[0].Error == nil {
				t.Fatal("failure was not retained")
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
