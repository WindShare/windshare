package desktop

import (
	"context"
	"errors"
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

func TestUseSavedSettingsRecoversFailedRelaySwitch(t *testing.T) {
	const relayA = "https://old-relay.example/v2/ws"
	const relayB = "https://healthy-relay.example"
	a, original, _ := fixture(t)
	original.state.Startup = engine.ShareAwaitingAuthorization
	original.state.Authorization = &engine.ShareAuthorization{Kind: engine.ShareOfficialAuthorization}
	settings := relayconfig.Config{Profiles: []relayprofile.Candidate{{PublicURL: relayA}, {PublicURL: relayB}}}
	a.deps.load = func() (relayconfig.Config, error) { return settings, nil }
	a.deps.setup = func(_ context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{}, input.Preference.Apply(&settings)
	}

	failed := &fakeShare{
		id: "engine:2", state: engine.ShareSnapshot{Startup: engine.ShareStartupFailed},
		events: make(chan engine.Observation), changed: make(chan struct{}), done: make(chan struct{}),
	}
	failed.finish()
	failed.state.Result = &engine.TaskResult[engine.ShareResult]{
		Outcome: engine.OutcomeFailed, FailureClass: engine.FailureNetwork, Err: errors.New("relay route unavailable"),
	}
	replacement := &fakeShare{
		id: "engine:3", state: engine.ShareSnapshot{State: engine.TaskRunning},
		events: make(chan engine.Observation), changed: make(chan struct{}), done: make(chan struct{}),
	}
	closeOriginal := a.deps.close
	a.deps.close = func(ctx context.Context) error {
		replacement.finish()
		return closeOriginal(ctx)
	}

	var attempts []engine.ShareRequest
	a.deps.start = func(_ context.Context, request engine.ShareRequest) (shareTask, error) {
		attempts = append(attempts, request)
		switch len(attempts) {
		case 1:
			return original, nil
		case 2:
			return failed, nil
		case 3:
			return nil, engine.ErrNetworkUnavailable
		default:
			return replacement, nil
		}
	}
	execute := func(request Request) {
		t.Helper()
		if reply := a.execute(request); reply.Error != nil {
			t.Fatalf("%s: %+v", request.Command, reply.Error)
		}
	}
	create := createRequest()
	execute(Request{Command: "preferences"})
	execute(create)
	awaitShare(t, a, create.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })
	execute(Request{Command: "switch_relay", OperationID: create.OperationID, RelayURL: relayA})
	original.finish()
	awaitShare(t, a, create.OperationID, func(s Share) bool { return s.State == AwaitingNetwork })
	a.work.Wait()

	execute(Request{
		Command: "configure", OperationID: create.OperationID,
		Setup: &SetupInput{ID: "enable-b", Preference: &relayconfig.PreferenceEdit{Action: "enable", Value: relayB}},
	})
	a.work.Wait()
	if !slices.Equal(settings.ActiveRelays, []string{relayB}) || len(attempts) != 2 {
		t.Fatalf("enabling B should only update settings, enabled=%v attempts=%d", settings.ActiveRelays, len(attempts))
	}

	execute(Request{Command: "resume", OperationID: create.OperationID})
	a.work.Wait()
	if len(attempts) != 3 {
		t.Fatalf("retry did not start a new attempt, attempts=%d", len(attempts))
	}
	retry := attempts[2].RelayIntent
	if candidates := retry.Candidates(); retry.Mode() != relayprofile.Explicit || len(candidates) != 1 || candidates[0].PublicURL != relayA {
		t.Fatalf("retry lost the explicit selection: %+v", retry)
	}

	execute(Request{Command: "use_saved_settings", OperationID: create.OperationID})
	a.work.Wait()
	if len(attempts) != 4 {
		t.Fatalf("saved settings did not start a new attempt, attempts=%d", len(attempts))
	}
	view := a.snapshot().Shares[0]
	resumed := attempts[3]
	if candidates := resumed.RelayIntent.Candidates(); resumed.RelayIntent.Mode() != relayprofile.Planned ||
		len(candidates) != 1 || candidates[0].PublicURL != relayB || resumed.Source == nil ||
		view.OperationID != create.OperationID || view.TaskID != string(replacement.ID()) {
		t.Fatalf("saved settings did not recover the original share on B: %+v, %+v", resumed.RelayIntent, view)
	}
	execute(create)
	if len(a.snapshot().Shares) != 1 {
		t.Fatal("replayed create changed the operation after recovery")
	}
}

func TestRelayRecoveryCommandsLeavePublishedTaskRunning(t *testing.T) {
	a, task, starts := fixture(t)
	create := createRequest()
	create.Relays = configured().ActiveRelays
	a.execute(create)
	admitted(t, a)
	task.ready()
	a.execute(Request{Command: "preferences"})
	for _, request := range []Request{
		{Command: "switch_relay", OperationID: create.OperationID, RelayURL: create.Relays[0]},
		{Command: "use_saved_settings", OperationID: create.OperationID},
	} {
		reply := a.execute(request)
		if reply.Error != nil || reply.Snapshot.Shares[0].State != Sharing || starts.Load() != 1 || task.Snapshot().State != engine.TaskRunning {
			t.Fatalf("%s changed a published share: %+v", request.Command, reply)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !slices.Equal(a.operations[create.OperationID].explicitRelays, create.Relays) {
		t.Fatal("recovery changed the selection of a published share")
	}
}
