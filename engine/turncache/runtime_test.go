package turncache

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turn"
)

type discoveryClient func(*http.Request) (*http.Response, error)

func (client discoveryClient) Do(request *http.Request) (*http.Response, error) {
	return client(request)
}

type observedCache struct {
	FileStore
	saved chan error
}

func (cache observedCache) Save(ctx context.Context, candidate senderrelay.Candidate, capability turn.Capability, at time.Time) error {
	err := cache.FileStore.Save(ctx, candidate, capability, at)
	cache.saved <- err
	return err
}

func TestRefreshCannotWriteUserSettingsOrBudget(t *testing.T) {
	store := relayconfig.FileStore{Path: filepath.Join(t.TempDir(), "config.json")}
	config := relayconfig.Config{
		Profiles:     []senderrelay.Candidate{{PublicURL: "https://relay.example", AccessKey: "device-key"}},
		ActiveRelays: []string{"https://relay.example"}, FrontURL: "https://front.example",
		ManagedTURNPolicies: []turn.ManagedPolicy{{RelayURL: "https://relay.example", LocalBudgetBytes: 1234}},
	}
	if err := store.Save(config); err != nil {
		t.Fatal(err)
	}
	cache := observedCache{FileStore: FileStore{Settings: store}, saved: make(chan error, 1)}
	if err := cache.FileStore.Save(t.Context(), config.Profiles[0], turn.Capability{ServiceID: "cf-service", Enabled: true}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	client := discoveryClient(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/turn/capability" {
			t.Error("discovery issued credentials")
		}
		_, err := store.Update(request.Context(), func(current *relayconfig.Config) error {
			current.FrontURL = "https://new-front.example"
			current.ActiveRelays = nil
			current.ManagedTURNPolicies[0].LocalBudgetBytes = 5678
			return nil
		})
		if err != nil {
			t.Error(err)
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"turn":{"service_id":"cf-service","enabled":false}}`))}, nil
	})
	runtime, err := turn.New(config.TURNSettings(), turn.Dependencies{Client: client, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	selection, release := runtime.ForShare(t.Context(), config.Profiles)
	defer release()
	select {
	case err := <-cache.saved:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("discovery did not persist its observation")
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.FrontURL != "https://new-front.example" || len(saved.ActiveRelays) != 0 || saved.ManagedTURNPolicies[0].LocalBudgetBytes != 5678 {
		t.Fatal("discovery overwrote user settings", saved)
	}
	if len(selection.Inventory().Profiles) != 0 {
		t.Fatal("disabled authority retained")
	}
	cached, err := cache.Load(config.Profiles[0])
	if err != nil || cached == nil || cached.Enabled {
		t.Fatal(cached, err)
	}
}
