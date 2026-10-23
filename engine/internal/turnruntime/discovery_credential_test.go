package turnruntime

import (
	"net/http"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/turnpath"
)

func TestWatchUsesCurrentDeviceAuthorityWhileDiscoveryIsPending(t *testing.T) {
	started := make(chan struct{})
	issuedWith := make(chan string, 1)
	registry, manager, _, config := discoveryFixture(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/api/turn/capability" {
			close(started)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		if request.URL.Path == "/api/turn/revoke" {
			return reply(http.StatusOK, `{"state":"canceled"}`), nil
		}
		issuedWith <- request.Header.Get("Authorization")
		return reply(http.StatusForbidden, "{}"), nil
	})
	config.Managed.Relays[0].AccessKey = "replacement-device"
	if err := registry.discovery.cache.Save(t.Context(), config.Managed.Relays[0], Capability{ServiceID: "cf-service", Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Configure(config); err != nil {
		t.Fatal(err)
	}
	stop := watchRuntime(registry, t.Context(), config.Managed.Relays)
	defer stop()
	<-started
	selection := manager.Select([]string{config.Managed.Relays[0].PublicURL})
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
