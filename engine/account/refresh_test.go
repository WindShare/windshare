package account

import (
	"encoding/json"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestOpeningExistingAccountRenewsExpiredTicketAndPreservesBackoff(t *testing.T) {
	config, initial := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		release := make(chan struct{})
		var requests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts/ticket" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			requests.Add(1)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			return accountReply(t, http.StatusTooManyRequests, fault{
				Kind: "rate_limited", RetryAfter: uint64(RefreshInterval.Seconds()),
			}), nil
		})

		idle := testService(t, config)
		time.Sleep(time.Unix(int64(initial.ExpiresAt), 0).Sub(issuedAt))
		service := testService(t, config)
		for _, candidate := range []*Service{idle, service} {
			snapshot, _ := candidate.Snapshot()
			if snapshot.State != RefreshRequired || snapshot.Active || snapshot.Account == "" {
				t.Fatalf("expired cached ticket should be waiting for renewal: %+v", snapshot)
			}
		}
		if got := requests.Load(); got != 0 {
			t.Fatalf("reading expired account sent %d requests", got)
		}

		closeView := service.ObserveAccount()
		synctest.Wait()
		snapshot, _ := service.Snapshot()
		if got := requests.Load(); got != 1 || !snapshot.Active || snapshot.State != Refreshing {
			t.Fatalf("renewal in flight: requests=%d snapshot=%+v", got, snapshot)
		}

		close(release)
		synctest.Wait()
		snapshot, _ = service.Snapshot()
		if snapshot.State != Unavailable || !snapshot.RetryAt.After(config.Now()) {
			t.Fatalf("failed renewal did not publish a retry deadline: %+v", snapshot)
		}

		closeView()
		if snapshot, _ := service.Snapshot(); snapshot.Active {
			t.Fatal("closed view still owns renewal")
		}
		defer service.ObserveAccount()()
		time.Sleep(PollInterval)
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("reopening bypassed refresh backoff: %d requests", got)
		}
	})
}

func TestRenewalDoesNotSetUpAnAccountWhenTheStoreIsCleared(t *testing.T) {
	config, initial := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		var requests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			requests.Add(1)
			return accountReply(t, http.StatusServiceUnavailable, fault{}), nil
		})
		service := testService(t, config)
		defer service.ObserveAccount()()
		synctest.Wait()
		if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) { return Record{}, nil }); err != nil {
			t.Fatal(err)
		}
		time.Sleep(PollInterval)
		synctest.Wait()
		snapshot, _ := service.Snapshot()
		if got := requests.Load(); got != 0 || snapshot.State != Pending || snapshot.Account != "" {
			t.Fatalf("renewal should only observe an empty store: requests=%d snapshot=%+v", got, snapshot)
		}
		share := service.NewShareAuthorization(t.Context())
		defer share.Close()
		share.beginOfficialUse()
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("official use did not wake the existing worker for setup: %d requests", got)
		}
	})
}

func TestOfficialRenewalStopsWithLastShareOrView(t *testing.T) {
	config, initial := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		config.ResolveNode = nil
		var requests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts/ticket" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			requests.Add(1)
			return accountReply(t, http.StatusOK, initial), nil
		})

		service := testService(t, config)
		first, second := service.NewShareAuthorization(t.Context()), service.NewShareAuthorization(t.Context())
		first.Source("https://first.example")
		first.Source("https://second.example")
		second.Source("https://second.example")
		closeView := service.ObserveAccount()
		synctest.Wait()
		if got := requests.Load(); got != 0 {
			t.Fatalf("opening with a fresh cache issued %d tickets", got)
		}

		first.Close()
		time.Sleep(RefreshInterval)
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("remaining share and view issued %d tickets", got)
		}
		closeView()
		closeView()
		// With only a background share remaining, client renewal waits until the
		// ticket approaches expiry instead of polling every minute.
		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("background share polled %d times before ticket renewal lead", got)
		}
		renewalDelay := time.Unix(int64(initial.ExpiresAt), 0).Add(-TicketRenewalLead).Sub(config.Now())
		time.Sleep(renewalDelay)
		synctest.Wait()
		if got := requests.Load(); got != 2 {
			t.Fatalf("background share did not renew ahead of ticket expiry: %d tickets", got)
		}
		second.Close()
		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		snapshot, _ := service.Snapshot()
		if got := requests.Load(); got != 2 || snapshot.Active || snapshot.Account == "" {
			t.Fatalf("idle service: %d tickets, active=%t, account=%q", got, snapshot.Active, snapshot.Account)
		}
		defer service.ObserveAccount()()
		synctest.Wait()
		if got := requests.Load(); got != 3 {
			t.Fatalf("reopening after the refresh deadline issued %d tickets", got)
		}
	})
}

func TestLoginAndExplicitRefreshDoNotStartMaintenance(t *testing.T) {
	config, session := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		var ticketRequests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path == "/v1/accounts/ticket" {
				ticketRequests.Add(1)
			}
			return accountReply(t, http.StatusOK, session), nil
		})

		service := testService(t, config)
		if err := service.Login(t.Context(), "alice", "secret"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * RefreshInterval)
		synctest.Wait()
		if got := ticketRequests.Load(); got != 0 {
			t.Fatalf("idle login triggered %d renewals", got)
		}
		if outcome, err := service.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
			t.Fatalf("explicit renewal did not complete: %s, %v", outcome, err)
		}
		time.Sleep(2 * RefreshInterval)
		synctest.Wait()
		if got := ticketRequests.Load(); got != 1 {
			t.Fatalf("explicit renewal triggered %d requests", got)
		}
	})
}

func TestSharedTicketSatisfiesBackgroundRefresh(t *testing.T) {
	config, initial := accountFixture(t)
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct{ Refreshed session }
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		var requests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts/ticket" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			if requests.Add(1) == 2 {
				return accountReply(t, http.StatusTooManyRequests, fault{
					Kind: "rate_limited", RetryAfter: uint64(RefreshInterval.Seconds()),
				}), nil
			}
			return accountReply(t, http.StatusOK, vector.Refreshed), nil
		})

		first, second := testService(t, config), testService(t, config)
		defer first.ObserveAccount()()
		defer second.ObserveAccount()()
		synctest.Wait()
		if got := requests.Load(); got != 0 {
			t.Fatalf("startup issued %d tickets with a fresh cache", got)
		}

		if snapshot, _ := first.Snapshot(); snapshot.State != Ready {
			t.Fatalf("manual refresh should start with a usable cache: %+v", snapshot)
		}
		if outcome, err := first.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
			t.Fatalf("manual refresh did not complete: %s, %v", outcome, err)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("manual refresh returned after %d authority requests", got)
		}
		if stored, err := first.load(t.Context()); err != nil || stored.Ticket != vector.Refreshed.Ticket {
			t.Fatalf("manual refresh returned before storing the issued ticket: %v", err)
		}
		time.Sleep(PollInterval)
		synctest.Wait()
		snapshot, _ := second.Snapshot()
		if snapshot.State != Ready || snapshot.Profile != vector.Refreshed.Profile ||
			!snapshot.RetryAt.Equal(issuedAt.Add(RefreshInterval)) {
			t.Fatalf("second service did not adopt the signed refresh: %+v", snapshot)
		}
		if got := requests.Load(); got != 1 {
			t.Fatalf("one manual refresh produced %d requests", got)
		}

		first.Close()
		time.Sleep(RefreshInterval - PollInterval)
		synctest.Wait()
		snapshot, _ = second.Snapshot()
		if got := requests.Load(); got != 2 || snapshot.State != Unavailable {
			t.Fatalf("refresh at the signed deadline: requests=%d state=%s", got, snapshot.State)
		}

		profile := snapshot.Profile
		profile.Revision++
		profile.Nickname.Value = "New nickname"
		if _, err := config.Store.Update(t.Context(), func(current Record) (Record, error) {
			current.Profile = profile
			return current, nil
		}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(PollInterval)
		synctest.Wait()
		snapshot, _ = second.Snapshot()
		if got := requests.Load(); got != 2 || snapshot.Profile != profile || snapshot.State != Unavailable {
			t.Fatalf("profile edit during backoff: requests=%d state=%s profile=%+v", got, snapshot.State, snapshot.Profile)
		}

		if outcome, err := second.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
			t.Fatalf("manual recovery did not complete: %s, %v", outcome, err)
		}
		synctest.Wait()
		snapshot, _ = second.Snapshot()
		if got := requests.Load(); got != 3 || snapshot.State != Ready || snapshot.Profile != profile {
			t.Fatalf("manual recovery: requests=%d state=%s profile=%+v", got, snapshot.State, snapshot.Profile)
		}
	})
}

func TestOpeningViewDuringIdleShareRefreshesStaleAllowanceImmediately(t *testing.T) {
	config, initial := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		config.Store = &MemoryStore{record: cachedRecord(initial)}
		config.ResolveNode = nil
		var requests atomic.Int32
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts/ticket" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			requests.Add(1)
			return accountReply(t, http.StatusOK, initial), nil
		})

		service := testService(t, config)
		share := service.NewShareAuthorization(t.Context())
		defer share.Close()
		share.Source("https://node.example")
		synctest.Wait()

		// Two hours of idle sharing needs no client ticket renewal.
		time.Sleep(2 * time.Hour)
		synctest.Wait()
		if got := requests.Load(); got != 0 {
			t.Fatalf("idle background share issued %d tickets in 2h", got)
		}

		// Opening the account view immediately fetches a fresh allowance snapshot.
		closeView := service.ObserveAccount()
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("opening account view during idle share issued %d tickets", got)
		}
		closeView()

		// Closing the view returns to ticket-expiry cadence without extra minute polls.
		time.Sleep(3 * RefreshInterval)
		synctest.Wait()
		if got := requests.Load(); got != 1 {
			t.Fatalf("closing account view left background share polling: %d tickets", got)
		}
	})
}
