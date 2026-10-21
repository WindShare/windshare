package relaysetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turn"
)

type cacheFunc func(context.Context, senderrelay.Candidate, turn.Capability, time.Time) error

func (save cacheFunc) Save(ctx context.Context, candidate senderrelay.Candidate, capability turn.Capability, at time.Time) error {
	return save(ctx, candidate, capability, at)
}

func TestOnboardingDiscoveryIsOptionalAndPreservesManualAccountsAndBudget(t *testing.T) {
	for _, cacheFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(cacheFailure), func(t *testing.T) {
			saved := pendingCredential(time.Now())
			saved.Profiles = append(saved.Profiles, senderrelay.Candidate{PublicURL: saved.Pending.RelayURL})
			saved.ManagedTURNPolicies = []turn.ManagedPolicy{{RelayURL: saved.Pending.RelayURL, LocalBudgetBytes: 99}}
			for i := range turn.MaximumAccounts {
				id := fmt.Sprintf("manual%d", i)
				saved.TURN.Accounts = append(saved.TURN.Accounts, turn.AccountSettings{ID: id, Provider: "coturn", Issuer: "static",
					Credentials: json.RawMessage(`{"username":"user","password":"password"}`)})
				saved.TURN.Profiles = append(saved.TURN.Profiles, turn.ProfileSettings{ID: id, Account: id, URLs: []string{"turn:localhost:3478"}})
			}
			saved.Pending.SettingsRevision = saved.SettingsRevision()
			store := &memoryStore{saved: saved}
			cached := false
			service := New(Dependencies{Store: store, HTTP: fakeHTTP(func(*http.Request) (*http.Response, error) {
				return response(200, map[string]any{"managed_turn": []any{map[string]any{
					"service_id": "cf-service", "relay_url": saved.Pending.RelayURL, "enabled": true, "local_budget_bytes": 0}}})
			}), Cache: cacheFunc(func(_ context.Context, candidate senderrelay.Candidate, capability turn.Capability, at time.Time) error {
				if store.saved.Pending != nil || candidate.AccessKey != "claimed-key" || capability.ServiceID != "cf-service" || at.IsZero() {
					t.Fatal("cache preceded committed enrollment or used unrelated authority")
				}
				cached = true
				if cacheFailure {
					return errors.New("cache unavailable")
				}
				return nil
			})})
			if _, err := service.Run(t.Context(), Request{URL: saved.Pending.RelayURL, Operation: AddProfile}, nil); err != nil {
				t.Fatal(err)
			}
			if !cached || len(store.saved.TURN.Accounts) != turn.MaximumAccounts || store.saved.ManagedTURNPolicies[0].LocalBudgetBytes != 99 {
				t.Fatal("discovery changed user settings")
			}
		})
	}
}
