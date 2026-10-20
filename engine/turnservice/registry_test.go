package turnservice

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turncache"
	"github.com/windshare/windshare/engine/turnconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func fixture(t *testing.T, client clientFunc) (*Registry, *turnaccount.Manager, relayconfig.FileStore, relayconfig.Config) {
	t.Helper()
	store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	config := relayconfig.Config{Profiles: []senderrelay.Candidate{{PublicURL: "https://relay.example", AccessKey: "device-key"}},
		ActiveRelays: []string{"https://relay.example"}, FrontURL: "https://front.example",
		ManagedTURNPolicies: []turnconfig.ManagedPolicy{{RelayURL: "https://relay.example", LocalBudgetBytes: 1234}}}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	cache := turncache.FileStore{Settings: store}
	if err := cache.Save(t.Context(), config.Profiles[0], turnprovider.Capability{ServiceID: "cf-service", Enabled: true}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	manager, err := turnaccount.New(turnaccount.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := New(manager, turnconfig.Dependencies{Client: client}, cache)
	t.Cleanup(func() {
		registry.Close()
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	return registry, manager, store, config
}

func TestRefreshCannotWriteUserSettingsOrBudget(t *testing.T) {
	var store relayconfig.FileStore
	client := clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/turn/capability" {
			t.Fatal("discovery issued credentials")
		}
		_, err := store.Update(t.Context(), func(config *relayconfig.Config) error {
			config.FrontURL = "https://new-front.example"
			config.ActiveRelays = nil
			config.ManagedTURNPolicies[0].LocalBudgetBytes = 5678
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return reply(200, `{"turn":{"service_id":"cf-service","enabled":false}}`), nil
	})
	registry, manager, actualStore, config := fixture(t, client)
	store = actualStore
	registry.Refresh(t.Context(), config.Profiles[0])
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.FrontURL != "https://new-front.example" || len(saved.ActiveRelays) != 0 || saved.ManagedTURNPolicies[0].LocalBudgetBytes != 5678 {
		t.Fatal(saved)
	}
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("disabled authority retained")
	}
	cached, err := registry.cache.Load(config.Profiles[0])
	if err != nil || cached == nil || cached.Enabled {
		t.Fatal(cached, err)
	}
}

func TestTimeoutRetainsAuthorityAndWatchDoesNotBlock(t *testing.T) {
	started := make(chan struct{})
	registry, manager, _, config := fixture(t, func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := registry.Watch(t.Context(), config.Profiles)
	<-started
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 1 {
		t.Fatal("discovery blocked cached inventory")
	}
	stop()
	cached, err := registry.cache.Load(config.Profiles[0])
	if err != nil || cached == nil || !cached.Enabled {
		t.Fatal(cached, err)
	}
}

func TestRevokedDeviceWithdrawsAndStaleDiscoveryCannotOverwriteNewAuthority(t *testing.T) {
	registry, manager, _, config := fixture(t, func(*http.Request) (*http.Response, error) { return reply(403, "{}"), nil })
	registry.Refresh(t.Context(), config.Profiles[0])
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("revoked device retained authority")
	}

	var current *Registry
	var settings relayconfig.Config
	registry, manager, _, config = fixture(t, func(*http.Request) (*http.Response, error) {
		settings.Profiles[0].AccessKey = "replacement-device-key"
		settings.ManagedTURNPolicies[0].LocalBudgetBytes = 99
		if err := current.Configure(settings); err != nil {
			t.Fatal(err)
		}
		return reply(200, `{"turn":{"service_id":"cf-other","enabled":true}}`), nil
	})
	current, settings = registry, config
	oldCandidate := config.Profiles[0]
	registry.Refresh(t.Context(), oldCandidate)
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("stale authority was published after reconfiguration")
	}
	cached, err := registry.cache.Load(oldCandidate)
	if err != nil || cached == nil || cached.ServiceID != "cf-service" {
		t.Fatal("stale result overwrote cache", cached, err)
	}
}
