package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/transport/relayv2"
)

func TestShareReadinessActivationAndFinalizationUseDurableAuthority(t *testing.T) {
	prepared := &preparedFacadeShare{}
	relays := &facadeRelays{}
	application := newTestEngine(t, Config{
		ObservationCapacity: 1,
		Share: ShareDependencies{
			Prepare:     func(context.Context, liveshare.SenderConfig) (PreparedShare, error) { return prepared, nil },
			Relays:      func(context.Context, []string, relayset.SenderFactory) (ShareRelays, error) { return relays, nil },
			PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
		},
	})
	current, err := application.StartShare(context.Background(), ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := current.Ready(context.Background())
	if err != nil || ready.RelayEndpoint.DialURL == "" {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	if prepared.prefetched.Load() {
		t.Fatal("root discovery started before capability publication")
	}
	current.Activate(nil)
	if err := current.Activated(context.Background()); err != nil || !prepared.prefetched.Load() {
		t.Fatalf("activation = %v, prefetch = %v", err, prepared.prefetched.Load())
	}
	current.StopShare()
	result, err := current.Wait(context.Background())
	if err != nil || result.Outcome != OutcomeStopped || !result.Value.Ready ||
		result.Err != nil || result.CleanupError != nil || result.StopReason != ShareStopped {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if !prepared.closed.Load() || !prepared.sessionClosed.Load() {
		t.Fatal("final result preceded resource shutdown")
	}
	if result.Observations.CapacityDropped == 0 {
		t.Fatal("test did not exercise an undrained stream")
	}
	// The ready value outlives the observation queue and task shutdown.
	if again, err := current.Ready(context.Background()); err != nil || again.RelayEndpoint != ready.RelayEndpoint {
		t.Fatalf("durable ready = %+v, %v", again, err)
	}
}

func TestSharePublicationFailureCompletesReadinessAndRetainsCause(t *testing.T) {
	prepared := &preparedFacadeShare{}
	application := newTestEngine(t, Config{Share: ShareDependencies{
		Prepare: func(context.Context, liveshare.SenderConfig) (PreparedShare, error) { return prepared, nil },
		Relays: func(context.Context, []string, relayset.SenderFactory) (ShareRelays, error) {
			return &facadeRelays{}, nil
		},
		PollNetwork: func(ctx context.Context, _ func()) { <-ctx.Done() },
	}})
	current, err := application.StartShare(context.Background(), ShareRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("capability sink failed")
	current.Activate(&SharePublicationError{Stage: SharePublicationOutput, Cause: cause})
	if err := current.Activated(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("activation error = %v", err)
	}
	result, err := current.Wait(context.Background())
	if err != nil || !errors.Is(result.Err, cause) || result.Outcome != OutcomeFailed || result.FailureClass != FailureLocal {
		t.Fatalf("publication result = %+v, %v", result, err)
	}
	if prepared.prefetched.Load() || !prepared.closed.Load() {
		t.Fatal("failed publication kept source or started discovery")
	}
}

type preparedFacadeShare struct {
	prefetched    atomic.Bool
	closed        atomic.Bool
	sessionClosed atomic.Bool
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
func (prepared *preparedFacadeShare) StartRootPrefetch() { prepared.prefetched.Store(true) }
func (prepared *preparedFacadeShare) Close() error       { prepared.closed.Store(true); return nil }

type facadeSessionFactory struct{ prepared *preparedFacadeShare }

func (facadeSessionFactory) AdmitChannel(context.Context, protocolsession.FrameChannel) (sessionruntime.SenderChannelAdmission, error) {
	return sessionruntime.SenderChannelAdmission{}, errors.New("unexpected session")
}
func (factory facadeSessionFactory) Stop(context.Context, string) error {
	factory.prepared.sessionClosed.Store(true)
	return nil
}

type facadeRelays struct{}

func (*facadeRelays) Accept(ctx context.Context) (*relayv2.Channel, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*facadeRelays) WaitReady(context.Context) error                       { return nil }
func (*facadeRelays) ReadyRelayURL() string                                 { return "ws://relay.example:8080" }
func (*facadeRelays) ObserveAvailability(func(relayset.SenderAvailability)) {}
func (*facadeRelays) Wake()                                                 {}
func (*facadeRelays) StopRecovery()                                         {}
func (*facadeRelays) Cleanup(context.Context) error                         { return nil }
