package desktop

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/engine"
	"github.com/windshare/windshare/engine/account"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/relaysetup"
)

func TestRepeatAuthorizationRetainsSelectionWithoutResurrectingFailedTask(t *testing.T) {
	a, original, starts := fixture(t)
	// Repeat consumes a settled task. Prepare its result before admission publishes it to observers.
	original.finish()
	original.state.Startup = engine.ShareStartupFailed
	original.state.Result.Outcome = engine.OutcomeFailed
	original.state.Authorization = &engine.ShareAuthorization{Kind: engine.ShareSelfHostedAuthorization, RelayURL: "https://chosen.example"}
	create := createRequest()
	create.Relays = []string{"https://chosen.example"}
	a.execute(create)
	admitted(t, a)
	request := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	reply := a.execute(request)
	if reply.Error != nil {
		t.Fatal(reply.Error)
	}
	repeated := awaitShare(t, a, request.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })
	if repeated.TaskID != "" || repeated.Authorization.RelayURL != "https://chosen.example" || starts.Load() != 1 {
		t.Fatal("authorization retried a rejected task before setup", repeated)
	}
	a.execute(request)
	if len(a.snapshot().Shares) != 2 {
		t.Fatal("repeat replay duplicated the selection")
	}
	a.mu.Lock()
	if !slices.Equal(a.operations[request.OperationID].paths, a.operations["operation"].paths) {
		t.Error("host did not retain the selected paths")
	}
	a.mu.Unlock()
	var newStarts int
	a.deps.start = func(_ context.Context, request engine.ShareRequest) (shareTask, error) {
		newStarts++
		candidates := request.RelayIntent.Candidates()
		if len(candidates) != 1 || candidates[0].PublicURL != "https://chosen.example" {
			t.Error("authorized selection lost its explicit relay")
		}
		return nil, engine.ErrNetworkUnavailable
	}
	a.deps.setup = func(_ context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
	}
	setup := setupRequest()
	setup.OperationID = request.OperationID
	a.execute(setup)
	a.work.Wait()
	if newStarts != 1 || a.operations["operation"].phase.(running).task.Snapshot().Result.Outcome != engine.OutcomeFailed {
		t.Fatal("setup did not create a fresh task attempt or overwrote the old result")
	}
}

func TestExplicitRelayOutageRetainsFilesAndRetriesTheOriginalSelection(t *testing.T) {
	a, task, _ := fixture(t)
	task.finish()
	task.state.Startup = engine.ShareStartupFailed
	task.state.Result.Outcome = engine.OutcomeFailed
	task.state.Result.FailureClass = engine.FailureNetwork
	task.state.Result.Err = errors.New("relay offline")
	create := createRequest()
	create.Relays = []string{"https://chosen.example"}
	a.execute(create)
	waiting := awaitShare(t, a, create.OperationID, func(s Share) bool { return s.State == AwaitingNetwork })
	if waiting.Blocking == nil || waiting.Blocking.Reason != "selected_relays_unavailable" || waiting.TaskID != "" {
		t.Fatal(waiting)
	}
	a.deps.start = func(_ context.Context, req engine.ShareRequest) (shareTask, error) {
		if candidates := req.RelayIntent.Candidates(); len(candidates) != 1 || candidates[0].PublicURL != create.Relays[0] {
			t.Error("retry changed the selected relay")
		}
		return nil, errors.New("retry observed")
	}
	a.execute(Request{Command: "resume", OperationID: create.OperationID})
	a.work.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.operations[create.OperationID].paths) != 1 {
		t.Fatal("retry lost the chosen files")
	}
}

func TestCancelledRepeatCannotAdmitOnReplay(t *testing.T) {
	a, original, starts := fixture(t)
	a.execute(createRequest())
	admitted(t, a)
	original.finish()
	request := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	a.execute(Request{Command: "cancel", OperationID: request.OperationID})
	if reply := a.execute(request); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if starts.Load() != 1 {
		t.Fatal("replayed repeat escaped cancellation")
	}
}

type stubOfficialAccount struct {
	mu        sync.Mutex
	status    account.Snapshot
	changed   chan struct{}
	onLogin   func(context.Context, string, string) error
	onRefresh func(context.Context) (account.RefreshOutcome, error)
	onReload  func(context.Context) error
}

func newStubOfficialAccount(initial account.Snapshot) *stubOfficialAccount {
	return &stubOfficialAccount{status: initial, changed: make(chan struct{})}
}

func (s *stubOfficialAccount) set(next account.Snapshot) {
	s.mu.Lock()
	s.status = next
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *stubOfficialAccount) Snapshot() (account.Snapshot, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.changed
}

func (s *stubOfficialAccount) ReloadSelection(ctx context.Context) error {
	if s.onReload != nil {
		return s.onReload(ctx)
	}
	return nil
}

func (*stubOfficialAccount) ObserveAccount() func() { return func() {} }
func (s *stubOfficialAccount) RefreshOnce(ctx context.Context) (account.RefreshOutcome, error) {
	if s.onRefresh != nil {
		return s.onRefresh(ctx)
	}
	return account.RefreshSucceeded, nil
}
func (*stubOfficialAccount) Register(context.Context) error { return nil }
func (s *stubOfficialAccount) Login(ctx context.Context, username, password string) error {
	if s.onLogin != nil {
		return s.onLogin(ctx, username, password)
	}
	return nil
}
func (*stubOfficialAccount) SetPassword(context.Context, string, string, string) error { return nil }
func (*stubOfficialAccount) SetNickname(context.Context, string, string) error         { return nil }
func (*stubOfficialAccount) RequestProfileEmail(context.Context, string, string) error { return nil }
func (*stubOfficialAccount) ConfirmProfileEmail(context.Context, string, string, string) error {
	return nil
}
func (*stubOfficialAccount) RequestEmail(context.Context, string) error               { return nil }
func (*stubOfficialAccount) VerifyEmail(context.Context, string) error                { return nil }
func (*stubOfficialAccount) RevokeCredential(context.Context, string, [16]byte) error { return nil }

func officialAccountFixture(t *testing.T, accounts *stubOfficialAccount) (*Application, *fakeShare, *fakeShare, *atomic.Int32) {
	t.Helper()
	original := &fakeShare{
		id: "engine:1", state: engine.ShareSnapshot{State: engine.TaskRunning},
		events: make(chan engine.Observation, 1), changed: make(chan struct{}), done: make(chan struct{}),
	}
	original.finish()
	original.state.Startup = engine.ShareStartupFailed
	original.state.Result.Outcome = engine.OutcomeFailed
	original.state.Authorization = &engine.ShareAuthorization{Kind: engine.ShareOfficialAuthorization}

	replacement := &fakeShare{
		id: "engine:2", state: engine.ShareSnapshot{State: engine.TaskRunning},
		events: make(chan engine.Observation, 1), changed: make(chan struct{}), done: make(chan struct{}),
	}
	starts := &atomic.Int32{}
	a := newApplication("session", dependencies{
		accounts: accounts,
		start: func(context.Context, engine.ShareRequest) (shareTask, error) {
			if starts.Add(1) == 1 {
				return original, nil
			}
			return replacement, nil
		},
		close: func(context.Context) error {
			original.finish()
			replacement.finish()
			return nil
		},
		load:        func() (relayconfig.Config, error) { return configured(), nil },
		log:         func(string, any) {},
		cancelSetup: func(context.Context, string) error { return nil },
	})
	t.Cleanup(func() {
		if err := a.close(); err != nil {
			t.Error(err)
		}
	})
	return a, original, replacement, starts
}

func TestOfficialAuthorizationRepeatResumesAfterLoginAndIgnoresRelaySetup(t *testing.T) {
	expires := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	accounts := newStubOfficialAccount(account.Snapshot{State: account.Denied, Account: "acc-1", Credential: "cred-1"})
	accounts.onLogin = func(_ context.Context, username, _ string) error {
		if username == "denied" {
			accounts.set(account.Snapshot{State: account.Denied, Account: "acc-denied", Credential: "cred-denied"})
			return nil
		}
		accounts.set(account.Snapshot{State: account.Ready, Account: "acc-2", Credential: "cred-2", TicketExpires: expires})
		return nil
	}
	a, _, replacement, starts := officialAccountFixture(t, accounts)
	a.deps.setup = func(_ context.Context, input SetupInput, _ func(relaysetup.Progress)) (SetupResult, error) {
		return SetupResult{RelayURL: input.RelayURL, RelaySaved: true}, nil
	}

	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })

	repeatReq := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	if reply := a.execute(repeatReq); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	repeated := awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })
	if repeated.TaskID != "" || repeated.Authorization == nil || repeated.Authorization.Kind != engine.ShareOfficialAuthorization || starts.Load() != 1 {
		t.Fatalf("official repeat did not wait cleanly before login: %+v starts=%d", repeated, starts.Load())
	}

	cancelledReq := Request{Command: "repeat", OperationID: "repeat:cancelled", SourceOperationID: "operation"}
	if reply := a.execute(cancelledReq); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	if reply := a.execute(Request{Command: "cancel", OperationID: cancelledReq.OperationID}); reply.Error != nil {
		t.Fatal(reply.Error)
	}

	// Saving a relay must not hijack an official authorization wait; switch_relay owns that opt-out.
	setup := setupRequest()
	setup.OperationID = repeatReq.OperationID
	if reply := a.execute(setup); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if starts.Load() != 1 {
		t.Fatalf("relay setup auto-admitted an official authorization wait: starts=%d", starts.Load())
	}

	// Switching to another denied account must keep the repeat parked.
	deniedLogin := Request{
		ID: "login-denied", OperationID: "acc-op:1", Command: "account",
		Account: &AccountInput{Action: "login", Username: "denied", Password: "secret"},
	}
	if reply := a.execute(deniedLogin); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()
	if starts.Load() != 1 {
		t.Fatalf("denied account resumed official authorization wait: starts=%d", starts.Load())
	}

	validLogin := Request{
		ID: "login-valid", OperationID: "acc-op:2", Command: "account",
		Account: &AccountInput{Action: "login", Username: "valid", Password: "secret"},
	}
	if reply := a.execute(validLogin); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	resumed := awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.TaskID == string(replacement.ID()) })
	if resumed.State != Preparing || starts.Load() != 2 {
		t.Fatalf("valid login did not resume repeated official share: %+v starts=%d", resumed, starts.Load())
	}
	if replay := a.execute(repeatReq); replay.Error != nil || starts.Load() != 2 {
		t.Fatalf("replaying repeat after login altered admission: err=%v starts=%d", replay.Error, starts.Load())
	}
	a.mu.Lock()
	samePaths := slices.Equal(a.operations[repeatReq.OperationID].paths, a.operations["operation"].paths)
	cancelledView := projectShare(a.operations[cancelledReq.OperationID])
	a.mu.Unlock()
	if !samePaths || cancelledView.Result == nil || cancelledView.Result.Outcome != OutcomeCancelled {
		t.Fatalf("login lost selection or resurrected cancelled repeat: samePaths=%v cancelled=%+v", samePaths, cancelledView)
	}
}

func TestOfficialAuthorizationRepeatAvoidsLoopOnUnchangedReadyGrantAndResumesOnRefreshOrReload(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	initial := account.Snapshot{State: account.Ready, Account: "acc-1", Credential: "cred-1", TicketExpires: t0}

	t.Run("explicit refresh resumes after relay rejection on cached ticket", func(t *testing.T) {
		accounts := newStubOfficialAccount(initial)
		a, _, replacement, starts := officialAccountFixture(t, accounts)
		a.execute(createRequest())
		awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })

		repeatReq := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
		if reply := a.execute(repeatReq); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })

		// Unrelated account updates carrying the same rejected grant must not spin retries.
		unchanged := initial
		unchanged.RemainingBytes = 42
		accounts.set(unchanged)
		awaitAccount := func(check func(*AccountView) bool) {
			queue, unsubscribe := a.subscribe()
			defer unsubscribe()
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			for !check(a.snapshot().Account) {
				select {
				case <-queue.snapshots:
				case <-deadline.C:
					t.Fatal("account update not observed")
				}
			}
		}
		awaitAccount(func(view *AccountView) bool { return view != nil && view.Account.RemainingBytes == 42 })
		if starts.Load() != 1 {
			t.Fatalf("unrelated account update retried rejected grant: starts=%d", starts.Load())
		}

		refreshReq := Request{
			ID: "refresh", OperationID: "acc-op:refresh", Command: "account",
			Account: &AccountInput{Action: "refresh"},
		}
		if reply := a.execute(refreshReq); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.TaskID == string(replacement.ID()) })
		if starts.Load() != 2 {
			t.Fatalf("explicit refresh did not resume repeated share: starts=%d", starts.Load())
		}
	})

	t.Run("external credential reload resumes parked official wait", func(t *testing.T) {
		accounts := newStubOfficialAccount(initial)
		accounts.onReload = func(context.Context) error {
			accounts.set(account.Snapshot{State: account.Ready, Account: "acc-2", Credential: "cred-2", TicketExpires: t0.Add(time.Hour)})
			return nil
		}
		a, _, replacement, starts := officialAccountFixture(t, accounts)
		a.execute(createRequest())
		awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })

		repeatReq := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
		if reply := a.execute(repeatReq); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.State == AwaitingAuthorization })

		if reply := a.execute(Request{ID: "reload", Command: "reload_account_selection"}); reply.Error != nil {
			t.Fatal(reply.Error)
		}
		awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.TaskID == string(replacement.ID()) })
		if starts.Load() != 2 {
			t.Fatalf("reloaded account did not resume repeated share: starts=%d", starts.Load())
		}
	})
}

func TestOfficialAuthorizationRepeatAdmitsImmediatelyWhenAccountAlreadyChanged(t *testing.T) {
	accounts := newStubOfficialAccount(account.Snapshot{State: account.Denied, Account: "acc-1", Credential: "cred-1"})
	accounts.onLogin = func(context.Context, string, string) error {
		accounts.set(account.Snapshot{State: account.Ready, Account: "acc-2", Credential: "cred-2", TicketExpires: time.Date(2026, 11, 21, 13, 0, 0, 0, time.UTC)})
		return nil
	}
	a, _, replacement, starts := officialAccountFixture(t, accounts)
	a.execute(createRequest())
	awaitShare(t, a, "operation", func(s Share) bool { return s.State == Finished })

	if reply := a.execute(Request{
		ID: "login-first", OperationID: "acc-op:1", Command: "account",
		Account: &AccountInput{Action: "login", Username: "valid", Password: "secret"},
	}); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	a.work.Wait()

	repeatReq := Request{Command: "repeat", OperationID: "repeat:1", SourceOperationID: "operation"}
	if reply := a.execute(repeatReq); reply.Error != nil {
		t.Fatal(reply.Error)
	}
	resumed := awaitShare(t, a, repeatReq.OperationID, func(s Share) bool { return s.TaskID == string(replacement.ID()) })
	if resumed.State != Preparing || starts.Load() != 2 {
		t.Fatalf("repeat did not admit immediately after account already changed: %+v starts=%d", resumed, starts.Load())
	}
}
