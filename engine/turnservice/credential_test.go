package turnservice

import (
	"github.com/windshare/windshare/engine/turnprovider"
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
)

func TestWatchUsesCurrentDeviceAuthorityWhileDiscoveryIsPending(t *testing.T) {
	started := make(chan struct{})
	issuedWith := make(chan string, 1)
	registry, manager, _, config := fixture(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/turn/capability" {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		issuedWith <- request.Header.Get("Authorization")
		return reply(http.StatusServiceUnavailable, "{}"), nil
	})
	config.Profiles[0].AccessKey = "replacement-device"
	if err := registry.cache.Save(t.Context(), config.Profiles[0], turnprovider.Capability{ServiceID: "cf-service", Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	stop := registry.Watch(t.Context(), config.Profiles)
	defer stop()
	<-started
	selection := manager.Select(config.ActiveRelays)
	profile := selection.Inventory().Profiles[0]
	_, err := selection.Acquire(t.Context(), profile.Identity.Profile, turnpath.Subject{
		SessionID: [16]byte{1}, PathID: [16]byte{2},
	})
	if err == nil {
		t.Fatal("provider failure ignored")
	}
	if header := <-issuedWith; header != "Bearer replacement-device" {
		t.Fatalf("issued with stale authority: %s", header)
	}
}
