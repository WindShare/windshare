package desktop

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/engine/account"
)

type controlledAccount struct {
	started  chan struct{}
	changed  chan struct{}
	calls    atomic.Int32
	finished atomic.Bool
}

func (service *controlledAccount) Snapshot() (account.Snapshot, <-chan struct{}) {
	return account.Snapshot{State: account.Pending}, service.changed
}
func (*controlledAccount) Retry() {}
func (service *controlledAccount) Login(ctx context.Context, _, _ string) error {
	service.calls.Add(1)
	close(service.started)
	<-ctx.Done()
	service.finished.Store(true)
	return ctx.Err()
}
func (*controlledAccount) SetPassword(context.Context, string, string) error { return nil }
func (*controlledAccount) RequestEmail(context.Context, string) error        { return nil }
func (*controlledAccount) VerifyEmail(context.Context, string) error         { return nil }
func (*controlledAccount) RevokeCredential(context.Context, [16]byte) error  { return nil }

// The desktop may only observe the final snapshot, including when two submissions have the same outcome.
func TestAccountCompletionIdentifiesItsSubmission(t *testing.T) {
	app := newApplication("test", dependencies{
		accounts: &controlledAccount{changed: make(chan struct{})},
		log:      func(string, any) {}, close: func(context.Context) error { return nil },
	})
	t.Cleanup(func() { _ = app.close() })
	for _, test := range []struct{ id, action, state string }{
		{"email-1", "email", "succeeded"}, {"email-2", "email", "succeeded"}, {"invalid", "unknown", "failed"},
	} {
		result := app.execute(Request{ID: "transport-" + test.id, OperationID: test.id, Command: "account", Account: &AccountInput{Action: test.action}})
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		app.work.Wait()
		view := app.snapshot().Account
		if view.busy() || view.Operation.ID != test.id || view.Operation.State != test.state ||
			(view.Operation.Error != nil) != (test.state == "failed") {
			t.Fatalf("unexpected completion: %+v", view.Operation)
		}
	}
}

func TestAccountCommandReplayAndShutdownOwnOneCancelableOperation(t *testing.T) {
	service := &controlledAccount{started: make(chan struct{}), changed: make(chan struct{})}
	app := newApplication("test", dependencies{
		accounts: service, log: func(string, any) {},
		close: func(context.Context) error {
			if !service.finished.Load() {
				t.Error("engine closed before account command joined")
			}
			return nil
		},
	})
	t.Cleanup(func() { _ = app.close() })
	request := Request{ID: "1", OperationID: "account-login", Command: "account", Account: &AccountInput{
		Action: "login", Username: "alice", Password: "correct horse battery",
	}}
	if result := app.execute(request); result.Error != nil || !result.Snapshot.Account.busy() || result.Snapshot.Account.Operation.ID != request.OperationID {
		t.Fatalf("login was not accepted asynchronously: %+v", result)
	}
	select {
	case <-service.started:
	case <-time.After(time.Second):
		t.Fatal("login did not start")
	}
	if result := app.execute(request); result.Error != nil {
		t.Fatal(result.Error)
	}
	conflicting := request
	input := *request.Account
	input.Username = "bob"
	conflicting.Account = &input
	if result := app.execute(conflicting); result.Error == nil {
		t.Fatal("conflicting replay accepted")
	}
	other := request
	other.OperationID = "account-login-2"
	if result := app.execute(other); result.Error == nil {
		t.Fatal("concurrent account operation accepted")
	}
	if result := app.execute(Request{Command: "list"}); result.Error != nil || !result.Snapshot.active() {
		t.Fatal("account work blocked host snapshots or lost activity")
	}
	if err := app.close(); err != nil {
		t.Fatal(err)
	}
	if service.calls.Load() != 1 {
		t.Fatal("replay duplicated login")
	}
	if result := app.execute(other); result.Error == nil {
		t.Fatal("closed host accepted account work")
	}
}
