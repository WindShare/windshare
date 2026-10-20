package turnservice

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
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
		ManagedTURN: []turnconfig.ManagedService{{ServiceID: "cf-service", RelayURL: "https://relay.example", Enabled: true, LocalBudgetBytes: 1234}}}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	manager, err := turnaccount.New(turnaccount.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := New(manager, turnconfig.Dependencies{Client: client}, store)
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	return registry, manager, store, config
}
func TestRefreshPreservesSettingsAndExistingBudget(t *testing.T) {
	var store relayconfig.FileStore
	client := clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/turn/capability" {
			t.Fatal("discovery issued credentials")
		}
		_, err := store.Update(t.Context(), func(config *relayconfig.Config) error {
			config.FrontURL = "https://new-front.example"
			config.ActiveRelays = nil
			config.ManagedTURN[0].LocalBudgetBytes = 5678
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return reply(200, "{\"turn\":{\"service_id\":\"cf-service\",\"enabled\":false}}"), nil
	})
	registry, manager, actualStore, config := fixture(t, client)
	store = actualStore
	registry.Refresh(t.Context(), config.Profiles[0])
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.FrontURL != "https://new-front.example" || len(saved.ActiveRelays) != 0 ||
		saved.ManagedTURN[0].Enabled || saved.ManagedTURN[0].LocalBudgetBytes != 5678 {
		t.Fatal(saved)
	}
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("disabled authority retained")
	}
}
func TestTimeoutRetainsAuthorityAndWatchDoesNotBlock(t *testing.T) {
	started := make(chan struct{})
	client := clientFunc(func(request *http.Request) (*http.Response, error) {
		close(started)
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	registry, manager, store, config := fixture(t, client)
	stop := registry.Watch(t.Context(), config.Profiles)
	<-started
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 1 {
		t.Fatal("discovery blocked cached inventory")
	}
	stop()
	saved, err := store.Load()
	if err != nil || !saved.ManagedTURN[0].Enabled {
		t.Fatal(saved, err)
	}
}
func TestRevokedDeviceWithdrawsAndStaleDiscoveryCannotOverwriteNewSettings(t *testing.T) {
	registry, manager, _, config := fixture(t, func(*http.Request) (*http.Response, error) {
		return reply(403, "{}"), nil
	})
	registry.Refresh(t.Context(), config.Profiles[0])
	if len(manager.Select(config.ActiveRelays).Inventory().Profiles) != 0 {
		t.Fatal("revoked device retained authority")
	}

	var store relayconfig.FileStore
	registry, manager, store, config = fixture(t, func(*http.Request) (*http.Response, error) {
		_, err := store.Update(t.Context(), func(current *relayconfig.Config) error {
			current.Profiles[0].AccessKey = "replacement-device-key"
			current.ManagedTURN[0].LocalBudgetBytes = 99
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return reply(200, "{\"turn\":{\"service_id\":\"cf-other\",\"enabled\":true}}"), nil
	})
	registry.Refresh(t.Context(), config.Profiles[0])
	saved, _ := store.Load()
	if saved.ManagedTURN[0].ServiceID != "cf-service" || saved.ManagedTURN[0].LocalBudgetBytes != 99 {
		t.Fatal(saved)
	}
	if manager.Snapshot()[0].AccountID != "managed.cf-service" {
		t.Fatal("stale discovery published")
	}
}
