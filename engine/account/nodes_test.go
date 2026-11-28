package account

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
	"github.com/windshare/windshare/engine/internal/statefile"
)

func loadNodeDiscoveryFixture(t *testing.T) (int64, [32]byte, json.RawMessage) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now        int64
		NodeKey    string `json:"node_key"`
		Descriptor json.RawMessage
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	key, err := serviceauthority.PublicKey(fixture.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	return fixture.Now, key, fixture.Descriptor
}

func TestSignedNodeDiscoveryBindsFullWebSocketEndpointWithoutAppendingTwice(t *testing.T) {
	nowUnix, key, rawDescriptor := loadNodeDiscoveryFixture(t)
	config, _ := accountFixture(t)
	config.Now = func() time.Time { return time.Unix(nowUnix, 0) }
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/windshare" || request.Header.Get("Authorization") != "" {
			t.Fatal("discovery leaked authority or used a transfer endpoint", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(rawDescriptor)))}, nil
	})
	service := testService(t, config)
	descriptor, err := service.nodes.discover(t.Context(), "https://node.example", key)
	if err != nil || descriptor.PublicRelay != serviceauthority.RelayAnonymous {
		t.Fatal(descriptor, err)
	}
	if _, err := service.nodes.discover(t.Context(), "https://node.example/another-base", key); err == nil {
		t.Fatal("node moved its signed endpoint")
	}
	key[0] ^= 1
	if _, err := service.nodes.discover(t.Context(), "https://node.example", key); err == nil {
		t.Fatal("directory-pinned node key was ignored")
	}
}

func TestLoggedInShareSkipsNodeDiscovery(t *testing.T) {
	nowUnix, key, rawDescriptor := loadNodeDiscoveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config, initial := accountFixture(t)
		start := time.Now()
		base := time.Unix(nowUnix, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: key}, true
			}
			return Node{}, false
		}
		var discoveries atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				discoveries.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(rawDescriptor)))}, nil
			}
			return accountReply(t, http.StatusOK, initial), nil
		})
		service := testService(t, config)
		share := service.NewShareAuthorization(t.Context())
		defer share.Close()
		source := share.Source("https://node.example")
		auth := source.ReserveRegistrationAuthorization()
		if auth.State != serviceauthority.AdmissionReady || auth.Bearer != initial.Ticket {
			t.Fatalf("auth=%+v, want ready with cached ticket", auth)
		}
		auth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)

		time.Sleep(2 * RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 0 {
			t.Fatalf("logged-in share triggered %d node discoveries, want 0", got)
		}
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("node pollers retained %d entries for logged-in share", pollers)
		}
		if policies := service.nodes.policyCount(); policies != 0 {
			t.Fatalf("node policy cache retained %d entries for logged-in share", policies)
		}
	})
}

func TestNodeDiscoveryStopsOnAttributionCommitAndSuppressesUnchangedBroadcasts(t *testing.T) {
	nowUnix, key, rawDescriptor := loadNodeDiscoveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config, _ := accountFixture(t)
		start := time.Now()
		base := time.Unix(nowUnix, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{}
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: key}, true
			}
			return Node{}, false
		}
		var discoveries atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				discoveries.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(rawDescriptor)))}, nil
			}
			return accountReply(t, http.StatusTooManyRequests, fault{
				Kind: "rate_limited", RetryAfter: maxRateLimitSeconds,
			}), nil
		})
		service := testService(t, config)
		share := service.NewShareAuthorization(t.Context())
		defer share.Close()
		source := share.Source("https://node.example")
		synctest.Wait()
		if got := discoveries.Load(); got != 0 {
			t.Fatalf("Source eagerly triggered %d discoveries before authorization check", got)
		}

		waiting := source.ReserveRegistrationAuthorization()
		if waiting.State != serviceauthority.AdmissionPendingSetup && waiting.State != serviceauthority.AdmissionTemporaryFailure {
			t.Fatalf("initial auth state = %s, want waiting", waiting.State.Name())
		}
		synctest.Wait()
		if got := discoveries.Load(); got != 1 {
			t.Fatalf("initial discoveries=%d, want 1", got)
		}
		select {
		case <-waiting.Changed:
		default:
			t.Fatal("first anonymous policy discovery did not wake waiting share")
		}

		// A steady-state renewal with unchanged effective policy must not broadcast.
		_, unchanged := service.Snapshot()
		time.Sleep(RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("steady-state discoveries=%d, want 2", got)
		}
		select {
		case <-unchanged:
			t.Fatal("unchanged node policy refresh broadcast service.changed")
		default:
		}

		// Once attribution commits, polling stops even while the share remains open.
		auth := source.ReserveRegistrationAuthorization()
		if auth.State != serviceauthority.AdmissionReady || auth.Bearer != "" {
			t.Fatalf("anonymous auth = %+v, want ready without bearer", auth)
		}
		auth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)

		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("node discovery continued after attribution committed: %d requests", got)
		}
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("node pollers retained %d entries after attribution committed", pollers)
		}
		if policies := service.nodes.policyCount(); policies != 1 {
			t.Fatalf("node policy cache retained %d entries after attribution committed, want 1", policies)
		}
	})
}

func TestNodeDiscoveryStopsWhenLastPendingShareCloses(t *testing.T) {
	nowUnix, key, rawDescriptor := loadNodeDiscoveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config, _ := accountFixture(t)
		start := time.Now()
		base := time.Unix(nowUnix, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{}
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: key}, true
			}
			return Node{}, false
		}
		var discoveries atomic.Int32
		allowDiscovery := make(chan struct{})
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				discoveries.Add(1)
				select {
				case <-allowDiscovery:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(rawDescriptor)))}, nil
			}
			return accountReply(t, http.StatusTooManyRequests, fault{
				Kind: "rate_limited", RetryAfter: maxRateLimitSeconds,
			}), nil
		})
		service := testService(t, config)
		first := service.NewShareAuthorization(t.Context())
		second := service.NewShareAuthorization(t.Context())
		firstSource := first.Source("https://node.example")
		secondSource := second.Source("https://node.example")
		firstSource.ReserveRegistrationAuthorization()
		firstSource.ReserveRegistrationAuthorization()
		secondSource.ReserveRegistrationAuthorization()
		close(allowDiscovery)
		synctest.Wait()
		if got := discoveries.Load(); got != 1 {
			t.Fatalf("initial discoveries=%d, want 1", got)
		}

		first.Close()
		time.Sleep(RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("discoveries with second pending share active=%d, want 2", got)
		}

		second.Close()
		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("node discovery continued after last share closed: %d requests", got)
		}
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("node pollers retained %d closed entries", pollers)
		}
		if policies := service.nodes.policyCount(); policies != 1 {
			t.Fatalf("node policy cache retained %d valid entries after share close, want 1", policies)
		}
	})
}

func TestConsecutiveAnonymousSharesReuseValidSignedNodePolicy(t *testing.T) {
	nowUnix, key, rawDescriptor := loadNodeDiscoveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config, _ := accountFixture(t)
		start := time.Now()
		base := time.Unix(nowUnix, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{}
		var activeKey atomic.Value
		activeKey.Store(key)
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: activeKey.Load().([32]byte)}, true
			}
			return Node{}, false
		}
		var discoveries atomic.Int32
		var failDiscovery atomic.Bool
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				discoveries.Add(1)
				if failDiscovery.Load() {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(rawDescriptor)))}, nil
			}
			return accountReply(t, http.StatusTooManyRequests, fault{
				Kind: "rate_limited", RetryAfter: maxRateLimitSeconds,
			}), nil
		})
		service := testService(t, config)

		first := service.NewShareAuthorization(t.Context())
		defer first.Close()
		firstSource := first.Source("https://node.example")
		if got := firstSource.ReserveRegistrationAuthorization(); got.State != serviceauthority.AdmissionPendingSetup && got.State != serviceauthority.AdmissionTemporaryFailure {
			t.Fatalf("first share initial auth = %s, want waiting", got.State.Name())
		}
		synctest.Wait()
		if got := discoveries.Load(); got != 1 {
			t.Fatalf("first share discoveries=%d, want 1", got)
		}
		firstAuth := firstSource.ReserveRegistrationAuthorization()
		if firstAuth.State != serviceauthority.AdmissionReady || firstAuth.Bearer != "" {
			t.Fatalf("first share auth = %+v, want anonymous ready", firstAuth)
		}
		firstAuth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("active pollers after first commit = %d, want 0", pollers)
		}

		// A subsequent anonymous share within the signed descriptor's validity
		// reuses the cached policy immediately even if discovery now fails.
		failDiscovery.Store(true)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		second := service.NewShareAuthorization(t.Context())
		defer second.Close()
		secondSource := second.Source("https://node.example")
		secondAuth := secondSource.ReserveRegistrationAuthorization()
		if secondAuth.State != serviceauthority.AdmissionReady || secondAuth.Bearer != "" {
			t.Fatalf("second share auth = %+v, want immediate anonymous ready from cache", secondAuth)
		}
		secondAuth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)
		if got := discoveries.Load(); got != 1 {
			t.Fatalf("second share triggered discovery: got %d requests, want 1", got)
		}
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("active pollers after cached hit = %d, want 0", pollers)
		}

		// Directory key rotation invalidates the cached policy even before expiry.
		rotatedKey := key
		rotatedKey[0] ^= 1
		activeKey.Store(rotatedKey)
		rotated := service.NewShareAuthorization(t.Context())
		rotatedSource := rotated.Source("https://node.example")
		if got := rotatedSource.ReserveRegistrationAuthorization(); got.State == serviceauthority.AdmissionReady {
			t.Fatalf("share after key rotation reused stale-key cached policy: %+v", got)
		}
		synctest.Wait()
		if got := discoveries.Load(); got != 2 {
			t.Fatalf("rotated key share discoveries=%d, want 2", got)
		}
		if policies := service.nodes.policyCount(); policies != 0 {
			t.Fatalf("stale-key policy remained cached: %d", policies)
		}
		rotated.Close()
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("active pollers after rotated share closed = %d, want 0", pollers)
		}

		// Re-populate with the original key, then advance past the 300s expiry.
		activeKey.Store(key)
		failDiscovery.Store(false)
		refill := service.NewShareAuthorization(t.Context())
		refillSource := refill.Source("https://node.example")
		refillSource.ReserveRegistrationAuthorization()
		synctest.Wait()
		refillAuth := refillSource.ReserveRegistrationAuthorization()
		if refillAuth.State != serviceauthority.AdmissionReady || refillAuth.Bearer != "" {
			t.Fatalf("refill share auth = %+v, want anonymous ready", refillAuth)
		}
		refillAuth.Reservation.Finish(serviceauthority.RegistrationMayHaveBeenSent)
		refill.Close()
		if got := discoveries.Load(); got != 3 {
			t.Fatalf("refill share discoveries=%d, want 3", got)
		}

		failDiscovery.Store(true)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		expired := service.NewShareAuthorization(t.Context())
		defer expired.Close()
		expiredSource := expired.Source("https://node.example")
		if got := expiredSource.ReserveRegistrationAuthorization(); got.State == serviceauthority.AdmissionReady {
			t.Fatalf("share after signature expiry reused expired policy: %+v", got)
		}
		synctest.Wait()
		if got := discoveries.Load(); got != 4 {
			t.Fatalf("expired policy share discoveries=%d, want 4", got)
		}
		if policies := service.nodes.policyCount(); policies != 0 {
			t.Fatalf("expired policy remained cached: %d", policies)
		}
	})
}

func TestFailedNodeDiscoveryLeavesNoCachedPolicyOrPoller(t *testing.T) {
	nowUnix, key, _ := loadNodeDiscoveryFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config, _ := accountFixture(t)
		start := time.Now()
		base := time.Unix(nowUnix, 0)
		config.Now = func() time.Time { return base.Add(time.Since(start)) }
		config.Store = &MemoryStore{}
		config.ResolveNode = func(address string) (Node, bool) {
			if address == "https://node.example" {
				return Node{Key: key}, true
			}
			return Node{}, false
		}
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/.well-known/windshare" {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			return accountReply(t, http.StatusTooManyRequests, fault{
				Kind: "rate_limited", RetryAfter: maxRateLimitSeconds,
			}), nil
		})
		service := testService(t, config)
		share := service.NewShareAuthorization(t.Context())
		source := share.Source("https://node.example")
		source.ReserveRegistrationAuthorization()
		synctest.Wait()
		share.Close()
		if pollers := service.nodes.pollerCount(); pollers != 0 {
			t.Fatalf("active pollers after failed share closed = %d, want 0", pollers)
		}
		if policies := service.nodes.policyCount(); policies != 0 {
			t.Fatalf("policy cache after failed share closed = %d, want 0", policies)
		}
	})
}

func TestFileStoreLoadIsLockFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	store := FileStore{Path: path}
	_, initial := accountFixture(t)
	saved := cachedRecord(initial)
	if _, err := store.Update(t.Context(), func(Record) (Record, error) { return saved, nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".lock"); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(t.Context())
	if err != nil || loaded.Ticket != saved.Ticket {
		t.Fatalf("load=%+v err=%v", loaded, err)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock-free load created lock file: err=%v", err)
	}

	// Load must not block even while another writer holds the exclusive lock.
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- statefile.WithLock(t.Context(), path, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	loaded, err = store.Load(t.Context())
	close(release)
	if lockErr := <-done; lockErr != nil {
		t.Fatal(lockErr)
	}
	if err != nil || loaded.Ticket != saved.Ticket {
		t.Fatalf("load under active write lock=%+v err=%v", loaded, err)
	}
}
