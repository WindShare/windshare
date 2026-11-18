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
	"testing"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

type clientFunc func(*http.Request) (*http.Response, error)

func (call clientFunc) Do(request *http.Request) (*http.Response, error) { return call(request) }

func accountFixture(t *testing.T) (Config, session) {
	t.Helper()
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Now     int64
		Root    string
		Session session
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	root, err := serviceauthority.PublicKey(fixture.Root)
	if err != nil {
		t.Fatal(err)
	}
	return Config{URL: "https://api.example", Root: root, Store: &MemoryStore{}, Now: func() time.Time { return time.Unix(fixture.Now, 0) },
		ResolveNode: func(string) (Node, bool) { return Node{Key: [32]byte{1}}, true }}, fixture.Session
}
func cachedRecord(response session) Record {
	return Record{Credential: &Credential{Account: response.Account, ID: response.Credential, Secret: response.Secret},
		Certificate: response.Certificate, Ticket: response.Ticket, Entitlement: response.Entitlement}
}
func testService(t *testing.T, config Config) *Service {
	t.Helper()
	service, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	return service
}

func TestCachedTicketDoesNotWaitForAuthorityAndBlockedAccountNeverBecomesAnonymous(t *testing.T) {
	config, response := accountFixture(t)
	config.Client = clientFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("authorization performed network I/O")
		return nil, nil
	})
	_, err := config.Store.Update(t.Context(), func(Record) (Record, error) { return cachedRecord(response), nil })
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, config)
	source := &Source{service: service, address: "https://node.example"}
	if authorization := source.RegistrationAuthorization(); authorization.State != serviceauthority.AdmissionReady || authorization.Bearer != response.Ticket {
		t.Fatal(authorization.State)
	}
	service.mu.Lock()
	service.nodes[source.address] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Hour)}
	service.record.Denial = "account_blocked"
	service.mu.Unlock()
	if authorization := source.RegistrationAuthorization(); authorization.State != serviceauthority.AdmissionDenied || authorization.Bearer != "" {
		t.Fatal("blocked account fell back to anonymous")
	}
	service.mu.Lock()
	service.record.Denial = ""
	service.mu.Unlock()
	service.config.ResolveNode = func(string) (Node, bool) { return Node{}, false }
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionDenied {
		t.Fatal("ticket sent to removed official node")
	}
}

func TestAnonymousAdmissionDuringSetupRequiresFreshSignedNodePolicy(t *testing.T) {
	config, _ := accountFixture(t)
	service := testService(t, config)
	source := &Source{service: service, address: "https://node.example"}
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionPendingSetup {
		t.Fatal("unknown anonymous policy was assumed")
	}
	service.nodes[source.address] = &nodeState{key: [32]byte{1}, anonymous: true, available: true, expires: config.Now().Add(time.Minute)}
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionReady {
		t.Fatal("usable anonymous admission waited for setup")
	}
	service.nodes[source.address].expires = config.Now()
	if source.RegistrationAuthorization().State != serviceauthority.AdmissionPendingSetup {
		t.Fatal("expired policy permitted anonymous admission")
	}
}

func TestRegistrationRetriesReusePersistedEntropyAndRefreshKeepsCredential(t *testing.T) {
	config, response := accountFixture(t)
	calls := 0
	var entropy string
	config.Client = clientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		var saved Record
		_, err := config.Store.Update(t.Context(), func(value Record) (Record, error) { saved = value; return value, nil })
		if err != nil {
			t.Fatal(err)
		}
		if calls <= 2 {
			if saved.SetupSecret == "" {
				t.Fatal("setup entropy was not durable before HTTP")
			}
			if calls == 1 {
				entropy = saved.SetupSecret
				return nil, errors.New("response lost")
			}
			if saved.SetupSecret != entropy {
				t.Fatal("retry created a new principal")
			}
		} else if request.Header.Get("Authorization") != "Bearer "+entropy {
			t.Fatal("refresh did not use the persistent credential")
		}
		response.Secret = entropy
		data, _ := json.Marshal(response)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	service := testService(t, config)
	if service.step(t.Context()) == nil {
		t.Fatal("expected response loss")
	}
	if err := service.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := service.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || service.record.Credential.ID != response.Credential || service.record.SetupSecret != "" {
		t.Fatal("credential changed")
	}
}

func TestCLIAndDesktopConcurrentSetupShareOneDurableRegistration(t *testing.T) {
	config, _ := accountFixture(t)
	config.Store = FileStore{Path: filepath.Join(t.TempDir(), "account.json")}
	first, second := testService(t, config), testService(t, config)
	var records [2]Record
	var failures [2]error
	var workers sync.WaitGroup
	for i, service := range []*Service{first, second} {
		workers.Go(func() { records[i], failures[i] = service.initialize(t.Context()) })
	}
	workers.Wait()
	if failures[0] != nil || failures[1] != nil || records[0].SetupSecret == "" || records[0].SetupSecret != records[1].SetupSecret {
		t.Fatal(records, failures)
	}
}

func TestProofOfWorkAndCancellation(t *testing.T) {
	config, _ := accountFixture(t)
	service := testService(t, config)
	solution, err := service.solve(t.Context(), Challenge{ID: "test", Mode: "proof_of_work", Bits: 8, ExpiresAt: uint64(config.Now().Unix()) + 900})
	if err != nil || solution == "" {
		t.Fatal(solution, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.solve(ctx, Challenge{ID: "test", Mode: workChallenge, Bits: 32, ExpiresAt: uint64(config.Now().Unix()) + 900}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
