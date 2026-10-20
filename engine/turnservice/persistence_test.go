package turnservice

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/turnprovider"
)

type failingCache struct {
	Cache
	fail bool
}

func (cache *failingCache) Save(ctx context.Context, candidate senderrelay.Candidate, value turnprovider.Capability, at time.Time) error {
	if cache.fail {
		return errors.New("disk temporarily unavailable")
	}
	return cache.Cache.Save(ctx, candidate, value, at)
}

func TestPersistenceFailureWithdrawsImmediatelyAndRetriesWithoutResettingInventory(t *testing.T) {
	registry, manager, _, config := fixture(t, func(*http.Request) (*http.Response, error) {
		return reply(200, `{"turn":{"service_id":"cf-service","enabled":false}}`), nil
	})
	cache := &failingCache{Cache: registry.cache, fail: true}
	registry.cache = cache
	registry.Refresh(t.Context(), config.Profiles[0])
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("failed save retained withdrawn authority")
	}
	saved, err := cache.Load(config.Profiles[0])
	if err != nil || saved == nil || !saved.Enabled {
		t.Fatal("injected failure did not fail", err)
	}
	revision := manager.Inventory().Revision
	cache.fail = false
	registry.Refresh(t.Context(), config.Profiles[0])
	saved, err = cache.Load(config.Profiles[0])
	if err != nil || saved == nil || saved.Enabled {
		t.Fatal("discovery did not retry persistence", err)
	}
	if manager.Inventory().Revision != revision {
		t.Fatal("persistence retry interrupted unrelated issuance")
	}
}
