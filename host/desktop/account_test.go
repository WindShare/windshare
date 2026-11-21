package desktop

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/account"
)

type controlledAccount struct {
	started  chan struct{}
	renewals int
	changed  chan struct{}
	status   account.Snapshot
	calls    atomic.Int32
	finished atomic.Bool
	reload   func(context.Context) error
}

func (service *controlledAccount) Snapshot() (account.Snapshot, <-chan struct{}) {
	if service.status.State != "" {
		return service.status, service.changed
	}
	return account.Snapshot{State: account.Pending}, service.changed
}
func (service *controlledAccount) ReloadSelection(ctx context.Context) error {
	return service.reload(ctx)
}
func (service *controlledAccount) ObserveAccount() func() {
	service.renewals++
	return func() { service.renewals-- }
}
func (*controlledAccount) RefreshOnce(context.Context) (account.RefreshOutcome, error) {
	return account.RefreshSucceeded, nil
}
func (*controlledAccount) Register(context.Context) error { return nil }
func (service *controlledAccount) Login(ctx context.Context, _, _ string) error {
	service.calls.Add(1)
	close(service.started)
	<-ctx.Done()
	service.finished.Store(true)
	return ctx.Err()
}
func (*controlledAccount) SetPassword(context.Context, string, string, string) error { return nil }
func (*controlledAccount) SetNickname(context.Context, string, string) error         { return nil }
func (*controlledAccount) RequestProfileEmail(context.Context, string, string) error { return nil }
func (*controlledAccount) ConfirmProfileEmail(context.Context, string, string, string) error {
	return nil
}
func (*controlledAccount) RequestEmail(context.Context, string) error               { return nil }
func (*controlledAccount) VerifyEmail(context.Context, string) error                { return nil }
func (*controlledAccount) RevokeCredential(context.Context, string, [16]byte) error { return nil }

func TestAccountSelectionReloadCoalescesOpenings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var reloads atomic.Int32
		service := &controlledAccount{changed: make(chan struct{}), reload: func(ctx context.Context) error {
			reloads.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		app := newApplication("test", dependencies{
			accounts: service, log: func(string, any) {}, close: func(context.Context) error { return nil },
		})
		t.Cleanup(func() { _ = app.close() })

		request := Request{ID: "open", Command: "reload_account_selection"}
		if reply := app.execute(request); reply.Error != nil || reply.Snapshot.Account.Operation != nil {
			t.Fatalf("opening the account should not submit an account operation: %+v", reply)
		}
		synctest.Wait()
		if reloads.Load() != 1 {
			t.Fatal("account selection reload did not start")
		}

		for _, id := range []string{"reopen", "reopen-again"} {
			request.ID = id
			if reply := app.execute(request); reply.Error != nil {
				t.Fatalf("reopening the account was rejected: %+v", reply)
			}
		}
		close(release)
		synctest.Wait()
		if reloads.Load() != 2 || service.renewals != 0 {
			t.Fatalf("want one queued reload and no renewal, got %d reloads and %d renewals", reloads.Load(), service.renewals)
		}
	})
}

func TestAccountViewRenewalFollowsItsControlSubscription(t *testing.T) {
	service := &controlledAccount{changed: make(chan struct{})}
	app := newApplication("test", dependencies{
		accounts: service, log: func(string, any) {}, close: func(context.Context) error { return nil },
	})
	t.Cleanup(func() { _ = app.close() })

	if result := app.execute(Request{Command: "list"}); result.Error != nil || service.renewals != 0 {
		t.Fatal("reading a snapshot started account renewal")
	}
	first, disconnectFirst := app.subscribe()
	defer disconnectFirst()
	second, disconnectSecond := app.subscribe()
	defer disconnectSecond()
	visible := true
	request := Request{ID: "1", Command: "set_account_view", AccountViewVisible: &visible}
	for _, current := range []*subscription{first, first, second} {
		if result := app.setAccountView(current, request); result.Error != nil {
			t.Fatalf("account view rejected: %+v", result)
		}
	}
	if service.renewals != 2 || app.snapshot().Account.Operation != nil {
		t.Fatalf("view should own one renewal independently of the form: %d", service.renewals)
	}
	visible = false
	if result := app.setAccountView(first, request); result.Error != nil || service.renewals != 1 {
		t.Fatalf("hiding a view affected another subscription: %+v", result)
	}
	disconnectSecond()
	if service.renewals != 0 {
		t.Fatal("disconnect left a renewal running")
	}
}

// The desktop may only observe the final snapshot, including when two submissions have the same outcome.
func TestAccountCompletionIdentifiesItsSubmission(t *testing.T) {
	app := newApplication("test", dependencies{
		accounts: &controlledAccount{changed: make(chan struct{})},
		log:      func(string, any) {}, close: func(context.Context) error { return nil },
	})
	t.Cleanup(func() { _ = app.close() })
	for _, test := range []struct{ id, action, state string }{
		{"register", "register", "succeeded"}, {"refresh", "refresh", "succeeded"}, {"email-1", "email", "succeeded"}, {"email-2", "email", "succeeded"},
		{"nickname", "nickname", "succeeded"}, {"send-code", "request_profile_email", "succeeded"},
		{"confirm-code", "confirm_profile_email", "succeeded"}, {"invalid", "unknown", "failed"},
	} {
		result := app.execute(Request{ID: "transport-" + test.id, OperationID: test.id, Command: "account", Account: &AccountInput{Action: test.action}})
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		app.work.Wait()
		view := app.snapshot().Account
		if view.ResetLocal != "" {
			t.Fatalf("zero reset time formatted as %q", view.ResetLocal)
		}
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

func TestAccountViewResetLocalRequiresFreshReadyAllowance(t *testing.T) {
	resetAt := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	wantFormatted := resetAt.Local().Format("2006-01-02 15:04 MST")
	for _, tc := range []struct {
		name      string
		status    account.Snapshot
		wantReset string
	}{
		{
			name:      "ready available",
			status:    account.Snapshot{State: account.Ready, Allowance: serviceauthority.AllowanceAvailable, ResetAt: resetAt},
			wantReset: wantFormatted,
		},
		{
			name:      "ready spent",
			status:    account.Snapshot{State: account.Ready, Allowance: serviceauthority.AllowanceSpent, ResetAt: resetAt},
			wantReset: wantFormatted,
		},
		{
			name:      "ready stale omits past reset",
			status:    account.Snapshot{State: account.Ready, Allowance: serviceauthority.AllowanceStale, ResetAt: resetAt},
			wantReset: "",
		},
		{
			name:      "ready unknown allowance omits reset",
			status:    account.Snapshot{State: account.Ready, ResetAt: resetAt},
			wantReset: "",
		},
		{
			name:      "non-ready omits reset",
			status:    account.Snapshot{State: account.Unavailable, Allowance: serviceauthority.AllowanceAvailable, ResetAt: resetAt},
			wantReset: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newApplication("test", dependencies{
				accounts: &controlledAccount{changed: make(chan struct{}), status: tc.status},
				log:      func(string, any) {},
				close:    func(context.Context) error { return nil },
			})
			t.Cleanup(func() { _ = app.close() })
			if got := app.snapshot().Account.ResetLocal; got != tc.wantReset {
				t.Fatalf("ResetLocal = %q, want %q", got, tc.wantReset)
			}
		})
	}
}
