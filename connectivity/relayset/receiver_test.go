package relayset

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/relay/httpapi"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/relay/signaling/v2endpoint"
	"github.com/windshare/windshare/relay/signaling/v2route"
	"github.com/windshare/windshare/transport/relayv2"
)

type receiverTombstones struct{}

func (receiverTombstones) Load(context.Context) ([]v2route.Tombstone, error) { return nil, nil }
func (receiverTombstones) Commit(context.Context, v2route.Tombstone) (v2route.CommitOutcome, error) {
	return v2route.CommitCommitted, nil
}

func testReceiverRelay(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	identity, err := v2.NormalizeRelayEndpoint("http://" + server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := v2route.New(context.Background(), v2route.Config{MaxRoutes: 8, MaxSessions: 32, MaxSessionsPerShare: 16, Random: rand.Reader, Tombstones: receiverTombstones{}})
	if err != nil {
		t.Fatal(err)
	}
	challenges, err := v2.NewChallengeLedger(v2.ChallengeLedgerConfig{Capacity: 32, Random: rand.Reader})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := v2endpoint.New(v2endpoint.Config{Registry: registry, Challenges: challenges, RelayIdentity: identity.Identity})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = httpapi.NewV2Handler(httpapi.V2Config{Server: endpoint, AllowLocalhost: true, AdmitConnection: func(string) (func(), bool) { return func() {}, true }})
	server.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = endpoint.Shutdown(ctx)
		server.Close()
	})
	return server
}

type receiverTestTerminal struct{}

func (receiverTestTerminal) StopRecovery()                 {}
func (receiverTestTerminal) Stop(context.Context) error    { return nil }
func (receiverTestTerminal) Cleanup(context.Context) error { return nil }

type receiverTestPeer struct{}

func (receiverTestPeer) HandleMessage(context.Context, protocolsession.Message) error { return nil }
func (receiverTestPeer) Cancel(context.Context, protocolsession.OperationID) error    { return nil }
func (receiverTestPeer) Run(ctx context.Context) error                                { <-ctx.Done(); return ctx.Err() }

func receiverTestShare(t *testing.T, urls []string) *liveshare.PreparedSender {
	t.Helper()
	capacity, err := revisioncapacity.NewProcessOwner(revisioncapacity.DefaultProcessConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := capacity.Close(); err != nil {
			t.Error(err)
		}
	})
	file := filepath.Join(t.TempDir(), "file.txt")
	if err = os.WriteFile(file, []byte("multi relay"), 0600); err != nil {
		t.Fatal(err)
	}
	sender, err := liveshare.PrepareSender(context.Background(), liveshare.SenderConfig{Paths: []string{file}, Relays: urls, RevisionCapacity: capacity.Coordinator()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	if err = sender.AuthorizeRegistration(); err != nil {
		t.Fatal(err)
	}
	factory, err := sender.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{TerminalConnectivity: receiverTestTerminal{}, PeerHandlers: sessionruntime.SenderPeerHandlerFactoryFunc(func(sessionruntime.SenderPeerSession) (sessionruntime.SenderPeerHandler, error) {
		return receiverTestPeer{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	material := sender.Registration()
	share, _ := v2.ShareIDFromBytes(material.ShareID)
	instance, _ := v2.ShareInstanceFromBytes(material.ShareInstance)
	pk, _ := v2.PKHashFromBytes(material.PKHash)
	token := v2.ResumeToken{}
	token[0] = 1
	init := v2.RegisterInit{Mode: v2.RegistrationFresh, ShareID: share, ShareInstance: instance, PKHash: pk, DescriptorDigest: sha256.Sum256(material.Descriptor), ResumeTokenHash: sha256.Sum256(token[:])}
	for _, url := range urls {
		connection, err := relayv2.DialSender(context.Background(), relayv2.SenderConfig{RelayBaseURL: url, Init: init, Descriptor: material.Descriptor, SenderPrivateKey: material.SenderPrivateKey})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				channel, err := connection.Accept(ctx)
				if err != nil {
					return
				}
				go func() { _, _ = factory.AdmitChannel(ctx, channel) }()
			}
		}()
		t.Cleanup(func() { cancel(); _ = connection.Close(); <-done })
	}
	return sender
}

func TestReceiverFirstUsableSessionDoesNotWaitForExtraRelayAndReconnectsExactLane(t *testing.T) {
	first, second := testReceiverRelay(t), testReceiverRelay(t)
	sender := receiverTestShare(t, []string{first.URL, second.URL})
	release := make(chan struct{})
	connected := make(chan *relayv2.ReceiverConnection, 8)
	var secondaryDials atomic.Int32
	set, err := NewReceiver(context.Background(), ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capability: sender.Capability()},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL == second.URL {
				secondaryDials.Add(1)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-release:
				}
			}
			return relayv2.DialReceiver(ctx, config)
		},
		Connected: func(c *relayv2.ReceiverConnection) { connected <- c },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runtime, primary, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.LaneSet().Len() != 1 {
		t.Fatal("blocked endpoint delayed first usable lane")
	}
	<-connected
	close(release)
	var additional *relayv2.ReceiverConnection
	select {
	case additional = <-connected:
	case <-ctx.Done():
		t.Fatal("additional relay was not admitted")
	}
	if additional == primary || runtime.LaneSet().Len() != 2 {
		t.Fatal("extra relay created a separate session")
	}
	sessionID := runtime.ProtocolSessionID()
	_ = additional.Channel().Close()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("retired relay channel was not reconnected")
	}
	if secondaryDials.Load() != 2 || !runtime.ProtocolSessionID().Equal(sessionID) || runtime.LaneSet().Len() != 2 {
		t.Fatalf("dials=%d lanes=%d", secondaryDials.Load(), runtime.LaneSet().Len())
	}
	select {
	case <-runtime.Done():
		t.Fatal("one endpoint closed the shared session")
	default:
	}
	set.Close()
	<-runtime.Done()
}

func TestInitiallyFailedRelayJoinsFirstAuthenticatedWinner(t *testing.T) {
	first, second := testReceiverRelay(t), testReceiverRelay(t)
	sender := receiverTestShare(t, []string{first.URL, second.URL})
	failed := make(chan struct{})
	connected := make(chan *relayv2.ReceiverConnection, 8)
	var secondaryDials atomic.Int32
	set, err := NewReceiver(context.Background(), ReceiverConfig{
		Receiver: liveshare.ReceiverConfig{Capability: sender.Capability()},
		Dial: func(ctx context.Context, config relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
			if config.RelayBaseURL == second.URL && secondaryDials.Add(1) == 1 {
				close(failed)
				return nil, errors.New("transient endpoint failure")
			}
			if config.RelayBaseURL == first.URL {
				<-failed
			}
			return relayv2.DialReceiver(ctx, config)
		},
		Connected: func(connection *relayv2.ReceiverConnection) { connected <- connection },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runtime, _, err := set.WaitReady(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-connected:
		case <-ctx.Done():
			t.Fatal("initial failed endpoint did not rejoin winner")
		}
	}
	if runtime.LaneSet().Len() != 2 || secondaryDials.Load() != 2 {
		t.Fatal("endpoint lost independent recovery")
	}
}

type receiverFakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
}

func (c *receiverFakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *receiverFakeClock) Wait(ctx context.Context, d time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return nil
}
func TestReceiverInitialStartingUsesBoundedClockAndAllFailuresSettle(t *testing.T) {
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	capability := link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: []string{"one", "two"}}
	clock := &receiverFakeClock{now: time.Unix(1, 0)}
	var calls atomic.Int32
	rejection := errors.New("endpoint offline")
	set, err := NewReceiver(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: capability}, Clock: clock, Dial: func(context.Context, relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
		if calls.Add(1) <= 2 {
			return nil, &relayv2.RelayError{Code: v2.ErrorStarting}
		}
		return nil, rejection
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	_, _, err = set.WaitReady(context.Background())
	if !errors.Is(err, rejection) {
		t.Fatal(err)
	}
	if len(clock.waits) != 2 || clock.waits[0] != receiverRetryDelay {
		t.Fatal(clock.waits)
	}
	var missingContext context.Context // Constructors reject missing lifetime ownership.
	if _, err = NewReceiver(missingContext, ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: capability}}); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err = NewReceiver(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: link.Link{Relays: []string{"one"}, ShareID: "invalid!"}}}); err == nil {
		t.Fatal("bad share accepted")
	}
}

func TestReceiverCancellationJoinsPendingDial(t *testing.T) {
	raw := make([]byte, v2.ShareIDBytes)
	raw[0] = 1
	entered := make(chan struct{})
	set, err := NewReceiver(context.Background(), ReceiverConfig{Receiver: liveshare.ReceiverConfig{Capability: link.Link{ShareID: base64.RawURLEncoding.EncodeToString(raw), Relays: []string{"one"}}}, Dial: func(ctx context.Context, _ relayv2.ReceiverConfig) (*relayv2.ReceiverConnection, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = set.WaitReady(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	set.Close()
	set.Close()
	if _, _, err = set.WaitReady(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
