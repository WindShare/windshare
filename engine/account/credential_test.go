package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func TestPublicationAttributionSurvivesLoginAndReleasesCredential(t *testing.T) {
	config, response := accountFixture(t)
	service := testService(t, config)
	service.nodes["node"] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}
	anonymous := &Source{service: service, address: "node"}
	releaseAnonymous := anonymous.RegistrationAuthorization().Retain()
	defer releaseAnonymous()
	service.record = cachedRecord(response)
	authenticated := &Source{service: service, address: "node"}
	release := authenticated.RegistrationAuthorization().Retain()
	if anonymous.RegistrationAuthorization().Bearer != "" {
		t.Fatal("login changed anonymous attribution")
	}
	service.record = Record{}
	if authenticated.RegistrationAuthorization().Bearer != response.Ticket {
		t.Fatal("login erased a live publication's credential")
	}
	if len(service.credentials) != 1 {
		t.Fatal("missing retained credential")
	}
	release()
	release()
	if len(service.credentials) != 0 {
		t.Fatal("publication leaked its credential")
	}
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
	source := &Source{service: service, address: "node"}
	release := source.RegistrationAuthorization().Retain()
	defer release()
	service.mu.Lock()
	source.credential.record.Ticket = ""
	service.record = Record{SetupSecret: "new-selection"}
	service.mu.Unlock()
	pending := source.RegistrationAuthorization()
	if pending.State != serviceauthority.AdmissionExpired {
		t.Fatal(pending.State)
	}
	select {
	case <-pending.Changed:
	case <-time.After(time.Second):
		t.Fatal("credential refresh did not wake publication")
	}
	if source.RegistrationAuthorization().Bearer != response.Ticket {
		t.Fatal("old credential failed to refresh")
	}
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
	source := &Source{service: service, address: "node"}
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionTemporaryFailure {
		t.Fatal("unknown local denial was discarded")
	}
}
