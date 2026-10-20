package turnservice

import (
	"context"
	"net/http"
	"testing"

	"github.com/windshare/windshare/connectivity/senderrelay"
	"github.com/windshare/windshare/engine/relayconfig"
)

func TestSharesUseOneDiscoveryWorkerAndReleaseIndependently(t *testing.T) {
	requests := make(chan context.Context, 2)
	registry, manager, _, config := fixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	revision := manager.Inventory().Revision
	first := registry.Watch(t.Context(), config.Profiles)
	request := <-requests
	address := addressKey(config.Profiles[0].PublicURL)
	registry.mu.Lock()
	generation := registry.entries[address].generation
	registry.mu.Unlock()
	equivalent := config.Profiles[0]
	equivalent.PublicURL = "wss://RELAY.example:443/"
	second := registry.Watch(t.Context(), []senderrelay.Candidate{equivalent, equivalent})
	registry.mu.Lock()
	if registry.demand[address] != 2 || registry.entries[address].generation != generation {
		t.Error("second share started another worker or duplicated its subscription")
	}
	registry.mu.Unlock()
	if manager.Inventory().Revision != revision {
		t.Fatal("share subscriptions invalidated active credential requests")
	}
	first()
	select {
	case <-request.Done():
		t.Fatal("one share cancelled another's discovery")
	default:
	}
	second()
	<-request.Done()
	first()
	second()
	registry.mu.Lock()
	if len(registry.demand) != 0 || registry.entries[address].cancel != nil {
		t.Error("idle registry retained a worker")
	}
	registry.mu.Unlock()
	restarted := registry.Watch(t.Context(), config.Profiles)
	<-requests
	restarted()
}

func TestDemandSurvivesRemovalAndReadditionWithoutRevivingOldRequests(t *testing.T) {
	requests := make(chan context.Context, 2)
	registry, _, _, config := fixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := registry.Watch(t.Context(), config.Profiles)
	first := <-requests
	if err := registry.Configure(relayconfig.Config{Profiles: []senderrelay.Candidate{}}); err != nil {
		t.Fatal(err)
	}
	<-first.Done()
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	second := <-requests
	stop()
	<-second.Done()
}

func TestBudgetReconfigurationDoesNotRestartDiscovery(t *testing.T) {
	requests := make(chan context.Context, 1)
	registry, manager, _, config := fixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := registry.Watch(t.Context(), config.Profiles)
	defer stop()
	request := <-requests
	config.ManagedTURNPolicies[0].LocalBudgetBytes = 5678
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot()[0].LocalBudgetBytes != 5678 {
		t.Fatal("budget did not reach active manager")
	}
	select {
	case <-request.Done():
		t.Fatal("budget edit restarted discovery")
	default:
	}
}
