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
		first.Start()
		second.Start()
		synctest.Wait()
		if got := requests.Load(); got != 0 {
			t.Fatalf("startup issued %d tickets with a fresh cache", got)
		}

		first.Retry()
		synctest.Wait()
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

		second.Retry()
		synctest.Wait()
		snapshot, _ = second.Snapshot()
		if got := requests.Load(); got != 3 || snapshot.State != Ready || snapshot.Profile != profile {
			t.Fatalf("manual recovery: requests=%d state=%s profile=%+v", got, snapshot.State, snapshot.Profile)
		}
	})
}
