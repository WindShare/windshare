package receive_test

import (
	"context"
	"crypto/rand"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type memoryStopStore struct {
	mu     sync.Mutex
	values []v2route.Tombstone
}

func (store *memoryStopStore) Lookup(_ context.Context, shareID v2.ShareID) (v2route.Tombstone, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, record := range store.values {
		if record.ShareID == shareID {
			return record, true, nil
		}
	}
	return v2route.Tombstone{}, false, nil
}

func (store *memoryStopStore) Commit(
	_ context.Context,
	value v2route.Tombstone,
) (v2route.CommitOutcome, error) {
	store.mu.Lock()
	store.values = append(store.values, value)
	store.mu.Unlock()
	return v2route.CommitCommitted, nil
}

func (store *memoryStopStore) Count() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return len(store.values)
}

func newReceiveRelayServer(t *testing.T, store *memoryStopStore) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	endpointIdentity, err := v2.NormalizeRelayEndpoint("http://" + server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 8, MaxSessions: 8, MaxSessionsPerShare: 4, Random: rand.Reader, Tombstones: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 16, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := v2endpoint.New(v2endpoint.Config{
		Registry: registry, Challenges: challenges, RelayIdentity: endpointIdentity.Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = httpapi.NewV2Handler(httpapi.V2Config{
		Server: endpoint, AllowLocalhost: true,
		AdmitConnection: func(string) (func(), bool) { return func() {}, true },
	})
	server.Start()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = endpoint.Shutdown(shutdown)
		cancel()
		server.Close()
	})

	return server
}
