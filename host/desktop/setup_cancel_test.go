package desktop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestShareCancellationJoinsSetupAndRetainsRetryableCleanup(t *testing.T) {
	a, _, starts := fixture(t)
	a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
		return nil, engine.ErrNetworkUnavailable
	}
	entered, joined := make(chan struct{}), make(chan struct{})
	a.deps.setup = func(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
		progress(relaysetup.Progress{
			Stage:           "approval_required",
			VerificationURL: "https://console.example/approve",
			UserCode:        "ABCD-EFGH",
			ServiceLogin:    &relaysetup.ServiceLogin{URL: "https://account.example/device#invite=token", Host: "account.example"},
		})
		close(entered)
		<-ctx.Done()
		close(joined)
		return SetupResult{FrontendSaved: true, RelayURL: input.RelayURL}, ctx.Err()
	}
	var cancellations atomic.Int32
	a.deps.cancelSetup = func(_ context.Context, address string) error {
		select {
		case <-joined:
		default:
			t.Error("enrollment cleanup raced the running setup")
		}
		if address != setupRequest().Setup.RelayURL {
			t.Error("cancellation targeted another relay")
		}
		if cancellations.Add(1) == 1 {
			return errors.New("relay unreachable")
		}
		return nil
	}
	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	if reply := a.execute(setupRequest()); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	<-entered
	if running := a.snapshot().Setup; running.ServiceLogin == nil || running.VerificationURL == "" || running.UserCode == "" {
		t.Fatalf("approval state was not populated before cancellation: %+v", running)
	}
	reply := a.execute(Request{Command: "cancel", OperationID: "operation"})
	if reply.Snapshot.Shares[0].State != Finished || reply.Snapshot.Setup.VerificationURL != "" ||
		reply.Snapshot.Setup.UserCode != "" || reply.Snapshot.Setup.ServiceLogin != nil {
		t.Fatalf("cancellation did not fence admission and approval: %+v", reply.Snapshot.Setup)
	}
	a.work.Wait()
	view := a.snapshot().Setup
	if view.State != "cancel_failed" || !view.FrontendSaved || view.Error == nil || view.Recovery != relaysetup.RecoveryRetryCancel ||
		view.VerificationURL != "" || view.UserCode != "" || view.ServiceLogin != nil {
		t.Fatalf("lost cancellation receipt: %+v", view)
	}
	retryReply := a.execute(Request{Command: "cancel_setup", SetupID: view.ID})
	if retryReply.Error != nil {
		t.Fatal(retryReply.Error)
	}
	if retryReply.Snapshot.Setup.State != "cancelling" || retryReply.Snapshot.Setup.Recovery != "" {
		t.Fatalf("cancelling snapshot leaked recovery state: %+v", retryReply.Snapshot.Setup)
	}
	a.work.Wait()
	settled := a.snapshot().Setup
	if settled.State != "cancelled" || settled.VerificationURL != "" || settled.UserCode != "" || settled.ServiceLogin != nil ||
		cancellations.Load() != 2 || starts.Load() != 0 {
		t.Fatalf("retry did not clean up the original setup without admitting a share: %+v", settled)
	}
	a.execute(Request{Command: "cancel_setup", SetupID: view.ID})
	a.work.Wait()
	if cancellations.Load() != 2 {
		t.Fatal("replayed cancellation repeated completed cleanup")
	}
}

func TestShutdownWaitsForEnrollmentCancellation(t *testing.T) {
	a, _, _ := fixture(t)
	entered, cleanup, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	a.deps.setup = func(ctx context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		close(entered)
		<-ctx.Done()
		return SetupResult{RelayURL: input.RelayURL}, ctx.Err()
	}
	a.deps.cancelSetup = func(context.Context, string) error {
		close(cleanup)
		<-release
		return nil
	}
	request := setupRequest()
	request.OperationID = ""
	a.execute(request)
	<-entered
	done := make(chan error, 1)
	go func() { done <- a.close() }()
	<-cleanup
	select {
	case <-done:
		t.Error("shutdown returned before enrollment cleanup joined")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSetupCleanupDoesNotCancelUnacquiredOrCommittedApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		receipt SetupView
	}{
		{name: "restart", receipt: SetupView{State: "failed", RelayURL: "https://relay.example", Recovery: relaysetup.RecoveryRestart}},
		{name: "blocked", receipt: SetupView{State: "failed", RelayURL: "https://relay.example", Recovery: relaysetup.RecoveryBlocked}},
		{name: "relay_saved", receipt: SetupView{State: "cancelling", RelayURL: "https://relay.example", RelaySaved: true}},
		{name: "empty_relay", receipt: SetupView{State: "cancelling"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := fixture(t)
			receipt := tc.receipt
			receipt.VerificationURL = "https://console.example/approve"
			receipt.UserCode = "ABCD-EFGH"
			receipt.ServiceLogin = &relaysetup.ServiceLogin{URL: "https://account.example/device#invite=token", Host: "account.example"}
			a.setup = receipt
			a.deps.cancelSetup = func(context.Context, string) error {
				t.Error("cleanup attempted to revoke another workflow's approval")
				return nil
			}
			a.finishSetupCancellation(setupCleanupFor(receipt.RelayURL, receipt.RelaySaved, receipt.Recovery))
			settled := a.snapshot().Setup
			if settled.State != "cancelled" || settled.Recovery != "" || settled.VerificationURL != "" || settled.UserCode != "" || settled.ServiceLogin != nil {
				t.Fatalf("finished setup did not settle cleanly: %+v", settled)
			}
		})
	}
}

func TestStaleSetupCancellationCannotCancelNewSetup(t *testing.T) {
	a, _, _ := fixture(t)
	a.deps.setup = func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{}, nil
	}
	request := setupRequest()
	request.OperationID = ""
	a.execute(request)
	a.work.Wait()
	previous := request.Setup.ID
	request.Setup = &SetupInput{ID: "new-setup", FrontURL: "https://new.example"}
	a.execute(request)
	a.work.Wait()
	if reply := a.execute(Request{Command: "cancel_setup", SetupID: previous}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	if view := a.snapshot().Setup; view.ID != request.Setup.ID || view.State != "finished" {
		t.Fatalf("stale cancellation changed another setup: %+v", view)
	}
}

func TestLateSetupCancellationDoesNotCancelResumedShare(t *testing.T) {
	a, _, starts := fixture(t)
	var saved atomic.Bool
	start := a.deps.start
	a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
		if !saved.Load() {
			return nil, engine.ErrNetworkUnavailable
		}
		return start(ctx, request)
	}
	a.deps.setup = func(context.Context, SetupInput, func(relaysetup.Progress)) (SetupResult, error) {
		saved.Store(true)
		return SetupResult{RelayURL: setupRequest().Setup.RelayURL, RelaySaved: true}, nil
	}
	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	if reply := a.execute(setupRequest()); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	before := a.snapshot()
	if before.Setup.State != "finished" || starts.Load() != 1 || before.Shares[0].State == Finished {
		t.Fatalf("setup did not finish and resume share: setup=%+v share=%+v starts=%d", before.Setup, before.Shares[0], starts.Load())
	}

	reply := a.execute(Request{Command: "cancel_setup", SetupID: before.Setup.ID})
	if reply.Error != nil {
		t.Fatalf("late cancel_setup failed: %+v", reply.Error)
	}
	if reply.Snapshot.Revision != before.Revision {
		t.Fatalf("late cancel_setup mutated state: before=%d after=%d", before.Revision, reply.Snapshot.Revision)
	}
	after := a.snapshot()
	if after.Setup.State != "finished" || after.Shares[0].State == Finished || after.Shares[0].State == Stopping {
		t.Fatalf("late cancel_setup cancelled resumed share: setup=%+v share=%+v", after.Setup, after.Shares[0])
	}
}

func TestShareScopedSetupCancellationPreservesBlockedShareForRetry(t *testing.T) {
	a, _, starts := fixture(t)
	var saved atomic.Bool
	start := a.deps.start
	a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
		if !saved.Load() {
			return nil, engine.ErrNetworkUnavailable
		}
		return start(ctx, request)
	}
	entered := make(chan struct{})
	var cleanedUp atomic.Int32
	a.deps.setup = func(ctx context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		if input.ID == "setup:1" {
			close(entered)
			<-ctx.Done()
			return SetupResult{RelayURL: input.RelayURL}, ctx.Err()
		}
		saved.Store(true)
		return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
	}
	a.deps.cancelSetup = func(_ context.Context, address string) error {
		if address != setupRequest().Setup.RelayURL {
			t.Errorf("unexpected cleanup address: %q", address)
		}
		cleanedUp.Add(1)
		return nil
	}

	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
	first := setupRequest()
	if reply := a.execute(first); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	<-entered

	if reply := a.execute(Request{Command: "cancel_setup", SetupID: first.Setup.ID}); reply.Error != nil {
		t.Fatalf("cancel_setup failed: %+v", reply.Error)
	}
	a.work.Wait()

	afterCancel := a.snapshot()
	if afterCancel.Setup.State != "cancelled" || cleanedUp.Load() != 1 || starts.Load() != 0 {
		t.Fatalf("setup cancellation did not settle cleanly: setup=%+v cleanedUp=%d starts=%d", afterCancel.Setup, cleanedUp.Load(), starts.Load())
	}
	if afterCancel.Shares[0].State != AwaitingNetwork {
		t.Fatalf("cancel_setup terminated blocked share: share=%+v", afterCancel.Shares[0])
	}

	retry := setupRequest()
	retry.Setup.ID = "setup:2"
	retry.Setup.RelayURL = "https://replacement.example"
	if reply := a.execute(retry); reply.Error != nil {
		t.Fatalf("retry configure after setup cancellation rejected: %+v", reply.Error)
	}
	admitted(t, a)
	a.work.Wait()
	if a.snapshot().Setup.State != "finished" || starts.Load() != 1 {
		t.Fatalf("retry configure did not resume preserved share: setup=%+v starts=%d", a.snapshot().Setup, starts.Load())
	}
}

func TestConfigureRejectsFinishedRunningShare(t *testing.T) {
	a, task, _ := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	task.StopShare()
	task.finish()
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })

	reply := a.execute(setupRequest())
	if reply.Error == nil || reply.Error.Message != "creation operation has finished" {
		t.Fatalf("expected finished operation rejection, got: %+v", reply.Error)
	}
}

func TestCancelledSetupWithoutRelayReceiptSkipsCleanupAndOmitsRecovery(t *testing.T) {
	a, _, _ := fixture(t)
	entered := make(chan struct{})
	a.deps.setup = func(ctx context.Context, _ SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		close(entered)
		<-ctx.Done()
		return SetupResult{}, ctx.Err()
	}
	a.deps.cancelSetup = func(context.Context, string) error {
		t.Error("cleanup attempted to cancel without a relay receipt")
		return nil
	}
	request := setupRequest()
	request.OperationID = ""
	if reply := a.execute(request); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	<-entered
	cancelReply := a.execute(Request{Command: "cancel_setup", SetupID: request.Setup.ID})
	if cancelReply.Error != nil {
		t.Fatal(cancelReply.Error)
	}
	if cancelReply.Snapshot.Setup.State != "cancelling" || cancelReply.Snapshot.Setup.Recovery != "" {
		t.Fatalf("cancelling snapshot leaked recovery: %+v", cancelReply.Snapshot.Setup)
	}
	a.work.Wait()
	settled := a.snapshot().Setup
	if settled.State != "cancelled" || settled.Recovery != "" {
		t.Fatalf("cancelled setup did not settle cleanly: %+v", settled)
	}
}

func TestGuestOptInRacePreservesCommittedOwnerAndNormalGuestSwitchStillWorks(t *testing.T) {
	t.Run("owner_approval_wins_race", func(t *testing.T) {
		a, _, starts := fixture(t)
		var saved atomic.Bool
		start := a.deps.start
		a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
			if !saved.Load() {
				return nil, engine.ErrNetworkUnavailable
			}
			return start(ctx, request)
		}
		var setupCalls atomic.Int32
		var racePending atomic.Bool
		var supersededLogged atomic.Bool
		a.deps.log = func(event string, fields any) {
			if event == "host.setup_guest_superseded" {
				if payload, ok := fields.(map[string]any); ok && payload["relay_origin"] == "https://chosen.example" {
					supersededLogged.Store(true)
				}
			}
		}
		a.deps.setup = func(_ context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
			setupCalls.Add(1)
			if input.Anonymous {
				t.Error("guest retry overwrote committed owner credential")
			}
			progress(relaysetup.Progress{
				Stage:           "approval_required",
				GuestAvailable:  true,
				VerificationURL: "https://console.example/approve",
				UserCode:        "ABCD-EFGH",
			})
			saved.Store(true)
			racePending.Store(true)
			return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
		}
		a.deps.cancelSetup = func(context.Context, string) error {
			t.Error("cleanup should not run when owner approval already committed")
			return nil
		}
		a.deps.load = func() (relayconfig.Config, error) {
			// Simulate clicking "Connect as guest" right after owner enrollment
			// commits on disk, before runSetup acquires a.mu.
			if racePending.CompareAndSwap(true, false) {
				if reply := a.execute(Request{Command: "guest_setup", SetupID: "setup:1"}); reply.Error != nil {
					t.Errorf("guest_setup rejected during race window: %+v", reply.Error)
				}
			}
			return configured(), nil
		}

		a.execute(createRequest())
		awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
		if reply := a.execute(setupRequest()); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		admitted(t, a)
		a.work.Wait()

		view := a.snapshot().Setup
		if view.State != "finished" || !view.RelaySaved || view.Error != nil || setupCalls.Load() != 1 || starts.Load() != 1 || !supersededLogged.Load() {
			t.Fatalf("owner race did not finish cleanly: setup=%+v calls=%d starts=%d logged=%v", view, setupCalls.Load(), starts.Load(), supersededLogged.Load())
		}
	})

	t.Run("guest_opt_in_interrupts_pending_approval", func(t *testing.T) {
		a, _, starts := fixture(t)
		var saved atomic.Bool
		start := a.deps.start
		a.deps.start = func(ctx context.Context, request engine.ShareRequest) (shareTask, error) {
			if !saved.Load() {
				return nil, engine.ErrNetworkUnavailable
			}
			return start(ctx, request)
		}
		approvalReady := make(chan struct{})
		var anonymousSeen atomic.Bool
		a.deps.setup = func(ctx context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
			if !input.Anonymous {
				progress(relaysetup.Progress{
					Stage:           "approval_required",
					GuestAvailable:  true,
					VerificationURL: "https://console.example/approve",
					UserCode:        "ABCD-EFGH",
				})
				close(approvalReady)
				<-ctx.Done()
				return SetupResult{RelayURL: input.RelayURL}, ctx.Err()
			}
			if input.FrontURL != "" {
				t.Errorf("guest retry re-sent front_url: %q", input.FrontURL)
			}
			anonymousSeen.Store(true)
			saved.Store(true)
			return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
		}

		a.execute(createRequest())
		awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
		if reply := a.execute(setupRequest()); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		<-approvalReady
		if reply := a.execute(Request{Command: "guest_setup", SetupID: "setup:1"}); reply.Error != nil {
			t.Fatalf("guest_setup failed: %+v", reply.Error)
		}
		admitted(t, a)
		a.work.Wait()

		view := a.snapshot().Setup
		if view.State != "finished" || !view.RelaySaved || !anonymousSeen.Load() || starts.Load() != 1 {
			t.Fatalf("guest opt-in did not complete cleanly: setup=%+v anon=%v starts=%d", view, anonymousSeen.Load(), starts.Load())
		}
	})

	t.Run("explicit_cancel_after_guest_request_still_cancels", func(t *testing.T) {
		a, _, starts := fixture(t)
		a.deps.start = func(context.Context, engine.ShareRequest) (shareTask, error) {
			return nil, engine.ErrNetworkUnavailable
		}
		var racePending atomic.Bool
		a.deps.setup = func(_ context.Context, input SetupInput, progress func(relaysetup.Progress)) (SetupResult, error) {
			progress(relaysetup.Progress{
				Stage:           "approval_required",
				GuestAvailable:  true,
				VerificationURL: "https://console.example/approve",
				UserCode:        "ABCD-EFGH",
			})
			racePending.Store(true)
			return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
		}
		a.deps.cancelSetup = func(context.Context, string) error {
			t.Error("cleanup should not revoke a committed relay")
			return nil
		}
		a.deps.load = func() (relayconfig.Config, error) {
			if racePending.CompareAndSwap(true, false) {
				if reply := a.execute(Request{Command: "guest_setup", SetupID: "setup:1"}); reply.Error != nil {
					t.Errorf("guest_setup failed: %+v", reply.Error)
				}
				if reply := a.execute(Request{Command: "cancel_setup", SetupID: "setup:1"}); reply.Error != nil {
					t.Errorf("cancel_setup failed: %+v", reply.Error)
				}
			}
			return configured(), nil
		}

		a.execute(createRequest())
		awaitShare(t, a, "operation", func(s Share) bool { return s.State == AwaitingNetwork })
		if reply := a.execute(setupRequest()); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		a.work.Wait()

		view := a.snapshot().Setup
		if view.State != "cancelled" || starts.Load() != 0 {
			t.Fatalf("explicit cancel after guest_setup did not cancel without admitting: setup=%+v starts=%d", view, starts.Load())
		}
	})
}
