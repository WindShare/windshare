package turnpath

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
)

func TestTransportWatcherIncludesFinalTrafficAfterAccountStops(t *testing.T) {
	credential, _ := iceconfig.Fixed("user", "password")
	relay, _ := iceconfig.NewRelay(iceconfig.Identity{Provider: "test", Account: "owner", Profile: "primary"}, []string{"turn:localhost"}, credential)
	authority, revoke := context.WithCancelCause(context.Background())
	defer revoke(nil)
	var sent atomic.Uint64
	var charged atomic.Uint64
	lease := NewLease(LeaseConfig{Relay: relay, Authority: authority, Charge: func(usage Usage) bool {
		charged.Add(usage.TransportSent)
		return false
	}})
	stopped := make(chan error, 1)
	join := WatchTransport(context.Background(), lease, func() Usage { return Usage{TransportSent: sent.Load()} },
		func(err error) { stopped <- err })
	sent.Store(10)
	revoke(ErrBudget)
	select {
	case err := <-stopped:
		if !errors.Is(err, ErrAccountUnavailable) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("account shutdown did not stop transport")
	}
	// Closing the native connection can write after the watcher observes Lost.
	sent.Store(15)
	join()
	if charged.Load() != 15 {
		t.Fatalf("final transport usage = %d", charged.Load())
	}
	lease.Close()
}
