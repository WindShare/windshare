package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"

	sendersession "github.com/windshare/windshare/core/session/sendersession"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/relayprofile"
)

const shareStartupTestTimeout = 5 * time.Second

func TestShareChunkSizeValidationPrecedesSenderPreparation(t *testing.T) {
	tests := []struct {
		name      string
		size      uint32
		wantValid bool
	}{
		{name: "default", wantValid: true},
		{name: "minimum", size: catalog.MinChunkSize, wantValid: true},
		{name: "explicit default", size: catalog.DefaultChunkSize, wantValid: true},
		{name: "maximum", size: catalog.MaxChunkSize, wantValid: true},
		{name: "below minimum", size: catalog.MinChunkSize - 1},
		{name: "not a power of two", size: catalog.MinChunkSize + 1},
		{name: "above maximum", size: catalog.MaxChunkSize + 1},
		{name: "maximum wire integer", size: ^uint32(0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preparationStopped := errors.New("preparation stopped")
			prepared := false
			var preparedSize uint32
			application := newTestEngine(t, Config{Share: ShareDependencies{
				Prepare: func(_ context.Context, config liveshare.SenderConfig) (PreparedShare, error) {
					prepared, preparedSize = true, config.ChunkSize
					return nil, preparationStopped
				},
			}})
			ctx, cancel := context.WithTimeout(t.Context(), shareStartupTestTimeout)
			defer cancel()
			current, err := application.StartShare(ctx, ShareRequest{ChunkSize: test.size})
			if err != nil {
				t.Fatal(err)
			}
			result, err := current.Wait(ctx)
			if err != nil || result.Outcome != OutcomeFailed || result.FailureClass != FailureUsage {
				t.Fatalf("result = %+v, %v", result, err)
			}
			if test.wantValid {
				if !prepared || preparedSize != test.size || !errors.Is(result.Err, preparationStopped) {
					t.Fatalf("valid chunk size was changed or rejected: prepared=%v size=%d result=%+v", prepared, preparedSize, result)
				}
				return
			}
			if prepared || !errors.Is(result.Err, ErrInvalidShareChunkSize) || result.Failure.Code() != appobs.FailureInvalidInput {
				t.Fatalf("invalid chunk size lost its input failure: prepared=%v result=%+v", prepared, result)
			}
			if _, err := current.Ready(ctx); !errors.Is(err, ErrInvalidShareChunkSize) {
				t.Fatalf("readiness lost the input error: %v", err)
			}
			settled := 0
			for observation := range current.Observations() {
				event, ok := observation.Event.(appobs.EngineTaskObserved)
				if !ok || event.Facts().Stage != appobs.EngineTaskSettled {
					continue
				}
				settled++
				if event.Facts().Failure.Code() != appobs.FailureInvalidInput || event.Facts().FailureClass != appobs.EngineFailureUsage {
					t.Fatalf("task observation lost the input failure: %+v", event.Facts())
				}
			}
			if settled != 1 {
				t.Fatalf("settled observations = %d", settled)
			}
		})
	}
}

func TestShareReadinessAndFinalizationUseDurableAuthority(t *testing.T) {
	prepared := &preparedFacadeShare{}
	relays := &facadeRelays{accepting: make(chan struct{})}
	peers := &facadePeers{}
	application := newTestEngine(t, Config{
		ObservationCapacity: 1,
		Share: ShareDependencies{
			Peers:       func() (SharePeers, error) { return peers, nil },
			Prepare:     func(context.Context, liveshare.SenderConfig) (PreparedShare, error) { return prepared, nil },
			Relays:      func(context.Context, []relayprofile.Candidate) (ShareRelays, error) { return relays, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		},
	})
	current, err := application.StartShare(context.Background(), ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := current.Ready(context.Background())
	if err != nil || !ready.RelayOrigin.Valid() {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	select {
	case <-relays.accepting:
	case <-time.After(shareStartupTestTimeout):
		t.Fatal("ready share cannot admit receivers without caller activation")
	}
	current.StopShare()
	result, err := current.Wait(context.Background())
	if err != nil || result.Outcome != OutcomeStopped || !result.Value.Ready ||
		result.Err != nil || result.CleanupError != nil || result.StopReason != ShareStopped {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !prepared.closed.Load() || !prepared.sessionClosed.Load() || !peers.closed.Load() {
		t.Fatal("final result preceded resource shutdown")
	}
	if result.Observations.CapacityDropped == 0 {
		t.Fatal("test did not exercise an undrained stream")
	}
	// The ready value outlives the observation queue and task shutdown.
	if again, err := current.Ready(context.Background()); err != nil || again.RelayOrigin != ready.RelayOrigin {
		t.Fatalf("durable ready = %+v, %v", again, err)
	}
}

func TestShareStartsWithoutReadinessConsumer(t *testing.T) {
	prepared := &preparedFacadeShare{prefetchStarted: make(chan struct{})}
	application := newTestEngine(t, Config{Share: ShareDependencies{
		Peers:   func() (SharePeers, error) { return &facadePeers{}, nil },
		Prepare: func(context.Context, liveshare.SenderConfig) (PreparedShare, error) { return prepared, nil },
		Relays: func(context.Context, []relayprofile.Candidate) (ShareRelays, error) {
			return &facadeRelays{}, nil
		},
		PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
	}})
	current, err := application.StartShare(context.Background(), ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-prepared.prefetchStarted:
	case <-time.After(shareStartupTestTimeout):
		t.Fatal("unobserved readiness held share startup")
	}
	current.StopShare()
	result, err := current.Wait(context.Background())
	if err != nil || result.Err != nil || result.Outcome != OutcomeStopped || !result.Value.Ready {
		t.Fatalf("unobserved share result = %+v, %v", result, err)
	}
	if !prepared.closed.Load() {
		t.Fatal("stopped share retained its source")
	}
	if _, err := current.Ready(context.Background()); err != nil {
		t.Fatalf("first readiness query after shutdown = %v", err)
	}
}

type preparedFacadeShare struct {
	prefetchStarted chan struct{}
	closed          atomic.Bool
	sessionClosed   atomic.Bool
}

func (*preparedFacadeShare) AuthorizeRegistration() error { return nil }
func (*preparedFacadeShare) Registration() liveshare.RegistrationMaterial {
	return liveshare.RegistrationMaterial{}
}
func (*preparedFacadeShare) Capability() link.Link { return link.Link{} }
func (*preparedFacadeShare) SelectedRootSummary() liveshare.SelectedRootSummary {
	return liveshare.SelectedRootSummary{}
}
func (prepared *preparedFacadeShare) NewRuntimeFactory(liveshare.RuntimeFactoryConfig) (ShareSessionFactory, error) {
	return facadeSessionFactory{prepared}, nil
}
func (prepared *preparedFacadeShare) StartRootPrefetch() {
	if prepared.prefetchStarted != nil {
		close(prepared.prefetchStarted)
	}
}
func (prepared *preparedFacadeShare) Close() error { prepared.closed.Store(true); return nil }

type facadeSessionFactory struct{ prepared *preparedFacadeShare }

func (facadeSessionFactory) AdmitChannel(context.Context, protocolsession.FrameChannel) (sendersession.ChannelAdmission, error) {
	return sendersession.ChannelAdmission{}, errors.New("unexpected session")
}
func (factory facadeSessionFactory) Stop(context.Context, string) error {
	factory.prepared.sessionClosed.Store(true)
	return nil
}

type facadeRelays struct{ accepting chan struct{} }

func (relays *facadeRelays) Accept(ctx context.Context) (protocolsession.FrameChannel, error) {
	if relays.accepting != nil {
		close(relays.accepting)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*facadeRelays) WaitReady(context.Context) error { return nil }
func (*facadeRelays) ReadyRelayURL() string           { return "ws://relay.example:8080" }
func (*facadeRelays) Wake()                           {}
func (*facadeRelays) StopRecovery()                   {}
func (*facadeRelays) Cleanup(context.Context) error   { return nil }

type facadePeers struct {
	sendersession.PeerHandlerFactoryFunc
	closed atomic.Bool
}

func (peers *facadePeers) Close(context.Context) error { peers.closed.Store(true); return nil }
func TestShareReleasesInjectedPeerAuthorityAfterPartialConstruction(t *testing.T) {
	peers := &facadePeers{}
	failure := errors.New("peer construction failed")
	application := newTestEngine(t, Config{Share: ShareDependencies{
		Prepare: func(context.Context, liveshare.SenderConfig) (PreparedShare, error) {
			return &preparedFacadeShare{}, nil
		},
		Relays:      func(context.Context, []relayprofile.Candidate) (ShareRelays, error) { return &facadeRelays{}, nil },
		Peers:       func() (SharePeers, error) { return peers, failure },
		PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
	}})
	current, err := application.StartShare(t.Context(), ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(t.Context())
	if err != nil || !errors.Is(result.Err, failure) || !result.Failure.Valid() || !peers.closed.Load() {
		t.Fatalf("partial construction retained authority: %+v %v closed=%v", result, err, peers.closed.Load())
	}
}
