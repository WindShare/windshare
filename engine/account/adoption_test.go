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

	"github.com/windshare/windshare/core/diagnostics"
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
		active, _ := service.Snapshot()

		if err := service.lockOperation(t.Context()); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- service.RevokeCredential(t.Context(), active.Account, issued.Credential) }()
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

func TestLateRefreshDoesNotOverwriteNewerDenial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		deny   func(t *testing.T, cli *Service, active Snapshot, issued session)
	}{
		{
			name:   "concurrent revoke",
			reason: denialCredentialRevoked,
			deny: func(t *testing.T, cli *Service, active Snapshot, issued session) {
				if err := cli.RevokeCredential(t.Context(), active.Account, issued.Credential); err != nil {
					t.Fatalf("cli revoke: %v", err)
				}
			},
		},
		{
			name:   "concurrent account block",
			reason: denialAccountBlocked,
			deny: func(t *testing.T, cli *Service, _ Snapshot, _ session) {
				if _, err := cli.RefreshOnce(t.Context()); CommandErrorCode(err) != "account_denied" {
					t.Fatalf("cli block refresh: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, issued := accountFixture(t)
			config.Store = FileStore{Path: filepath.Join(t.TempDir(), "account.json")}
			if _, err := config.Store.Update(t.Context(), func(Record) (Record, error) {
				return cachedRecord(issued), nil
			}); err != nil {
				t.Fatal(err)
			}

			stream, err := diagnostics.NewStream(32, nil)
			if err != nil {
				t.Fatal(err)
			}
			desktopCfg := config
			desktopCfg.Diagnostics = stream.Sink()
			inFlight := make(chan struct{})
			release := make(chan struct{})
			desktopCfg.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/accounts/ticket" {
					t.Errorf("unexpected desktop route %s", request.URL.Path)
				}
				close(inFlight)
				<-release
				return accountReply(t, http.StatusOK, issued), nil
			})
			desktop := testService(t, desktopCfg)

			cliCfg := config
			cliCfg.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/v1/accounts/credentials/revoke":
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				case "/v1/accounts/ticket":
					return accountReply(t, http.StatusForbidden, fault{Kind: "denied", Reason: tc.reason}), nil
				default:
					t.Errorf("unexpected cli route %s", request.URL.Path)
					return nil, context.Canceled
				}
			})
			cli := testService(t, cliCfg)
			active, _ := cli.Snapshot()

			done := make(chan error, 1)
			go func() {
				_, refreshErr := desktop.RefreshOnce(t.Context())
				done <- refreshErr
			}()
			<-inFlight

			tc.deny(t, cli, active, issued)
			close(release)

			if refreshErr := <-done; CommandErrorCode(refreshErr) != "account_denied" {
				t.Fatalf("late refresh error = %v (%s), want account_denied", refreshErr, CommandErrorCode(refreshErr))
			}
			stored, err := config.Store.Load(t.Context())
			if err != nil || stored.Denial != tc.reason {
				t.Fatalf("stored denial = %q (err=%v), want %q", stored.Denial, err, tc.reason)
			}
			if snapshot, _ := desktop.Snapshot(); snapshot.State != Denied || snapshot.Reason != tc.reason {
				t.Fatalf("desktop snapshot = %+v, want Denied/%s", snapshot, tc.reason)
			}

			desktop.Close()
			var sawSuperseded bool
			for _, rec := range drainDiagnostics(stream) {
				if rec.Name() == "account_issuance_superseded_by_denial" && rec.Payload().Get("reason").Text() == tc.reason {
					sawSuperseded = true
				}
			}
			if !sawSuperseded {
				t.Fatalf("missing account_issuance_superseded_by_denial diagnostic for %s", tc.reason)
			}
		})
	}
}

func TestAccountUnblockStillRecoversWhileRevokedCredentialRequiresNewLogin(t *testing.T) {
	config, issued := accountFixture(t)
	replacement := otherAccount(t, config, issued)

	blocked := cachedRecord(issued)
	blocked.Denial = denialAccountBlocked
	config.Store = &MemoryStore{record: blocked}
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/v1/accounts/ticket":
			return accountReply(t, http.StatusOK, issued), nil
		case "/v1/accounts/login":
			return accountReply(t, http.StatusOK, replacement), nil
		default:
			t.Errorf("unexpected route %s", request.URL.Path)
			return nil, context.Canceled
		}
	})
	service := testService(t, config)

	// A refresh that started from a reversible account_blocked state must still recover on unblock.
	if outcome, err := service.RefreshOnce(t.Context()); err != nil || outcome != RefreshSucceeded {
		t.Fatalf("unblock recovery failed: outcome=%s err=%v", outcome, err)
	}
	if stored, err := config.Store.Load(t.Context()); err != nil || stored.Denial != "" {
		t.Fatalf("unblock did not clear stored denial: %+v, %v", stored, err)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != Ready {
		t.Fatalf("unblock snapshot = %+v, want Ready", snapshot)
	}

	// Permanent credential_revoked cannot be cleared by a non-login refresh even if a stale 200 arrives.
	if _, err := config.Store.Update(t.Context(), func(current Record) (Record, error) {
		current.Denial = denialCredentialRevoked
		return current, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshOnce(t.Context()); CommandErrorCode(err) != "account_denied" {
		t.Fatalf("revoked refresh = %v, want account_denied", err)
	}
	if stored, err := config.Store.Load(t.Context()); err != nil || stored.Denial != denialCredentialRevoked {
		t.Fatalf("revoked denial cleared by refresh: %+v, %v", stored, err)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != Denied || snapshot.Reason != denialCredentialRevoked {
		t.Fatalf("revoked snapshot = %+v, want Denied/%s", snapshot, denialCredentialRevoked)
	}

	// Explicit Login installs a new credential and replaces the revoked one.
	if err := service.Login(t.Context(), "alice", "new-password"); err != nil {
		t.Fatalf("login after revoke: %v", err)
	}
	if stored, err := config.Store.Load(t.Context()); err != nil || stored.Denial != "" || stored.Credential.ID != replacement.Credential {
		t.Fatalf("login did not replace revoked credential: %+v, %v", stored, err)
	}
	if snapshot, _ := service.Snapshot(); snapshot.State != Ready {
		t.Fatalf("post-login snapshot = %+v, want Ready", snapshot)
	}
}
