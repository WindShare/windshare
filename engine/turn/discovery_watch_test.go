package turn

import (
	"context"
	"net/http"
	"testing"

	"github.com/windshare/windshare/connectivity/senderrelay"
)

func TestSharesUseOneDiscoveryWorkerAndReleaseIndependently(t *testing.T) {
	requests := make(chan context.Context, 2)
	registry, manager, _, config := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	revision := manager.Inventory().Revision
	first := watchRuntime(registry, t.Context(), config.Managed.Relays)
	request := <-requests
	address := addressKey(config.Managed.Relays[0].PublicURL)
	registry.discovery.mu.Lock()
	generation := registry.discovery.entries[address].generation
	registry.discovery.mu.Unlock()
	equivalent := config.Managed.Relays[0]
	equivalent.PublicURL = "wss://RELAY.example:443/"
	second := watchRuntime(registry, t.Context(), []senderrelay.Candidate{equivalent, equivalent})
	registry.discovery.mu.Lock()
	if registry.discovery.demand[address] != 2 || registry.discovery.entries[address].generation != generation {
		t.Error("second share started another worker or duplicated its subscription")
	}
	registry.discovery.mu.Unlock()
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
	registry.discovery.mu.Lock()
	if len(registry.discovery.demand) != 0 || registry.discovery.entries[address].cancel != nil {
		t.Error("idle registry retained a worker")
	}
	registry.discovery.mu.Unlock()
	restarted := watchRuntime(registry, t.Context(), config.Managed.Relays)
	<-requests
	restarted()
}

func TestDemandSurvivesRemovalAndReadditionWithoutRevivingOldRequests(t *testing.T) {
	requests := make(chan context.Context, 2)
	registry, _, _, config := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := watchRuntime(registry, t.Context(), config.Managed.Relays)
	first := <-requests
	if err := registry.Configure(Settings{}); err != nil {
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
	registry, manager, _, config := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		requests <- request.Context()
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	stop := watchRuntime(registry, t.Context(), config.Managed.Relays)
	defer stop()
	request := <-requests
	config.Managed.Policies[0].LocalBudgetBytes = 5678
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
