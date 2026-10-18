package relayv2_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/internal/testrelaynative"
	"github.com/windshare/windshare/internal/testrun"
	"github.com/windshare/windshare/internal/testscenario"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

const (
	relaysetEventCapacity = 32
	relaysetCacheBytes    = uint64(64) << 20
	relaysetFileName      = "file.txt"
	relaysetFileContent   = "multi relay"
)

// All relay-set scenarios borrow the suite binary and supervisor; only durable
// authority and listening sockets belong to each individual native relay.
func startRelaysetScenario(t *testing.T, count int) (context.Context, *testscenario.Trace, []string) {
	t.Helper()
	trace, urls := startNativeRelays(t, count)
	ctx, cancel := context.WithTimeout(t.Context(), relayScenarioTimeout)
	t.Cleanup(cancel)
	return ctx, trace, urls
}

func startNativeRelays(t *testing.T, count int) (*testscenario.Trace, []string) {
	t.Helper()
	if testing.Short() {
		t.Skip("native relay scenarios belong to the long integration gate")
	}
	prepareRelaySuite(t)
	directories := make([]string, count)
	for index := range directories {
		// SQLite directories must outlive the trace's process cleanup on Windows.
		directories[index] = t.TempDir()
	}
	trace, identity := startRelayScenario(t, "integration/relayv2/"+t.Name())
	urls := make([]string, count)
	for index, directory := range directories {
		fixture := testrelaynative.NewFixture(directory, "")
		host, err := fixture.Start(t.Context(), relaySuite.owner, testrelaynative.Launch{
			Executable: relaySuite.binary, WorkingDirectory: relaySuite.root, Identity: identity,
		})
		if err != nil {
			t.Fatal(err)
		}
		trace.RequireCleanup(t, fmt.Sprintf("native relay %d", index+1), host.Close)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("native relay %s: %s", host.Ready.Endpoint, host.Diagnostic())
			}
		})
		trace.RequireRecord(t, testrun.ListenerReadyMilestone, testrun.OutcomeSucceeded, host.Ready)
		urls[index] = host.Ready.BaseURL
	}
	return trace, urls
}

type relaysetTerminal struct{}

func (relaysetTerminal) StopRecovery()                 {}
func (relaysetTerminal) Stop(context.Context) error    { return nil }
func (relaysetTerminal) Cleanup(context.Context) error { return nil }

type relaysetPeer struct{}

func (relaysetPeer) HandleMessage(context.Context, protocolsession.Message) error { return nil }
func (relaysetPeer) Cancel(context.Context, protocolsession.OperationID) error    { return nil }
func (relaysetPeer) Run(ctx context.Context) error                                { <-ctx.Done(); return ctx.Err() }

type relaysetAdmission struct {
	Endpoint string                           `json:"endpoint"`
	Session  v2.RelaySessionID                `json:"relay_session_id"`
	Lane     sessionruntime.LaneIdentity      `json:"lane"`
	Kind     sessionruntime.SenderChannelKind `json:"kind"`
	Err      error                            `json:"-"`
}

type relaysetShare struct {
	*liveshare.PreparedSender
	admitted <-chan relaysetAdmission
}

func prepareRelaysetShare(t *testing.T, ctx context.Context, trace *testscenario.Trace, urls []string) relaysetShare {
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
	file := filepath.Join(t.TempDir(), relaysetFileName)
	if err := os.WriteFile(file, []byte(relaysetFileContent), 0600); err != nil {
		t.Fatal(err)
	}
	config := liveshare.SenderConfig{Relays: urls, RevisionCapacity: capacity.Coordinator()}
	config.Source = liveshare.FileSourceFactoryFunc(func(ctx context.Context, source liveshare.FileSourceContext) (liveshare.FileSource, error) {
		return osfs.NewSelectedFileSource(ctx, osfs.SelectedCatalogSourceConfig{
			Paths: []string{file}, SyntheticRoot: source.SyntheticRoot, Identities: osfs.CatalogIdentitySourceFunc(source.NewIdentity),
		})
	})
	config.CatalogBudget, err = catalog.NewBudgetAccount("relayset-integration", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		t.Fatal(err)
	}
	config.CacheBudget, err = contentflow.NewProcessCacheBudget(relaysetCacheBytes)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := liveshare.PrepareSender(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sender.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := sender.AuthorizeRegistration(); err != nil {
		t.Fatal(err)
	}
	factory, err := sender.NewRuntimeFactory(liveshare.RuntimeFactoryConfig{
		TerminalConnectivity: relaysetTerminal{},
		PeerHandlers: sessionruntime.SenderPeerHandlerFactoryFunc(func(sessionruntime.SenderPeerSession) (sessionruntime.SenderPeerHandler, error) {
			return relaysetPeer{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	material := sender.Registration()
	share, err := v2.ShareIDFromBytes(material.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := v2.ShareInstanceFromBytes(material.ShareInstance)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := v2.PKHashFromBytes(material.PKHash)
	if err != nil {
		t.Fatal(err)
	}
	token := v2.ResumeToken{1}
	init, err := relayv2.NewFreshRegisterInit(share, instance, pk, material.Descriptor, token)
	if err != nil {
		t.Fatal(err)
	}
	admitted := make(chan relaysetAdmission, relaysetEventCapacity)
	for _, url := range urls {
		connection, err := relayv2.DialSender(ctx, relayv2.SenderConfig{
			RelayBaseURL: url, Init: init, Descriptor: material.Descriptor, SenderPrivateKey: material.SenderPrivateKey,
		})
		if err != nil {
			t.Fatalf("register at %s: %v", url, err)
		}
		trace.RequireRecord(t, "sender_registered", testrun.OutcomeSucceeded, struct {
			Endpoint string `json:"endpoint"`
		}{url})
		lifetime, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			var admissions sync.WaitGroup
			defer close(done)
			defer admissions.Wait()
			for {
				channel, err := connection.Accept(lifetime)
				if err != nil {
					return
				}
				admissions.Go(func() {
					result, err := factory.AdmitChannel(lifetime, channel)
					lane := result.Lane
					if result.Session != nil {
						lane.ID, lane.Epoch = result.Session.LaneIdentity()
					}
					event := relaysetAdmission{Endpoint: url, Session: channel.RelaySessionID(), Lane: lane, Kind: result.Kind, Err: err}
					if err != nil {
						_ = channel.Close()
					}
					select {
					case admitted <- event:
					case <-lifetime.Done():
					}
				})
			}
		}()
		t.Cleanup(func() {
			cancel()
			if err := connection.Close(); err != nil {
				t.Error(err)
			}
			select {
			case <-done:
			case <-time.After(relayCleanupTimeout):
				t.Error("sender admission workers did not release ownership")
			}
		})
	}
	return relaysetShare{PreparedSender: sender, admitted: admitted}
}

func (share relaysetShare) waitAdmission(t *testing.T, ctx context.Context, connection *relayv2.ReceiverConnection) relaysetAdmission {
	t.Helper()
	for {
		event := waitRelaysetEvent(t, ctx, share.admitted)
		endpoint, err := v2.NormalizeRelayEndpoint(event.Endpoint)
		if err != nil {
			t.Fatal(err)
		}
		if endpoint.Identity != connection.Endpoint().Identity || event.Session != connection.Channel().RelaySessionID() {
			continue
		}
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		return event
	}
}

func waitRelaysetEvent[T any](t *testing.T, ctx context.Context, events <-chan T) T {
	t.Helper()
	select {
	case value := <-events:
		return value
	case <-ctx.Done():
		t.Fatalf("relay-set event missing: %v", ctx.Err())
		var zero T
		return zero
	}
}

type relaysetClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *relaysetClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *relaysetClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
	return nil
}

func requireRelaysetLive(t *testing.T, runtime *sessionruntime.ReceiverRuntime, connection *relayv2.ReceiverConnection, lane sessionruntime.LaneIdentity) {
	t.Helper()
	done, live := runtime.LaneDone(lane)
	if !live {
		t.Fatalf("healthy lane %v lost its ownership", lane)
	}
	for _, stopped := range []<-chan struct{}{runtime.Done(), connection.Done(), done} {
		select {
		case <-stopped:
			t.Fatalf("healthy relay stopped: %v", errors.Join(runtime.Err(), connection.Err()))
		default:
		}
	}
}
