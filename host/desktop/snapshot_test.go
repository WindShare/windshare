package desktop

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relayprofile"
	"github.com/windshare/windshare/engine/relaysetup"
)

// Operation IDs are opaque; "share:10" sorts between "share:1" and "share:2".
// Insertion must neither re-log unchanged shares nor hide a real transition.
func TestShareStateLogsOnlyChangedSharesWhenInsertionShiftsOrder(t *testing.T) {
	for _, test := range []struct {
		name           string
		cancelExisting bool
		want           []string
	}{
		{name: "insertion only", want: []string{"share:10"}},
		{name: "insertion and existing transition", cancelExisting: true, want: []string{"share:10", "share:3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logged []string
			a := newApplication("session", dependencies{
				log: func(event string, value any) {
					if event == "host.share_state" {
						logged = append(logged, value.(map[string]any)["operation_id"].(string))
					}
				},
			})
			for n := 1; n <= 9; n++ {
				id := fmt.Sprintf("share:%d", n)
				a.operations[id] = &operation{id: id, phase: admission{}}
			}
			a.snapshot()
			logged = nil

			// Both changes must reach the same snapshot to exercise identity
			// matching while an existing operation moves in the sorted list.
			if test.cancelExisting {
				a.operations["share:3"].phase = admission{cancelling: true}
			}
			reply := a.execute(Request{Command: "cancel", OperationID: "share:10"})
			if reply.Error != nil {
				t.Fatalf("insert cancelled operation: %+v", reply.Error)
			}
			if !slices.Equal(logged, test.want) {
				t.Fatalf("host.share_state logged %v; want %v", logged, test.want)
			}
		})
	}
}

func TestProjectTaskPublishesAndClearsBackupURL(t *testing.T) {
	publicKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	capability, err := link.NewSenderAuthenticated(make([]byte, link.ReadSecretBytes), publicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	capability.NodeAliases = []string{"cn1", "us1"}

	task := &fakeShare{
		id: "engine:1",
		state: engine.ShareSnapshot{
			State:   engine.TaskRunning,
			Startup: engine.ShareReadyReached,
			Ready: engine.ShareReady{
				Capability:        capability,
				Frontend:          "https://ws.example",
				AlternateFrontend: "https://backup.example",
			},
		},
	}
	var view Share
	projectTask(&view, running{task: task})
	wantPrimary, err := capability.URL("https://ws.example")
	if err != nil {
		t.Fatal(err)
	}
	wantBackup, err := capability.URL("https://backup.example")
	if err != nil {
		t.Fatal(err)
	}
	if view.URL != wantPrimary || view.BackupURL != wantBackup {
		t.Fatalf("projected URLs = (%q, %q), want (%q, %q)", view.URL, view.BackupURL, wantPrimary, wantBackup)
	}

	task.state.Ready.AlternateFrontend = ""
	var explicit Share
	projectTask(&explicit, running{task: task})
	if explicit.URL != wantPrimary || explicit.BackupURL != "" {
		t.Fatalf("explicit share URLs = (%q, %q), want (%q, %q)", explicit.URL, explicit.BackupURL, wantPrimary, "")
	}

	task.state.Ready.AlternateFrontend = "https://backup.example"
	task.state.State = engine.TaskStopping
	var stopping Share
	projectTask(&stopping, running{task: task})
	if stopping.URL != "" || stopping.BackupURL != "" {
		t.Fatalf("stopping share retained links: (%q, %q)", stopping.URL, stopping.BackupURL)
	}

	task.state.State = engine.TaskFinished
	task.state.Result = &engine.TaskResult[engine.ShareResult]{Outcome: engine.OutcomeStopped, StopReason: engine.ShareStopped}
	var finished Share
	projectTask(&finished, running{task: task})
	if finished.URL != "" || finished.BackupURL != "" {
		t.Fatalf("finished share retained links: (%q, %q)", finished.URL, finished.BackupURL)
	}
}

func TestProjectTaskProjectsStructuredFailuresCollisionsRuntimeErrorsAndOmissions(t *testing.T) {
	// 1. Startup cloud placeholder error preserves Failure.Code and exposes EngineCode + SourcePath.
	cloudErr := &catalog.SelectionError{
		Label: `C:\Users\alice\OneDrive\deck.pptx`,
		Cause: &catalog.SourceError{Failure: catalog.SourceFailureCloudPlaceholder},
	}
	var startupView Share
	projectTask(&startupView, running{task: &fakeShare{
		id: "engine:cloud",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: cloudErr,
			Result:       &engine.TaskResult[engine.ShareResult]{Outcome: engine.OutcomeFailed, Err: cloudErr},
		},
	}})
	if startupView.Error == nil || startupView.Error.Code != "startup_failed" ||
		startupView.Error.EngineCode != string(engine.FailureSourceCloudPlaceholder) ||
		startupView.Error.SourcePath != `C:\Users\alice\OneDrive\deck.pptx` {
		t.Fatalf("cloud startup error = %+v", startupView.Error)
	}

	// 2. Selection collision carries FirstPath / SecondPath without overloading Failure.Code.
	collisionErr := &catalog.SelectionNameCollisionError{
		FirstLabel:  `C:\work\Report.pdf`,
		SecondLabel: `D:\other\report.pdf`,
	}
	var collisionView Share
	projectTask(&collisionView, running{task: &fakeShare{
		id: "engine:collision",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: collisionErr,
			Result:       &engine.TaskResult[engine.ShareResult]{Outcome: engine.OutcomeFailed, Err: collisionErr},
		},
	}})
	if collisionView.Error == nil || collisionView.Error.Code != "startup_failed" ||
		collisionView.Error.EngineCode != string(engine.FailureSelectionCollision) ||
		collisionView.Error.SelectionCollision == nil ||
		collisionView.Error.SelectionCollision.FirstPath != `C:\work\Report.pdf` ||
		collisionView.Error.SelectionCollision.SecondPath != `D:\other\report.pdf` {
		t.Fatalf("collision startup error = %+v", collisionView.Error)
	}

	// 3. Runtime failure after ShareReadyReached populates structured view.Error and preserves Omissions.
	publicKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	capability, err := link.NewSenderAuthenticated(make([]byte, link.ReadSecretBytes), publicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeErr := &catalog.SourceError{Failure: catalog.SourceFailureStale}
	var runtimeView Share
	projectTask(&runtimeView, running{task: &fakeShare{
		id: "engine:runtime",
		state: engine.ShareSnapshot{
			State:   engine.TaskFinished,
			Startup: engine.ShareReadyReached,
			Ready:   engine.ShareReady{Capability: capability, Frontend: "https://ws.example"},
			SourceOmissions: &engine.ShareSourceOmissions{
				Count: 2,
				Samples: []catalog.SourceOmission{
					{Name: "cloud-only.docx", Reason: catalog.SourceOmissionCloudPlaceholder},
				},
			},
			Result: &engine.TaskResult[engine.ShareResult]{Outcome: engine.OutcomeFailed, Err: runtimeErr},
		},
	}})
	if runtimeView.Error == nil || runtimeView.Error.Code != "share_failed" ||
		runtimeView.Error.EngineCode != string(engine.FailureSourceChanged) {
		t.Fatalf("runtime failure = %+v", runtimeView.Error)
	}
	if runtimeView.Omissions == nil || runtimeView.Omissions.Count != 2 ||
		len(runtimeView.Omissions.Samples) != 1 || runtimeView.Omissions.Samples[0].Name != "cloud-only.docx" ||
		runtimeView.Omissions.Samples[0].Reason != string(catalog.SourceOmissionCloudPlaceholder) {
		t.Fatalf("omissions projection = %+v", runtimeView.Omissions)
	}
}

type failingAccount struct {
	*controlledAccount
	err error
}

func (a failingAccount) Register(context.Context) error { return a.err }

func TestHostFailuresOmitEngineClassificationWhileEngineBoundariesPreserveIt(t *testing.T) {
	pathErr := &fs.PathError{Op: "open", Path: "config.json", Err: fs.ErrPermission}
	netErr := &url.Error{Op: "Post", URL: "https://authority.example/login", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}

	// Host/control-layer failures never inherit engine.DescribeFailure stdlib fallbacks.
	a, _, _ := fixture(t)
	a.deps.load = func() (relayconfig.Config, error) { return relayconfig.Config{}, pathErr }
	if reply := a.execute(Request{ID: "prefs", Command: "preferences"}); reply.Error == nil ||
		reply.Error.Code != "settings_unavailable" || reply.Error.EngineCode != "" {
		t.Fatalf("preferences error = %+v, want settings_unavailable without EngineCode", reply.Error)
	}
	if reply := a.execute(Request{ID: "bad", Command: "unknown"}); reply.Error == nil ||
		reply.Error.Code != "request_rejected" || reply.Error.EngineCode != "" {
		t.Fatalf("request_rejected error = %+v, want empty EngineCode", reply.Error)
	}
	a.execute(createRequest())
	blocked := awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingConfiguration })
	if blocked.Error == nil || blocked.Error.Code != "settings_unavailable" || blocked.Error.EngineCode != "" {
		t.Fatalf("configurationBlocked error = %+v, want settings_unavailable without EngineCode", blocked.Error)
	}

	// Setup and setup-cancellation failures stay pure host failures.
	setupApp := newApplication("setup-test", dependencies{
		load:  func() (relayconfig.Config, error) { return configured(), nil },
		log:   func(string, any) {},
		close: func(context.Context) error { return nil },
		setup: func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error) {
			return SetupResult{RelayURL: "https://relay.example"}, netErr
		},
		cancelSetup: func(context.Context, string) error { return netErr },
	})
	t.Cleanup(func() { _ = setupApp.close() })
	setupReq := setupRequest()
	setupReq.OperationID = ""
	if reply := setupApp.execute(setupReq); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	setupApp.work.Wait()
	if setupErr := setupApp.snapshot().Setup.Error; setupErr == nil ||
		setupErr.Code != "setup_failed" || setupErr.EngineCode != "" {
		t.Fatalf("setup_failed error = %+v, want empty EngineCode", setupErr)
	}
	if reply := setupApp.execute(Request{ID: "cancel-setup", Command: "cancel_setup", SetupID: setupReq.Setup.ID}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	setupApp.work.Wait()
	if cancelErr := setupApp.snapshot().Setup.Error; cancelErr == nil ||
		cancelErr.Code != "setup_cancel_failed" || cancelErr.EngineCode != "" {
		t.Fatalf("setup_cancel_failed error = %+v, want empty EngineCode", cancelErr)
	}

	// Account command failures stay pure host/account failures even when wrapping net/url errors.
	accountApp := newApplication("account-test", dependencies{
		accounts: failingAccount{controlledAccount: &controlledAccount{changed: make(chan struct{})}, err: netErr},
		log:      func(string, any) {},
		close:    func(context.Context) error { return nil },
	})
	t.Cleanup(func() { _ = accountApp.close() })
	if reply := accountApp.execute(Request{
		ID: "acct", Command: "account", OperationID: "acct-op",
		Account: &AccountInput{Action: "register"},
	}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	accountApp.work.Wait()
	if acctView := accountApp.snapshot().Account; acctView == nil || acctView.Operation == nil ||
		acctView.Operation.State != "failed" || acctView.Operation.Error == nil ||
		acctView.Operation.Error.EngineCode != "" {
		t.Fatalf("account command error = %+v, want failed without EngineCode", acctView)
	}

	// Engine boundaries preserve structured EngineCode.
	netApp, _, _ := fixture(t)
	netApp.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
		return nil, &engine.NetworkUnavailableError{Reason: engine.NetworkDirectoryUnavailable, Cause: netErr}
	}
	netApp.execute(createRequest())
	netView := awaitShare(t, netApp, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	if netView.Error == nil || netView.Error.Code != "network_unavailable" ||
		netView.Error.EngineCode != string(engine.FailureNetworkUnavailable) {
		t.Fatalf("network_unavailable error = %+v", netView.Error)
	}

	createApp, _, _ := fixture(t)
	createApp.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
		return nil, &catalog.SourceError{Failure: catalog.SourceFailureAccessDenied}
	}
	createApp.execute(createRequest())
	createView := awaitShare(t, createApp, "operation", func(s Share) bool { return s.State == Finished })
	if createView.Error == nil || createView.Error.Code != "creation_failed" ||
		createView.Error.EngineCode != string(engine.FailureSourceAccessDenied) {
		t.Fatalf("creation_failed error = %+v", createView.Error)
	}

	retainApp, _, _ := fixture(t)
	selectedTask := &fakeShare{
		id: "engine:selected",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: netErr,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:      engine.OutcomeFailed,
				FailureClass: engine.FailureNetwork,
				Err:          netErr,
			},
		},
	}
	retainApp.operations["selected-op"] = &operation{
		id:    "selected-op",
		phase: running{task: selectedTask, selectedRelays: true},
	}
	retainSnap := retainApp.snapshot()
	if len(retainSnap.Shares) != 1 || retainSnap.Shares[0].State != AwaitingNetwork ||
		retainSnap.Shares[0].Error == nil ||
		retainSnap.Shares[0].Error.Code != "selected_relays_unavailable" ||
		retainSnap.Shares[0].Error.EngineCode != string(engine.FailureRelayUnavailable) {
		t.Fatalf("selected_relays_unavailable view = %+v", retainSnap.Shares)
	}
}

func TestProjectTaskFollowsSettlementOutcomeAndCancellationContract(t *testing.T) {
	cleanupErr := errors.New("relay cleanup failed")
	cloudErr := &catalog.SelectionError{
		Label: `C:\Users\alice\OneDrive\deck.pptx`,
		Cause: &catalog.SourceError{Failure: catalog.SourceFailureCloudPlaceholder},
	}

	// 1. Pre-ready cancellation with clean teardown clears startup cancellation error.
	var cancelledPreReady Share
	projectTask(&cancelledPreReady, running{task: &fakeShare{
		id: "engine:cancel-startup",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: context.Canceled,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:    engine.OutcomeCancelled,
				StopReason: engine.Cancelled,
			},
		},
	}})
	if cancelledPreReady.State != Finished || cancelledPreReady.Result == nil ||
		cancelledPreReady.Result.Outcome != OutcomeCancelled ||
		cancelledPreReady.Result.Error != "" || cancelledPreReady.Result.CleanupError != "" ||
		cancelledPreReady.Error != nil {
		t.Fatalf("pre-ready cancelled view = %+v (result=%+v, error=%+v)", cancelledPreReady, cancelledPreReady.Result, cancelledPreReady.Error)
	}

	// 2. OutcomeCancelled retaining a non-nil cause never emits share_failed or Result.Error.
	var cancelledWithCause Share
	projectTask(&cancelledWithCause, running{task: &fakeShare{
		id: "engine:cancel-cause",
		state: engine.ShareSnapshot{
			State:   engine.TaskFinished,
			Startup: engine.SharePreparing,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:    engine.OutcomeCancelled,
				StopReason: engine.Cancelled,
				Err:        context.Canceled,
			},
		},
	}})
	if cancelledWithCause.State != Finished || cancelledWithCause.Result == nil ||
		cancelledWithCause.Result.Outcome != OutcomeCancelled ||
		cancelledWithCause.Result.Error != "" || cancelledWithCause.Error != nil {
		t.Fatalf("cancelled with retained cause = %+v (result=%+v, error=%+v)", cancelledWithCause, cancelledWithCause.Result, cancelledWithCause.Error)
	}

	// 3. Non-terminal stopping after startup cancellation does not flash startup_failed.
	var stoppingCancel Share
	projectTask(&stoppingCancel, running{task: &fakeShare{
		id: "engine:stopping-cancel",
		state: engine.ShareSnapshot{
			State:        engine.TaskStopping,
			Startup:      engine.ShareStartupFailed,
			StartupError: context.Canceled,
		},
	}})
	if stoppingCancel.State != Stopping || stoppingCancel.Error != nil {
		t.Fatalf("stopping after startup cancel = %+v (error=%+v)", stoppingCancel, stoppingCancel.Error)
	}

	// 4. Startup cancellation followed by teardown failure surfaces share_failed rather than canceled.
	var cancelThenTeardownFailed Share
	projectTask(&cancelThenTeardownFailed, running{task: &fakeShare{
		id: "engine:cancel-teardown-fail",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: context.Canceled,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:      engine.OutcomeFailed,
				StopReason:   engine.Cancelled,
				Err:          cleanupErr,
				CleanupError: cleanupErr,
			},
		},
	}})
	if cancelThenTeardownFailed.Error == nil ||
		cancelThenTeardownFailed.Error.Code != "share_failed" ||
		cancelThenTeardownFailed.Error.Message != cleanupErr.Error() ||
		cancelThenTeardownFailed.Error.EngineCode == string(engine.FailureCanceled) ||
		cancelThenTeardownFailed.Result == nil ||
		cancelThenTeardownFailed.Result.Error != cleanupErr.Error() ||
		cancelThenTeardownFailed.Result.CleanupError != cleanupErr.Error() {
		t.Fatalf("cancel then teardown failure = %+v (result=%+v, error=%+v)", cancelThenTeardownFailed, cancelThenTeardownFailed.Result, cancelThenTeardownFailed.Error)
	}

	// 5. Startup cancellation followed by cleanup-only failure still surfaces share_failed.
	var cancelThenCleanupOnly Share
	projectTask(&cancelThenCleanupOnly, running{task: &fakeShare{
		id: "engine:cancel-cleanup-only",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: context.Canceled,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:      engine.OutcomeFailed,
				StopReason:   engine.Cancelled,
				CleanupError: cleanupErr,
			},
		},
	}})
	if cancelThenCleanupOnly.Error == nil ||
		cancelThenCleanupOnly.Error.Code != "share_failed" ||
		cancelThenCleanupOnly.Error.Message != cleanupErr.Error() {
		t.Fatalf("cancel then cleanup-only failure = %+v (error=%+v)", cancelThenCleanupOnly, cancelThenCleanupOnly.Error)
	}

	// 6. Real startup failure plus cleanup error preserves structured startup_failed and CleanupError.
	var startupAndCleanup Share
	projectTask(&startupAndCleanup, running{task: &fakeShare{
		id: "engine:startup-and-cleanup",
		state: engine.ShareSnapshot{
			State:        engine.TaskFinished,
			Startup:      engine.ShareStartupFailed,
			StartupError: cloudErr,
			Result: &engine.TaskResult[engine.ShareResult]{
				Outcome:      engine.OutcomeFailed,
				Err:          errors.Join(cloudErr, cleanupErr),
				CleanupError: cleanupErr,
			},
		},
	}})
	if startupAndCleanup.Error == nil ||
		startupAndCleanup.Error.Code != "startup_failed" ||
		startupAndCleanup.Error.EngineCode != string(engine.FailureSourceCloudPlaceholder) ||
		startupAndCleanup.Error.SourcePath != `C:\Users\alice\OneDrive\deck.pptx` ||
		startupAndCleanup.Result == nil ||
		startupAndCleanup.Result.CleanupError != cleanupErr.Error() {
		t.Fatalf("startup + cleanup failure = %+v (result=%+v, error=%+v)", startupAndCleanup, startupAndCleanup.Result, startupAndCleanup.Error)
	}
}

func TestUnpublishableReadyShareStopsTaskAndSettlesAsPublicationFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability link.Link
		frontend   string
	}{
		{name: "missing frontend", capability: validTestCapability(), frontend: ""},
		{name: "invalid capability", capability: link.Link{}, frontend: "https://ws.example"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var loggedPublicationFailure bool
			a, task, _ := fixture(t)
			a.deps.log = func(event string, _ any) {
				if event == "host.share_publication_failed" {
					loggedPublicationFailure = true
				}
			}
			a.deps.load = func() (relayconfig.Config, error) {
				return relayconfig.Config{
					ActiveRelays: []string{"https://relay.example"},
					Profiles:     []relayprofile.Candidate{{PublicURL: "https://relay.example"}},
				}, nil
			}
			a.execute(createRequest())
			admitted(t, a)

			task.mu.Lock()
			task.state.Startup = engine.ShareReadyReached
			task.state.Ready = engine.ShareReady{Capability: test.capability, Frontend: test.frontend}
			task.state.RelayAvailability = &engine.RelayAvailability{Available: 1, Total: 1, EverReady: true}
			task.signal()
			task.mu.Unlock()

			stopping := awaitShare(t, a, "operation", func(s Share) bool { return s.State == Stopping })
			if stopping.URL != "" || stopping.BackupURL != "" || stopping.Relay != nil {
				t.Fatalf("stopping unpublishable share leaked ready fields: %+v", stopping)
			}
			if task.Snapshot().State != engine.TaskStopping {
				t.Fatalf("unpublishable ready share did not stop engine task: state=%v", task.Snapshot().State)
			}
			if !loggedPublicationFailure {
				t.Fatal("missing host.share_publication_failed structured log")
			}

			task.finish()
			finished := awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })
			if finished.Result == nil || finished.Result.Outcome != OutcomeFailed ||
				finished.Result.StopReason != ShareStopped || finished.Result.Error == "" ||
				finished.Error == nil || finished.Error.Code != publicationFailedCode ||
				finished.Error.EngineCode != string(engine.FailureCapabilityInvalid) {
				t.Fatalf("settled publication failure = %+v (result=%+v, error=%+v)", finished, finished.Result, finished.Error)
			}
		})
	}

	// Direct projection without snapshotLocked reconciliation still refuses to emit
	// Sharing with an omitted/empty URL.
	var direct Share
	projectTask(&direct, running{task: &fakeShare{
		id: "engine:direct",
		state: engine.ShareSnapshot{
			State:   engine.TaskRunning,
			Startup: engine.ShareReadyReached,
			Ready:   engine.ShareReady{Capability: validTestCapability(), Frontend: ""},
		},
	}})
	if direct.State == Sharing || direct.URL != "" {
		t.Fatalf("direct projection emitted illegal empty-URL sharing state: %+v", direct)
	}
}
