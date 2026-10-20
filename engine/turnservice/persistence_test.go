package turnservice

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/windshare/windshare/engine/relayconfig"
)

type storeFunc func(context.Context, func(*relayconfig.Config) error) (relayconfig.Config, error)

func (f storeFunc) Update(ctx context.Context, edit func(*relayconfig.Config) error) (relayconfig.Config, error) {
	return f(ctx, edit)
}

func TestPersistenceFailureWithdrawsImmediatelyAndRetriesWithoutResettingInventory(t *testing.T) {
	registry, manager, store, config := fixture(t, func(*http.Request) (*http.Response, error) {
		return reply(200, "{\"turn\":{\"service_id\":\"cf-service\",\"enabled\":false}}"), nil
	})
	failure := true
	registry.store = storeFunc(func(ctx context.Context, edit func(*relayconfig.Config) error) (relayconfig.Config, error) {
		if failure {
			return relayconfig.Config{}, errors.New("disk temporarily unavailable")
		}
		return store.Update(ctx, edit)
	})
	registry.Refresh(t.Context(), config.Profiles[0])
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("failed save retained withdrawn authority")
	}
	saved, _ := store.Load()
	if !saved.ManagedTURN[0].Enabled {
		t.Fatal("injected failure did not fail")
	}
	revision := manager.Inventory().Revision
	failure = false
	registry.Refresh(t.Context(), config.Profiles[0])
	saved, _ = store.Load()
	if saved.ManagedTURN[0].Enabled {
		t.Fatal("discovery did not retry persistence")
	}
	if manager.Inventory().Revision != revision {
		t.Fatal("persistence retry interrupted unrelated issuance")
	}
}
