package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func (store *interruptedSelectionStore) setError(err error) {
	store.mu.Lock()
	store.err = err
	store.mu.Unlock()
}

func (store *interruptedSelectionStore) currentError() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.err
}

func (store *interruptedSelectionStore) Load(ctx context.Context) (Record, error) {
	if err := store.currentError(); err != nil {
		return Record{}, err
	}
	return store.MemoryStore.Load(ctx)
}

func (store *interruptedSelectionStore) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	if err := store.currentError(); err != nil {
		return Record{}, err
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

func TestCorruptOrUnreadableAccountStoreRequiresActionWithoutAnonymousFallback(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		setup func(*testing.T, string, Config, session)
	}{
		{
			name: "invalid json",
			setup: func(t *testing.T, path string, _ Config, _ session) {
				if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized file",
			setup: func(t *testing.T, path string, _ Config, _ session) {
				if err := os.WriteFile(path, []byte(strings.Repeat(" ", 17<<10)), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "directory instead of file",
			setup: func(t *testing.T, path string, _ Config, _ session) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "wrong issuer",
			setup: func(t *testing.T, path string, _ Config, _ session) {
				if err := os.WriteFile(path, []byte(`{"issuer":"another-issuer"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid certificate",
			setup: func(t *testing.T, path string, _ Config, _ session) {
				if err := os.WriteFile(path, []byte(`{"certificate":"not-a-generation"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			config, issued := accountFixture(t)
			path := filepath.Join(t.TempDir(), "account.json")
			scenario.setup(t, path, config, issued)
			config.Store = FileStore{Path: path}

			service := testService(t, config)
			service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
			if snapshot, _ := service.Snapshot(); snapshot.State != StorageError || snapshot.Reason == "" {
				t.Fatalf("startup snapshot = %+v, want storage_error", snapshot)
			}

			share := service.NewShareAuthorization(t.Context())
			defer share.Close()
			source := &Source{share: share, address: "primary"}
			got := source.ReserveRegistrationAuthorization()
			if got.State != serviceauthority.AdmissionUserActionRequired || got.Bearer != "" || got.Changed == nil || !isLocalStoreError(got.Cause) {
				t.Fatalf("corrupt store authorization = %+v, want user_action_required without anonymous fallback", got)
			}
			if code := CommandErrorCode(got.Cause); code != "account_storage_error" {
				t.Fatalf("CommandErrorCode = %q, want account_storage_error", code)
			}

			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) {
				return cachedRecord(issued), nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.ReloadSelection(t.Context()); err != nil {
				t.Fatalf("reload after repair failed: %v", err)
			}
			select {
			case <-got.Changed:
			default:
				t.Fatal("repairing account store did not wake waiting share")
			}
			if snapshot, _ := service.Snapshot(); snapshot.State != Ready {
				t.Fatalf("recovered snapshot = %+v, want ready", snapshot)
			}
			requireBearer(t, source, issued.Ticket)
		})
	}
}

func TestCorruptingAccountStoreAfterHealthyLoadRecoversWhenRestored(t *testing.T) {
	config, issued := accountFixture(t)
	path := filepath.Join(t.TempDir(), "account.json")
	config.Store = FileStore{Path: path}
	if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) {
		return cachedRecord(issued), nil
	}); err != nil {
		t.Fatal(err)
	}
	validBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	service := testService(t, config)
	service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "primary"}
	waiting := source.ReserveRegistrationAuthorization()
	if waiting.State != serviceauthority.AdmissionUserActionRequired || waiting.Bearer != "" {
		t.Fatalf("corrupted store after healthy load = %+v, want user_action_required", waiting)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != StorageError {
		t.Fatalf("snapshot = %+v, want storage_error", snapshot)
	}

	// Restoring the exact same account must still clear StorageError and resume.
	if err := os.WriteFile(path, validBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := service.ReloadSelection(t.Context()); err != nil {
		t.Fatalf("reload after restoring identical record failed: %v", err)
	}
	select {
	case <-waiting.Changed:
	default:
		t.Fatal("restoring identical account record did not wake waiting share")
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != Ready {
		t.Fatalf("snapshot after restoring identical record = %+v, want ready", snapshot)
	}
	requireBearer(t, source, issued.Ticket)
}

func TestPersistentStoreReadErrorEscalatesToUserActionAndRecovers(t *testing.T) {
	config, issued := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		issuedAt, start := config.Now(), time.Now()
		config.Now = func() time.Time { return issuedAt.Add(time.Since(start)) }
		store := &interruptedSelectionStore{err: errors.New("persistent disk i/o error")}
		store.record = cachedRecord(issued)
		config.Store = store

		service := testService(t, config)
		service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
		share := service.NewShareAuthorization(t.Context())
		defer share.Close()
		source := share.Source("primary")
		synctest.Wait()

		first := source.ReserveRegistrationAuthorization()
		if first.State != serviceauthority.AdmissionTemporaryFailure || first.Bearer != "" {
			t.Fatalf("first read failure = %+v, want temporary failure", first)
		}

		time.Sleep(MinimumRetry)
		synctest.Wait()
		select {
		case <-first.Changed:
		default:
			t.Fatal("escalation to storage_error did not wake waiting share")
		}
		escalated := source.ReserveRegistrationAuthorization()
		if escalated.State != serviceauthority.AdmissionUserActionRequired || escalated.Bearer != "" {
			t.Fatalf("persistent read failure = %+v, want user_action_required", escalated)
		}
		if snapshot, _ := service.Snapshot(); snapshot.State != StorageError {
			t.Fatalf("snapshot = %+v, want storage_error", snapshot)
		}

		// Repeated polls on the same broken store must not spuriously wake waiters.
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case <-escalated.Changed:
			t.Fatal("unchanged storage_error spuriously woke waiters")
		default:
		}

		store.setError(nil)
		time.Sleep(PollInterval)
		synctest.Wait()
		select {
		case <-escalated.Changed:
		default:
			t.Fatal("store recovery did not wake waiting share")
		}
		if snapshot, _ := service.Snapshot(); snapshot.State != Ready {
			t.Fatalf("recovered snapshot = %+v, want ready", snapshot)
		}
		requireBearer(t, source, issued.Ticket)
	})
}

type blockingLoadStore struct {
	MemoryStore
	block   atomic.Bool
	entered chan struct{}
	once    sync.Once
}

func (store *blockingLoadStore) Load(ctx context.Context) (Record, error) {
	if store.block.Load() {
		store.once.Do(func() { close(store.entered) })
		<-ctx.Done()
		return Record{}, ctx.Err()
	}
	return store.MemoryStore.Load(ctx)
}

func TestPrepareShareAuthorizationAfterCloseFailsClosed(t *testing.T) {
	config, _ := accountFixture(t)
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
	service.Close()

	share := service.PrepareShareAuthorization(t.Context())
	if share == nil {
		t.Fatal("PrepareShareAuthorization on closed service returned nil")
	}
	defer share.Close()
	if share.loading != nil || share.attribution != attributionClosed {
		t.Fatalf("closed PrepareShareAuthorization state = (loading=%v, attribution=%d), want closed without loading",
			share.loading != nil, share.attribution)
	}
	got := share.Source("primary").ReserveRegistrationAuthorization()
	if got.State != serviceauthority.AdmissionDenied || got.Bearer != "" {
		t.Fatalf("authorization after service close = %+v, want denied", got)
	}
}

func TestPrepareShareAuthorizationCloseCancelsInFlightLoadImmediately(t *testing.T) {
	config, _ := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		store := &blockingLoadStore{entered: make(chan struct{})}
		config.Store = store
		service, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		defer service.Close()
		service.nodes.put("primary", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})

		store.block.Store(true)
		share := service.PrepareShareAuthorization(context.Background())
		defer share.Close()
		<-store.entered

		source := share.Source("primary")
		waiting := source.ReserveRegistrationAuthorization()
		if waiting.State != serviceauthority.AdmissionPendingSetup || waiting.Changed == nil {
			t.Fatalf("in-flight PrepareShareAuthorization = %+v, want pending_setup with signal", waiting)
		}

		closed := make(chan struct{})
		go func() {
			service.Close()
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("Service.Close waited for CacheLoadTimeout instead of canceling PrepareShareAuthorization load")
		}
		select {
		case <-waiting.Changed:
		default:
			t.Fatal("Service.Close did not unblock waiters on share.loading")
		}
		got := source.ReserveRegistrationAuthorization()
		if got.State != serviceauthority.AdmissionDenied || got.Bearer != "" {
			t.Fatalf("authorization after canceled PrepareShareAuthorization = %+v, want denied", got)
		}
	})
}

func TestPrepareShareAuthorizationConcurrentWithServiceClose(t *testing.T) {
	config, issued := accountFixture(t)
	for range 64 {
		iterationConfig := config
		iterationConfig.Store = &MemoryStore{record: cachedRecord(issued)}
		service, err := New(iterationConfig)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Go(func() {
			<-start
			share := service.PrepareShareAuthorization(t.Context())
			defer share.Close()
			source := share.Source("primary")
			got := source.ReserveRegistrationAuthorization()
			if got.State == serviceauthority.AdmissionPendingSetup && got.Changed != nil {
				<-got.Changed
				got = source.ReserveRegistrationAuthorization()
			}
			switch got.State {
			case serviceauthority.AdmissionReady:
				if got.Bearer != issued.Ticket {
					t.Errorf("ready bearer = %q, want issued ticket", got.Bearer)
				}
			case serviceauthority.AdmissionDenied:
			default:
				t.Errorf("unexpected authorization state during concurrent close: %+v", got)
			}
		})
		group.Go(func() {
			<-start
			service.Close()
		})
		close(start)
		group.Wait()
	}
}
