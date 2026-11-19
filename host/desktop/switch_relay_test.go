package desktop

import (
	"context"
	"slices"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestSwitchRelayReusesSelectionAfterJoiningUnpublishedAttempt(t *testing.T) {
	a, original, starts := fixture(t)
	original.state.Startup = engine.ShareAwaitingAuthorization
	original.state.Authorization = &engine.ShareAuthorization{Kind: engine.ShareOfficialAuthorization}
	create := createRequest()
	a.execute(create)
	waiting := awaitShare(t, a, create.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })
	if waiting.TaskID != string(original.ID()) || waiting.URL != "" || !a.snapshot().active() {
		t.Fatalf("authorization wait lost the underlying task: %+v", waiting)
	}

	relay := configured()
	address := relay.ActiveRelays[0]
	relay.ActiveRelays = nil
	a.deps.load = func() (relayconfig.Config, error) { return relay, nil }
	a.deps.setup = func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{RelayURL: address, RelaySaved: true}, nil
	}
	setup := setupRequest()
	setup.OperationID = create.OperationID
	if reply := a.execute(setup); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if got := a.snapshot().Shares[0]; got.TaskID != waiting.TaskID || got.State != AwaitingAuthorization {
		t.Fatalf("saving settings replaced an attempt without a switch: %+v", got)
	}

	replacement := &fakeShare{
		id: "engine:2", state: engine.ShareSnapshot{State: engine.TaskRunning},
		events: make(chan engine.Observation), changed: make(chan struct{}), done: make(chan struct{}),
	}
	closeOriginal := a.deps.close
	a.deps.close = func(ctx context.Context) error { replacement.finish(); return closeOriginal(ctx) }
	requests := make(chan engine.ShareRequest, 1)
	a.deps.start = func(_ context.Context, request engine.ShareRequest) (shareTask, error) {
		starts.Add(1)
		requests <- request
		return replacement, nil
	}
	a.mu.Lock()
	paths := slices.Clone(a.operations[create.OperationID].paths)
	a.mu.Unlock()

	switchRelay := Request{Command: "switch_relay", OperationID: create.OperationID, RelayURL: address}
	if reply := a.execute(switchRelay); reply.Error != nil || reply.Snapshot.Shares[0].State != Admitting {
		t.Fatalf("switch did not retain the operation through cleanup: %+v", reply)
	}
	if reply := a.execute(switchRelay); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	if starts.Load() != 1 || original.Snapshot().State != engine.TaskStopping {
		t.Fatal("replacement started before the cancelled attempt joined")
	}
	original.finish()
	ready := awaitShare(t, a, create.OperationID, func(s Share) bool { return s.TaskID == string(replacement.ID()) })
	request := <-requests
	candidates := request.RelayIntent.Candidates()
	if request.Source == nil || request.RelayIntent.Mode() != relayprofile.Explicit || len(candidates) != 1 || candidates[0].PublicURL != address {
		t.Fatalf("replacement did not use the saved relay explicitly: %+v", request.RelayIntent)
	}
	if reply := a.execute(create); reply.Error != nil {
		t.Fatal("replaying create after a switch changed the request identity", reply.Error)
	}
	if reply := a.execute(switchRelay); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if starts.Load() != 2 || ready.OperationID != create.OperationID ||
		!slices.Equal(a.operations[create.OperationID].paths, paths) || a.preferences.Relays[0].Enabled {
		t.Fatal("switch or replay changed the selection, global settings, or attempt count")
	}
}

func TestSwitchRelayLeavesPublishedTaskRunning(t *testing.T) {
	a, task, starts := fixture(t)
	create := createRequest()
	a.execute(create)
	admitted(t, a)
	task.ready()
	a.execute(Request{Command: "preferences"})
	reply := a.execute(Request{Command: "switch_relay", OperationID: create.OperationID, RelayURL: configured().ActiveRelays[0]})
	if reply.Error != nil || reply.Snapshot.Shares[0].State != Sharing || starts.Load() != 1 || task.Snapshot().State != engine.TaskRunning {
		t.Fatalf("stale switch changed a published share: %+v", reply)
	}
}
