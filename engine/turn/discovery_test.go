package turn

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Date": {time.Now().UTC().Format(http.TimeFormat)}}, Body: io.NopCloser(strings.NewReader(body))}
}

type memoryCache struct {
	mu     sync.Mutex
	values map[senderrelay.Candidate]Capability
}

func (cache *memoryCache) Load(candidate senderrelay.Candidate) (*Capability, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if value, ok := cache.values[candidate]; ok {
		return &value, nil
	}
	return nil, nil
}
func (cache *memoryCache) Save(_ context.Context, candidate senderrelay.Candidate, capability Capability, _ time.Time) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.values[candidate] = capability
	return nil
}

func discoveryFixture(t *testing.T, client clientFunc) (*Runtime, *accountManager, *memoryCache, Settings) {
	t.Helper()
	settings := Settings{Managed: ManagedSettings{
		Relays:   []senderrelay.Candidate{{PublicURL: "https://relay.example", AccessKey: "device-key"}},
		Policies: []ManagedPolicy{{RelayURL: "https://relay.example", LocalBudgetBytes: 1234}},
	}}
	cache := &memoryCache{values: make(map[senderrelay.Candidate]Capability)}
	if err := cache.Save(t.Context(), settings.Managed.Relays[0], Capability{ServiceID: "cf-service", Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(settings, Dependencies{Client: client, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Error(err)
		}
	})
	return runtime, runtime.accounts, cache, settings
}

func watchRuntime(runtime *Runtime, ctx context.Context, relays []senderrelay.Candidate) func() {
	_, release := runtime.ForShare(ctx, relays)
	return release
}

func TestTimeoutRetainsAuthorityAndWatchDoesNotBlock(t *testing.T) {
	started := make(chan struct{})
	registry, manager, _, config := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := watchRuntime(registry, t.Context(), config.Managed.Relays)
	<-started
	if len(manager.Select([]string{config.Managed.Relays[0].PublicURL}).Inventory().Profiles) != 1 {
		t.Fatal("discovery blocked cached inventory")
	}
	stop()
	cached, err := registry.discovery.cache.Load(config.Managed.Relays[0])
	if err != nil || cached == nil || !cached.Enabled {
		t.Fatal(cached, err)
	}
}

func TestRevokedDeviceWithdrawsAndStaleDiscoveryCannotOverwriteNewAuthority(t *testing.T) {
	registry, manager, _, config := discoveryFixture(t, func(*http.Request) (*http.Response, error) { return reply(403, "{}"), nil })
	registry.discovery.Refresh(t.Context(), config.Managed.Relays[0])
	if len(manager.Select([]string{config.Managed.Relays[0].PublicURL}).Inventory().Profiles) != 0 {
		t.Fatal("revoked device retained authority")
	}

	var current *Runtime
	var settings Settings
	registry, manager, _, config = discoveryFixture(t, func(*http.Request) (*http.Response, error) {
		settings.Managed.Relays[0].AccessKey = "replacement-device-key"
		settings.Managed.Policies[0].LocalBudgetBytes = 99
		if err := current.Configure(settings); err != nil {
			t.Fatal(err)
		}
		return reply(200, `{"turn":{"service_id":"cf-other","enabled":true}}`), nil
	})
	current, settings = registry, config
	oldCandidate := config.Managed.Relays[0]
	registry.discovery.Refresh(t.Context(), oldCandidate)
	if len(manager.Select([]string{config.Managed.Relays[0].PublicURL}).Inventory().Profiles) != 0 {
		t.Fatal("stale authority was published after reconfiguration")
	}
	cached, err := registry.discovery.cache.Load(oldCandidate)
	if err != nil || cached == nil || cached.ServiceID != "cf-service" {
		t.Fatal("stale result overwrote cache", cached, err)
	}
}
