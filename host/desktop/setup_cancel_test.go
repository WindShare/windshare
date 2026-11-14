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
		close(entered)
		progress(relaysetup.Progress{Stage: "approval_required", VerificationURL: "https://console.example/approve"})
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
	reply := a.execute(Request{Command: "cancel", OperationID: "operation"})
	if reply.Snapshot.Shares[0].State != Finished || reply.Snapshot.Setup.VerificationURL != "" {
		t.Fatal("cancellation did not fence admission and approval")
	}
	a.work.Wait()
	view := a.snapshot().Setup
	if view.State != "cancel_failed" || !view.FrontendSaved || view.Error == nil || view.Recovery != relaysetup.RecoveryRetryCancel {
		t.Fatalf("lost cancellation receipt: %+v", view)
	}
	if reply := a.execute(Request{Command: "cancel_setup", SetupID: view.ID}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if a.snapshot().Setup.State != "cancelled" || cancellations.Load() != 2 || starts.Load() != 0 {
		t.Fatal("retry did not clean up the original setup without admitting a share")
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
			a.setup = receipt
			a.deps.cancelSetup = func(context.Context, string) error {
				t.Error("cleanup attempted to revoke another workflow's approval")
				return nil
			}
			a.finishSetupCancellation()
			if a.snapshot().Setup.State != "cancelled" {
				t.Fatal("finished setup did not settle")
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
