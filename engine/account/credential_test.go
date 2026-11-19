package account

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestShareAttributionSurvivesLoginAcrossOfficialNodes(t *testing.T) {
	config, first := accountFixture(t)
	second := otherAccount(t, config, first)
	previous := cachedRecord(first)
	config.Store = &MemoryStore{record: previous}
	service := testService(t, config)
	published := service.NewShareAuthorization(t.Context())
	defer published.Close()
	starting := service.NewShareAuthorization(t.Context())
	defer starting.Close()
	requireBearer(t, &Source{share: published, address: "primary"}, first.Ticket)

	if err := service.install(t.Context(), previous, second, true); err != nil {
		t.Fatal(err)
	}
	requireBearer(t, &Source{share: published, address: "backup"}, first.Ticket)
	requireBearer(t, &Source{share: starting, address: "primary"}, first.Ticket)

	newShare := service.NewShareAuthorization(t.Context())
	defer newShare.Close()
	requireBearer(t, &Source{share: newShare, address: "primary"}, second.Ticket)

	published.Close()
	published.Close()
	requireBearer(t, &Source{share: starting, address: "backup"}, first.Ticket)
	starting.Close()
	if _, retained := service.credentials[first.Credential]; retained {
		t.Fatal("old credential outlived its shares")
	}
	if got := (&Source{share: published, address: "backup"}).RegistrationAuthorization(); got.State != serviceauthority.AdmissionDenied {
		t.Fatal("closed share acquired authorization", got.State)
	}
}

func TestAnonymousShareKeepsAttributionWhenSetupFinishes(t *testing.T) {
	config, response := accountFixture(t)
	service := testService(t, config)
	anonymous := service.NewShareAuthorization(t.Context())
	defer anonymous.Close()
	waiting := service.NewShareAuthorization(t.Context())
	defer waiting.Close()
	primary := &Source{share: anonymous, address: "primary"}
	backup := &Source{share: anonymous, address: "backup"}
	pending := backup.RegistrationAuthorization()
	if pending.State != serviceauthority.AdmissionPendingSetup {
		t.Fatal(pending.State)
	}
	service.nodes[primary.address] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}
	requireBearer(t, primary, "")
	select {
	case <-pending.Changed:
	default:
		t.Fatal("selecting anonymous authorization did not wake the backup")
	}

	service.mu.Lock()
	service.issuedLocked(cachedRecord(response))
	service.mu.Unlock()
	requireBearer(t, backup, "")
	requireBearer(t, &Source{share: waiting, address: "backup"}, response.Ticket)
}

func requireBearer(t *testing.T, source *Source, want string) {
	t.Helper()
	got := source.RegistrationAuthorization()
	if got.State != serviceauthority.AdmissionReady || got.Bearer != want {
		t.Fatalf("%s did not receive the intended authorization (state %s, cause %v)", source.address, got.State.Name(), got.Cause)
	}
}

func otherAccount(t *testing.T, config Config, first session) session {
	t.Helper()
	generation, err := serviceauthority.VerifyGeneration(first.Certificate, config.Root)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Account, second.Credential = [16]byte{1}, [16]byte{2}
	entitlement := second.Entitlement
	payload, err := cbor.Marshal([]any{
		1, generation.Issuer[:], generation.Number, second.Account[:], second.Credential[:],
		uint64(config.Now().Unix()), second.ExpiresAt, "official",
		[]uint64{entitlement.Version, entitlement.Day, entitlement.ValidUntil, entitlement.RemainingBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Use the online signing key of the Rust-generated test fixture.
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{42}, ed25519.SeedSize))
	signature := ed25519.Sign(key, append([]byte(serviceauthority.TicketDomain), payload...))
	second.Ticket = base64.RawURLEncoding.EncodeToString(append(payload, signature...))
	return second
}

func TestOldPublicationRefreshCannotReplaceNewShareAccount(t *testing.T) {
	config, response := accountFixture(t)
	config.Store = &MemoryStore{record: cachedRecord(response)}
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/accounts/ticket" || request.Header.Get("Authorization") != "Bearer "+response.Secret {
			t.Error("old publication refreshed with a different credential")
		}
		body, _ := json.Marshal(response)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})
	service := testService(t, config)
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	primary := &Source{share: share, address: "primary"}
	backup := &Source{share: share, address: "backup"}
	requireBearer(t, primary, response.Ticket)
	service.mu.Lock()
	share.credential.record.Ticket = ""
	service.adoptLocked(Record{SetupSecret: "new-selection"})
	service.mu.Unlock()
	pending := backup.RegistrationAuthorization()
	if pending.State != serviceauthority.AdmissionExpired {
		t.Fatal(pending.State)
	}
	select {
	case <-pending.Changed:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not wake publication")
	}
	requireBearer(t, backup, response.Ticket)
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.record.SetupSecret != "new-selection" {
		t.Fatal("old refresh replaced current account")
	}
}

type unavailableStore struct{}

func (unavailableStore) Update(context.Context, func(Record) (Record, error)) (Record, error) {
	return Record{}, errors.New("account cache unavailable")
}
func TestUnreadableAccountCacheDoesNotBlockEngineOrAssumeAnonymous(t *testing.T) {
	config, _ := accountFixture(t)
	config.Store = unavailableStore{}
	service := testService(t, config)
	service.nodes["node"] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "node"}
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionTemporaryFailure {
		t.Fatal("unknown local denial was discarded")
	}
}
