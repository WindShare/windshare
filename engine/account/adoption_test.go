package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestReloadSelectionAdoptsSharedLogin(t *testing.T) {
	config, issued := accountFixture(t)
	config.Store = FileStore{Path: filepath.Join(t.TempDir(), "account.json")}
	_, err := config.Store.Update(t.Context(), func(Record) (Record, error) {
		return Record{
			SetupSecret: issued.Secret,
			Proof: &Proof{Challenge: Challenge{
				ID: "email-code", Mode: emailChallenge, ExpiresAt: uint64(config.Now().Add(time.Minute).Unix()),
			}},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		t.Errorf("local account selection contacted the authority: %s", request.URL.Path)
		return nil, context.Canceled
	})
	desktop := testService(t, config)
	stale, changed := desktop.Snapshot()
	if stale.State != EmailRequired || !stale.EmailCodePending || stale.Account != "" {
		t.Fatalf("expected a pending email on desktop: %+v", stale)
	}

	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/accounts/login" {
			t.Errorf("unexpected CLI authority request: %s", request.URL.Path)
		}
		return accountReply(t, http.StatusOK, issued), nil
	})
	cli := testService(t, config)
	if err := cli.Login(t.Context(), "alice", "fixture-password"); err != nil {
		t.Fatal(err)
	}
	if before, _ := desktop.Snapshot(); before != stale {
		t.Fatalf("idle desktop tracked an external login before reloading: %+v", before)
	}

	if err := desktop.ReloadSelection(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("the new selection was not published to observers")
	}
	snapshot, _ := desktop.Snapshot()
	selected, _ := cli.Snapshot()
	if snapshot.Account == "" || snapshot.Account != selected.Account || snapshot.State != Ready ||
		snapshot.EmailCodePending || snapshot.Active {
		t.Fatalf("the local login should replace email setup without starting renewal: %+v", snapshot)
	}
}

func TestReloadSelectionDiscardsOldRegistrationFailure(t *testing.T) {
	config, issued := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		reply := make(chan struct{})
		oldFailure := errors.New("registration request failed")
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts" {
				t.Errorf("unexpected desktop authority request: %s", request.URL.Path)
			}
			select {
			case <-reply:
				return nil, oldFailure
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		})
		desktop := testService(t, config)
		config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
			return accountReply(t, http.StatusOK, issued), nil
		})
		cli := testService(t, config)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- desktop.Register(ctx) }()
		synctest.Wait()
		if snapshot, _ := desktop.Snapshot(); snapshot.State != Registering {
			t.Fatalf("registration should still be in flight: %+v", snapshot)
		}
		if err := cli.Login(t.Context(), "alice", "fixture-password"); err != nil {
			t.Fatal(err)
		}
		if err := desktop.ReloadSelection(t.Context()); err != nil {
			t.Fatalf("local reload waited for registration: %v", err)
		}
		close(reply)
		if err := <-result; !errors.Is(err, oldFailure) {
			t.Fatalf("registration outcome: %v", err)
		}
		if snapshot, _ := desktop.Snapshot(); snapshot.State != Ready || snapshot.Account == "" || snapshot.RegistrationPermille != 0 {
			t.Fatalf("obsolete registration replaced the selected account state: %+v", snapshot)
		}
	})
}

type rejectingDenialStore struct{ record Record }

func (store *rejectingDenialStore) Load(context.Context) (Record, error) {
	return cloneRecord(store.record), nil
}

func (store *rejectingDenialStore) Update(_ context.Context, apply func(Record) (Record, error)) (Record, error) {
	next, err := apply(cloneRecord(store.record))
	if err != nil {
		return Record{}, err
	}
	if next.Denial != "" {
		return Record{}, errors.New("disk full")
	}
	store.record = cloneRecord(next)
	return next, nil
}

func TestDenialSurvivesFailedWriteAndCacheReload(t *testing.T) {
	config, response := accountFixture(t)
	cached := cachedRecord(response)
	config.Store = &rejectingDenialStore{record: cached}
	config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"kind":"denied","reason":"account_blocked"}`))}, nil
	})
	service := testService(t, config)
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "node"}
	requireBearer(t, source, response.Ticket)
	if _, err := service.RefreshOnce(t.Context()); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("failed persistence: %v", err)
	}
	service.mu.Lock()
	service.adoptLocked(cached)
	service.nodes.put("node", &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)})
	service.mu.Unlock()
	newShare := service.NewShareAuthorization(t.Context())
	defer newShare.Close()
	for _, source := range []*Source{source, {share: newShare, address: "node"}} {
		if got := source.ReserveRegistrationAuthorization(); got.State != serviceauthority.AdmissionDenied || got.Bearer != "" {
			t.Fatalf("denial lost: %+v", got)
		}
	}
}

func TestRefreshOnceReportsDenialWithCachedSetupChallenge(t *testing.T) {
	config, response := accountFixture(t)
	config.Store = &MemoryStore{record: Record{
		SetupSecret: response.Secret,
		Denial:      "account_blocked",
		Proof:       &Proof{Challenge: Challenge{Mode: emailChallenge, ExpiresAt: uint64(config.Now().Add(time.Minute).Unix())}},
	}}
	config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("live setup challenge should be evaluated from the store")
		return nil, nil
	})
	service := testService(t, config)
	if outcome, err := service.RefreshOnce(t.Context()); outcome != "" || err == nil || CommandErrorCode(err) != "account_denied" {
		t.Fatalf("known denial returned a setup outcome: %s, %v", outcome, err)
	}
}

func TestAdoptionUpdatesRetainedCredentialBeforeLoginChanges(t *testing.T) {
	config, response := accountFixture(t)
	config.Store = &MemoryStore{record: cachedRecord(response)}
	service := testService(t, config)
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "node"}
	requireBearer(t, source, response.Ticket)
	service.mu.Lock()
	record := cloneRecord(service.record)
	record.Ticket = "fresh-ticket"
	service.adoptLocked(record)
	service.adoptLocked(Record{SetupSecret: "next-account"})
	retained := share.credential.record.Ticket
	service.mu.Unlock()
	if retained != "fresh-ticket" {
		t.Fatal("login lost the last credential refresh")
	}
}

func TestRevokeCredentialSerializesWithOperationAndLocalGates(t *testing.T) {
	config, issued := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config.Store = &MemoryStore{record: cachedRecord(issued)}
		revoked := make(chan struct{})
		config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1/accounts/credentials/revoke" {
				t.Errorf("unexpected route %s", request.URL.Path)
			}
			select {
			case <-revoked:
			default:
				close(revoked)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		})
		service := testService(t, config)

		if err := service.lockOperation(t.Context()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- service.RevokeCredential(t.Context(), issued.Credential) }()
		synctest.Wait()
		select {
		case <-revoked:
			t.Fatal("RevokeCredential bypassed the operation gate")
		default:
		}

		// Hold local before releasing operation so revocation must wait before persisting.
		if err := service.lockLocal(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-service.operation
		synctest.Wait()
		select {
		case <-revoked:
		default:
			t.Fatal("RevokeCredential did not contact the authority after operation released")
		}
		select {
		case err := <-done:
			t.Fatalf("RevokeCredential persisted without the local gate: %v", err)
		default:
		}

		<-service.local
		if err := <-done; err != nil {
			t.Fatalf("RevokeCredential failed: %v", err)
		}
		stored, err := service.load(t.Context())
		if err != nil || stored.Denial != "credential_revoked" {
			t.Fatalf("revocation not persisted: record=%+v err=%v", stored, err)
		}
		if snapshot, _ := service.Snapshot(); snapshot.State != Denied || snapshot.Reason != "credential_revoked" {
			t.Fatalf("revocation not reflected in snapshot: %+v", snapshot)
		}
	})
}

func TestRequestIssuanceDenialSerializesWithLocalGate(t *testing.T) {
	config, issued := accountFixture(t)
	synctest.Test(t, func(t *testing.T) {
		config.Store = &MemoryStore{record: cachedRecord(issued)}
		denied := make(chan struct{})
		config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
			close(denied)
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader(`{"kind":"denied","reason":"credential_revoked"}`)),
			}, nil
		})
		service := testService(t, config)
		record := cachedRecord(issued)

		if err := service.lockLocal(t.Context()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- service.requestIssuance(t.Context(), record, "/v1/accounts/ticket", record.Credential.Secret, struct{}{})
		}()
		<-denied
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("requestIssuance wrote denial without holding the local gate: %v", err)
		default:
		}

		<-service.local
		if err := <-done; CommandErrorCode(err) != "account_denied" {
			t.Fatalf("expected denial fault, got %v", err)
		}
		stored, err := service.load(t.Context())
		if err != nil || stored.Denial != "credential_revoked" {
			t.Fatalf("denial not persisted: record=%+v err=%v", stored, err)
		}
	})
}
