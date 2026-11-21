package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestShareBindsLoginFromSharedStoreBeforeDesktopRefresh(t *testing.T) {
	config, first := accountFixture(t)
	second := otherAccount(t, config, first)
	config.Store = FileStore{Path: filepath.Join(t.TempDir(), "shared.account.json")}
	if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) {
		return cachedRecord(first), nil
	}); err != nil {
		t.Fatal(err)
	}

	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("desktop authorization called the authority: %s", request.URL.Path)
		return nil, context.Canceled
	})
	desktop := testService(t, config)
	oldShare := desktop.NewShareAuthorization(t.Context())
	defer oldShare.Close()
	requireBearer(t, &Source{share: oldShare, address: "primary"}, first.Ticket)

	payload, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	cliConfig := config
	cliConfig.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/accounts/login" {
			return nil, context.Canceled
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(payload)))}, nil
	})
	cli := testService(t, cliConfig)
	if err := cli.Login(t.Context(), "second-account", "fixture-password"); err != nil {
		t.Fatal(err)
	}
	cli.Close()

	// A remote desktop account operation may still be in progress when the
	// share is created. Capturing the stored selection must remain independent.
	if err := desktop.lockOperation(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { <-desktop.operation }()
	newShare := desktop.NewShareAuthorization(t.Context())
	defer newShare.Close()

	for _, address := range []string{"primary", "backup"} {
		requireBearer(t, &Source{share: newShare, address: address}, second.Ticket)
		requireBearer(t, &Source{share: oldShare, address: address}, first.Ticket)
	}
}

type interruptedSelectionStore struct {
	MemoryStore
	err error
}

func (store *interruptedSelectionStore) Load(ctx context.Context) (Record, error) {
	if store.err != nil {
		return Record{}, store.err
	}
	return store.MemoryStore.Load(ctx)
}

func (store *interruptedSelectionStore) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	if store.err != nil {
		return Record{}, store.err
	}
	return store.MemoryStore.Update(ctx, change)
}

func TestShareWaitsForConfirmedSelectionAfterLocalReadFailure(t *testing.T) {
	config, first := accountFixture(t)
	second := otherAccount(t, config, first)
	store := &interruptedSelectionStore{}
	store.record = cachedRecord(first)
	config.Store = store
	service := testService(t, config)
	service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})

	failure := errors.New("could not open shared account state")
	store.err = failure
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "primary"}
	waiting := source.ReserveRegistrationAuthorization()
	if waiting.State != serviceauthority.AdmissionTemporaryFailure || waiting.Bearer != "" || !errors.Is(waiting.Cause, failure) {
		t.Fatalf("share did not wait for a confirmed identity: %+v", waiting)
	}

	store.err = nil
	if _, err := store.Update(t.Context(), func(Record) (Record, error) {
		return cachedRecord(second), nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.syncRecord(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waiting.Changed:
	default:
		t.Fatal("successful synchronization did not wake the waiting share")
	}
	requireBearer(t, source, second.Ticket)
}

func TestLoggedInShareWaitsForTicketRenewalBeforeAnonymousFallback(t *testing.T) {
	t.Run("renewal succeeds", func(t *testing.T) {
		config, initial := accountFixture(t)
		synctest.Test(t, func(t *testing.T) {
			issuedAt, start := config.Now(), time.Now()
			config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
			expired := cachedRecord(initial)
			expired.Ticket = ""
			config.Store = &MemoryStore{record: expired}
			started := make(chan struct{})
			release := make(chan struct{})
			config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/accounts/ticket" {
					t.Errorf("unexpected route %s", request.URL.Path)
				}
				close(started)
				<-release
				return accountReply(t, http.StatusOK, initial), nil
			})

			service := testService(t, config)
			service.nodes.put("global", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
			share := service.NewShareAuthorization(t.Context())
			defer share.Close()
			share.beginOfficialUse()
			<-started
			synctest.Wait()

			global := &Source{share: share, address: "global"}
			mainland := &Source{share: share, address: "mainland"}
			waiting := global.ReserveRegistrationAuthorization()
			if waiting.State != serviceauthority.AdmissionExpired || waiting.Bearer != "" {
				t.Fatalf("logged-in share downgraded while renewing: %+v", waiting)
			}

			close(release)
			synctest.Wait()
			select {
			case <-waiting.Changed:
			default:
				t.Fatal("ticket renewal did not wake waiting share")
			}
			requireBearer(t, global, initial.Ticket)
			requireBearer(t, mainland, initial.Ticket)
		})
	})

	t.Run("renewal failure falls back to anonymous", func(t *testing.T) {
		config, initial := accountFixture(t)
		synctest.Test(t, func(t *testing.T) {
			issuedAt, start := config.Now(), time.Now()
			config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
			expired := cachedRecord(initial)
			expired.Ticket = ""
			config.Store = &MemoryStore{record: expired}
			release := make(chan struct{})
			var attempts atomic.Int32
			config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/accounts/ticket" {
					t.Errorf("unexpected route %s", request.URL.Path)
				}
				if attempts.Add(1) == 1 {
					<-release
					return accountReply(t, http.StatusServiceUnavailable, fault{}), nil
				}
				return accountReply(t, http.StatusOK, initial), nil
			})

			service := testService(t, config)
			service.nodes.put("global", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
			first := service.NewShareAuthorization(t.Context())
			first.beginOfficialUse()
			synctest.Wait()

			firstGlobal := &Source{share: first, address: "global"}
			waiting := firstGlobal.ReserveRegistrationAuthorization()
			if waiting.State != serviceauthority.AdmissionExpired {
				t.Fatalf("state while refreshing = %s, want expired", waiting.State.Name())
			}

			close(release)
			synctest.Wait()
			select {
			case <-waiting.Changed:
			default:
				t.Fatal("renewal failure did not wake waiting share")
			}
			requireBearer(t, firstGlobal, "")
			first.Close()

			// Once retry backoff elapses, a new share must retry the authority
			// instead of acting on an expired Unavailable state.
			time.Sleep(2 * MinimumRetry)
			synctest.Wait()
			second := service.NewShareAuthorization(t.Context())
			defer second.Close()
			secondGlobal := &Source{share: second, address: "global"}
			if got := secondGlobal.ReserveRegistrationAuthorization(); got.State != serviceauthority.AdmissionTemporaryFailure || got.Bearer != "" {
				t.Fatalf("stale unavailable state downgraded new share: %+v", got)
			}
			second.beginOfficialUse()
			synctest.Wait()
			requireBearer(t, secondGlobal, initial.Ticket)
		})
	})

	t.Run("retained credential waits before anonymous fallback", func(t *testing.T) {
		config, initial := accountFixture(t)
		synctest.Test(t, func(t *testing.T) {
			issuedAt, start := config.Now(), time.Now()
			config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
			expired := cachedRecord(initial)
			expired.Ticket = ""
			config.Store = &MemoryStore{record: expired}
			config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
				return accountReply(t, http.StatusServiceUnavailable, fault{}), nil
			})

			service := testService(t, config)
			share := service.NewShareAuthorization(t.Context())
			defer share.Close()
			service.mu.Lock()
			service.adoptLocked(Record{SetupSecret: "new-selection"})
			service.mu.Unlock()
			service.nodes.put("global", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})

			global := &Source{share: share, address: "global"}
			waiting := global.ReserveRegistrationAuthorization()
			if waiting.State != serviceauthority.AdmissionExpired {
				t.Fatalf("retained credential downgraded before retry: %+v", waiting)
			}
			synctest.Wait()
			time.Sleep(MinimumRetry)
			synctest.Wait()
			select {
			case <-waiting.Changed:
			default:
				t.Fatal("retained refresh backoff expiry did not wake waiting share")
			}
			requireBearer(t, global, "")
		})
	})
}
