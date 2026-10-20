package turnservice

import (
	"net/http"
	"testing"

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
