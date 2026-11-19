package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
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

func (store *interruptedSelectionStore) Update(ctx context.Context, change func(Record) (Record, error)) (Record, error) {
	if store.err != nil {
		return Record{}, store.err
	}
	return store.MemoryStore.Update(ctx, change)
}

func TestShareWaitsForConfirmedSelectionAfterLocalReadFailure(t *testing.T) {
	config, first := accountFixture(t)
	second := otherAccount(t, config, first)
	store := &interruptedSelectionStore{MemoryStore: MemoryStore{record: cachedRecord(first)}}
	config.Store = store
	service := testService(t, config)
	service.nodes["primary"] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}

	failure := errors.New("could not open shared account state")
	store.err = failure
	share := service.NewShareAuthorization(t.Context())
	defer share.Close()
	source := &Source{share: share, address: "primary"}
	waiting := source.RegistrationAuthorization()
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
