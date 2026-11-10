package relaysetup

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func setupRecovery(t *testing.T, err error) Recovery {
	t.Helper()
	var failure *SetupError
	if !errors.As(err, &failure) {
		t.Fatalf("setup failure has no recovery outcome: %v", err)
	}
	return failure.Recovery
}

func TestSetupRecoveryDescribesAvailableAction(t *testing.T) {
	now := time.Unix(100, 0)
	pending := expiredPending(now)
	setExpiry(pending, now.Add(time.Minute).UnixMilli())
	request := Request{URL: pending.RelayURL, Operation: AddProfile}
	for _, scenario := range []struct {
		name    string
		phase   setupPhase
		change  func(*Snapshot, *Request)
		action  RecoveryAction
		wantErr error
	}{
		{name: "discovery", action: RecoveryRestart, wantErr: context.Canceled},
		{name: "requesting", phase: requesting{}, action: RecoveryContinue, wantErr: context.Canceled},
		{name: "approval", phase: pending.phase, action: RecoveryContinue, wantErr: context.Canceled},
		{name: "installation", phase: installing{Enrollment: pending.enrollment(), Credential: credential{ID: "credential", AccessKey: "key"}},
			action: RecoveryContinue, wantErr: context.Canceled},
		{name: "cancelling", phase: cancelling{EnrollmentID: pending.enrollment().ID}, action: RecoveryRetryCancel, wantErr: context.Canceled},
		{name: "changed settings", phase: pending.phase, action: RecoveryCancel, wantErr: ErrSettingsChanged,
			change: func(saved *Snapshot, _ *Request) { saved.FrontURL = "https://changed.example" }},
		{name: "other operation", phase: pending.phase, action: RecoveryBlocked, wantErr: ErrPendingSetup,
			change: func(_ *Snapshot, request *Request) { request.Operation = PrivateOnboarding }},
		{name: "other relay", phase: pending.phase, action: RecoveryBlocked, wantErr: ErrPendingSetup,
			change: func(_ *Snapshot, request *Request) { request.URL = "https://other.example" }},
		{name: "expired", phase: expiredPending(now).phase, action: RecoveryRestart, wantErr: ErrExpired},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			saved, request := prior(), request
			if scenario.phase != nil {
				saved.Pending = pending.withPhase(scenario.phase)
			}
			if scenario.change != nil {
				scenario.change(&saved, &request)
			}
			store := &memoryStore{saved: saved}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			service := New(Dependencies{Store: store, Now: func() time.Time { return now },
				HTTP: fakeHTTP(func(*http.Request) (*http.Response, error) {
					cancel()
					return nil, context.Canceled
				})})
			_, err := service.Run(ctx, request, nil)
			if !errors.Is(err, scenario.wantErr) {
				t.Fatalf("Run = %v, want %v", err, scenario.wantErr)
			}
			recovery := setupRecovery(t, err)
			if recovery.Action != scenario.action || recovery.RelayURL != pending.RelayURL {
				t.Fatalf("recovery = %+v, want %s for %s", recovery, scenario.action, pending.RelayURL)
			}
			if scenario.action == RecoveryRestart && store.saved.Pending != nil {
				t.Fatal("restart would leave a blocking pending record")
			}
		})
	}
}

// A failed commit is ambiguous even when a particular store implementation
// happened to finish applying it. Recovery must not promise either outcome.
type uncertainSetupStore struct {
	*memoryStore
	loadErr error
	commit  bool
	failure error
}

func (store uncertainSetupStore) Load() (Snapshot, error) {
	if store.loadErr != nil {
		return Snapshot{}, store.loadErr
	}
	return store.memoryStore.Load()
}

func (store uncertainSetupStore) Update(ctx context.Context, change func(*Snapshot) error) (Snapshot, error) {
	if !store.commit {
		return Snapshot{}, store.failure
	}
	saved, err := store.memoryStore.Update(ctx, change)
	if err != nil {
		return saved, err
	}
	return saved, store.failure
}

func TestSetupRecoveryDoesNotGuessAfterStorageFailure(t *testing.T) {
	failure := errors.New("storage unavailable")
	for _, scenario := range []string{"load", "before commit", "after commit"} {
		t.Run(scenario, func(t *testing.T) {
			store := uncertainSetupStore{memoryStore: &memoryStore{saved: prior()}, failure: failure, commit: scenario == "after commit"}
			if scenario == "load" {
				store.loadErr = failure
			}
			service := New(Dependencies{Store: store, HTTP: fakeHTTP(func(*http.Request) (*http.Response, error) {
				return response(http.StatusOK, discovery{Version: 1, Access: "open", RelayURL: "https://open.example"})
			})})
			_, err := service.Run(t.Context(), Request{URL: "https://open.example", Operation: AddProfile}, nil)
			if !errors.Is(err, failure) || setupRecovery(t, err).Action != RecoveryUnknown {
				t.Fatalf("uncertain storage outcome = %v", err)
			}
			if store.commit && len(store.saved.Profiles) != 2 {
				t.Fatal("test did not exercise an error after installation")
			}
		})
	}
}
