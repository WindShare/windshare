package desktop

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/engine"
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
	if reply := a.execute(Request{Command: "cancel_setup", SetupID: view.ID}); reply.Error != nil {
		t.Fatal(reply.Error)
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
	for _, receipt := range []SetupView{
		{State: "failed", Recovery: relaysetup.RecoveryRestart},
		{State: "failed", Recovery: relaysetup.RecoveryBlocked},
		{State: "cancelling", RelaySaved: true},
	} {
		t.Run(string(receipt.Recovery), func(t *testing.T) {
			a, _, _ := fixture(t)
			receipt.RelayURL = "https://relay.example"
			receipt.VerificationURL = "https://console.example/approve"
			receipt.UserCode = "ABCD-EFGH"
			receipt.ServiceLogin = &relaysetup.ServiceLogin{URL: "https://account.example/device#invite=token", Host: "account.example"}
			a.setup = receipt
			a.deps.cancelSetup = func(context.Context, string) error {
				t.Error("cleanup attempted to revoke another workflow's approval")
				return nil
			}
			a.finishSetupCancellation()
			settled := a.snapshot().Setup
			if settled.State != "cancelled" || settled.VerificationURL != "" || settled.UserCode != "" || settled.ServiceLogin != nil {
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
