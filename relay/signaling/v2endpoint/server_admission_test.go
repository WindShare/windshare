package v2endpoint

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestServerRejectsInvalidConfigurationAndFrames(t *testing.T) {
	if _, err := New(Config{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("empty config error = %v", err)
	}
	if err := (*Server)(nil).Serve(context.Background(), nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil serve error = %v", err)
	}
	client, server := newMemorySocketPair()
	defer client.Close(websocket.StatusNormalClosure, "")
	if err := client.Write(context.Background(), websocket.MessageText, []byte("WS2J")); err != nil {
		t.Fatal(err)
	}
	if _, err := readBinary(context.Background(), server); !errors.Is(err, ErrProtocol) {
		t.Fatalf("text frame error = %v", err)
	}
}

func TestStopStorageFailuresAreClassifiedAsAdmissionNotMalformed(t *testing.T) {
	for name, cause := range map[string]error{
		"definite":  errors.Join(v2route.ErrAdmission, v2route.ErrCommitFailed),
		"uncertain": errors.Join(v2route.ErrAdmission, v2route.ErrCommitUncertain),
	} {
		t.Run(name, func(t *testing.T) {
			if code := registryErrorCode(cause); code != v2.ErrorAdmission {
				t.Fatalf("storage failure code = %d, want admission", code)
			}
		})
	}
}

func TestRegisteredWriteFailureLeavesRecoverableRoute(t *testing.T) {
	const relayBase = "https://relay.example/team"
	endpoint, err := v2.NormalizeRelayEndpoint(relayBase)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{
		MaxRoutes: 4, MaxSessions: 16, MaxSessionsPerShare: 8,
		Random: &sequenceReader{next: 1}, Tombstones: &memoryTombstoneStore{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{
		Capacity: 16, Random: &sequenceReader{next: 31},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Registry: registry, Challenges: ledger, RelayIdentity: endpoint.Identity,
		ConnectionIDs: ConnectionIDSourceFunc(func() (v2route.ConnectionID, error) {
			return "registration-write-failure", nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	dial := func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		failing := &failNthWriteSocket{
			BinaryConnection: relay,
			failAt:           2, // Challenge succeeds; REGISTERED fails.
			err:              errors.New("injected REGISTERED write failure"),
		}
		go func() { serveDone <- server.Serve(context.Background(), failing) }()
		return client, nil
	}
	fixture := newEndpointFixture(t)
	if _, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{
		RelayBaseURL: relayBase, Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, Dial: relayv2.DialOptions{SocketDialer: dial},
	}); err == nil {
		t.Fatal("registration unexpectedly survived a failed REGISTERED write")
	}
	select {
	case <-serveDone:
	case <-time.After(time.Second):
		t.Fatal("relay did not clean up the failed registration")
	}
	result, err := registry.Join(fixture.init.ShareID, endpointTestConnectionRef("receiver"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != v2route.JoinStarting {
		t.Fatalf("route status after failed REGISTERED write = %d, want crash grace", result.Status)
	}
}
