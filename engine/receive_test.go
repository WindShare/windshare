package engine

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
)

type admissionOutput struct{}

func (admissionOutput) BindDestination(context.Context) (OutputMode, error) {
	return OutputLiveOnly, nil
}

func (admissionOutput) LookupActive(context.Context, transfer.SelectionSpec) (OutputLookup, error) {
	return OutputLookup{}, errors.New("live output cannot reopen")
}

func (admissionOutput) Close() error { return nil }

func TestReceiveOwnsCapabilityAtAdmission(t *testing.T) {
	const originalRelay = "ws://original.example:8080"
	capability, err := link.NewSenderAuthenticated(
		bytes.Repeat([]byte{1}, link.ReadSecretBytes),
		ed25519.PublicKey(bytes.Repeat([]byte{2}, ed25519.PublicKeySize)),
		[]string{originalRelay},
	)
	if err != nil {
		t.Fatal(err)
	}
	outputEntered, releaseOutput := make(chan struct{}), make(chan struct{})
	dialed := make(chan string, 1)
	application := newTestEngine(t, Config{Receive: ReceiveDependencies{
		ReceiverDial: func(_ context.Context, config ReceiveDialRequest) (ReceiveConnection, error) {
			dialed <- config.RelayBaseURL
			return nil, relayset.ErrReceiverAuthentication
		},
	}})
	current, err := application.StartReceive(context.Background(), ReceiveRequest{
		Capability: capability, Connectivity: ConnectivityRelayOnly,
		Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) {
			close(outputEntered)
			<-releaseOutput
			return admissionOutput{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	<-outputEntered
	capability.ReadSecret[0] ^= 0xff
	capability.PKHash[0] ^= 0xff
	capability.Relays[0] = "ws://modified.example:8080"
	close(releaseOutput)
	if _, err := current.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-dialed:
		if got != originalRelay {
			t.Fatalf("task used mutated caller relay %q", got)
		}
	default:
		t.Fatal("receiver did not reach injected dial")
	}
}

// A malformed descriptor exercises acquired-connection disposal with no socket or concrete relay object.
func TestReceiveRejectsAndClosesInjectedConnectionBeforeChannelUse(t *testing.T) {
	capability, err := link.NewSenderAuthenticated(bytes.Repeat([]byte{1}, link.ReadSecretBytes), ed25519.PublicKey(bytes.Repeat([]byte{2}, ed25519.PublicKeySize)), []string{"https://relay.example"})
	if err != nil {
		t.Fatal(err)
	}
	connection := &facadeReceiveConnection{}
	application := newTestEngine(t, Config{Receive: ReceiveDependencies{ReceiverDial: func(context.Context, ReceiveDialRequest) (ReceiveConnection, error) { return connection, nil }}})
	current, err := application.StartReceive(t.Context(), ReceiveRequest{Capability: capability, Connectivity: ConnectivityRelayOnly, Output: OutputFactoryFunc(func(OutputConfig) (OutputAuthority, error) { return admissionOutput{}, nil })})
	if err != nil {
		t.Fatal(err)
	}
	result, err := current.Wait(t.Context())
	if err != nil || result.Outcome != OutcomeFailed || !result.Failure.Valid() || !connection.closed.Load() {
		t.Fatalf("authentication failure=%+v wait=%v closed=%v", result, err, connection.closed.Load())
	}
}

type facadeReceiveConnection struct{ closed atomic.Bool }

func (*facadeReceiveConnection) Descriptor() []byte { return []byte("invalid descriptor") }
func (*facadeReceiveConnection) Channel() protocolsession.FrameChannel {
	panic("unauthenticated channel used")
}
func (*facadeReceiveConnection) Done() <-chan struct{}   { return nil }
func (*facadeReceiveConnection) Err() error              { return nil }
func (connection *facadeReceiveConnection) Close() error { connection.closed.Store(true); return nil }
