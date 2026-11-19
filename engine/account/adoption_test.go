package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

type rejectingDenialStore struct{ record Record }

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
	if got := source.RegistrationAuthorization(); got.State != serviceauthority.AdmissionReady {
		t.Fatal(got.State, got.Cause)
	}
	if err := service.step(t.Context()); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("failed persistence: %v", err)
	}
	service.mu.Lock()
	service.adoptLocked(cached)
	service.nodes["node"] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}
	service.mu.Unlock()
	newShare := service.NewShareAuthorization(t.Context())
	defer newShare.Close()
	for _, source := range []*Source{source, {share: newShare, address: "node"}} {
		if got := source.RegistrationAuthorization(); got.State != serviceauthority.AdmissionDenied || got.Bearer != "" {
			t.Fatalf("denial lost: %+v", got)
		}
	}
}

func TestAdoptionUpdatesRetainedCredentialBeforeLoginChanges(t *testing.T) {
	config, response := accountFixture(t)
	config.Store = &MemoryStore{record: cachedRecord(response)}
	service := testService(t, config)
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "node"}
	if got := source.RegistrationAuthorization(); got.State != serviceauthority.AdmissionReady {
		t.Fatal(got.State, got.Cause)
	}
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
