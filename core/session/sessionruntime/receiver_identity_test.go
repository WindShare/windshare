package sessionruntime

import (
	"context"
	"crypto/ecdh"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestReceiverHandshakeValidationUsesTerminalProtocolBoundary(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
	}{
		{"malformed", protocolsession.ErrServerHelloMalformed},
		{"wire_version", protocolsession.ErrUnsupportedVersion},
		{"key_agreement", protocolsession.ErrKeyAgreement},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newVerticalFixture(t)
			server, client := newMemoryChannelPair()
			defer server.Close()
			defer client.Close()
			served := make(chan error, 1)
			go func() {
				serve := func() error {
					encoded := <-server.Recv()
					if test.name == "malformed" {
						return server.Send(t.Context(), framechannel.Frame{1})
					}
					sender := fixture.senderFactory
					hello, err := sender.replay.AcceptClientHello(encoded, sender.share, sender.authKey)
					if err != nil {
						return err
					}
					// X25519 accepts this encoded public key but rejects it during
					// agreement, after the sender's signature has authenticated it.
					lowOrder, err := ecdh.X25519().NewPublicKey(make([]byte, protocolsession.X25519KeyBytes))
					if err != nil {
						return err
					}
					response, err := protocolsession.NewServerHello(hello, make([]byte, protocolsession.HandshakeNonceBytes), lowOrder, 1, sender.privateKey)
					if err != nil {
						return err
					}
					payload := response.Encoded()
					if test.name == "wire_version" {
						const wireVersionOffset = 4
						payload[wireVersionOffset]++
					}
					return server.Send(t.Context(), payload)
				}
				served <- serve()
			}()
			_, err := fixture.receiverFactory.Connect(t.Context(), client, transfer.LaneRouteRelay)
			if serveErr := <-served; serveErr != nil {
				t.Fatal(serveErr)
			}
			var boundary *transferfault.BoundaryError
			if !errors.Is(err, test.cause) || !errors.As(err, &boundary) {
				t.Fatal(err)
			}
			if code, ok := boundary.Fault().SessionCode(); !ok || code != transferfault.SessionProtocol {
				t.Fatal(boundary.Fault())
			}
		})
	}
}

func TestReceiverIdentityRejectionPreservesProtocolFailureThroughCleanup(t *testing.T) {
	fixture := newVerticalFixture(t)
	close(fixture.scanGate)
	sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
	defer sender.Close()
	defer receiver.Close()
	dependencies, err := receiver.TransferDependencies()
	if err != nil {
		t.Fatal(err)
	}
	identityFailure := errors.New("authenticated share descriptor changed")
	receiver.RejectShareIdentity(identityFailure)
	receiver.WaitClosed()
	if receiver.PathsExhausted() || !errors.Is(receiver.Err(), identityFailure) {
		t.Fatal(receiver.Err())
	}
	for _, operation := range []func() error{
		func() error { return dependencies.classifyAccess(context.Background(), context.Canceled) },
		func() error { return dependencies.classifySource(context.Background(), transfer.ErrBrokerClosed) },
		func() error { return dependencies.classifyCatalog(context.Background(), ErrRuntimeClosed) },
		func() error {
			return dependencies.classifySource(context.Background(), sessionTransportBoundaryError(transfer.ErrLaneClosed))
		},
	} {
		err := operation()
		var boundary *transferfault.BoundaryError
		if !errors.Is(err, identityFailure) || !errors.As(err, &boundary) || boundary.Fault().Domain() != transferfault.DomainSession {
			t.Fatal(err)
		}
		code, ok := boundary.Fault().SessionCode()
		if !ok || code != transferfault.SessionProtocol {
			t.Fatal(boundary.Fault())
		}
	}
	local := sourceBoundaryError(transferfault.SourceRevisionChanged, content.ErrSourceDrift)
	if got := dependencies.classifySource(context.Background(), local); got != local {
		t.Fatal("file failure lost its scope", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := dependencies.classifySource(ctx, transfer.ErrBrokerClosed); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var absent *ReceiverRuntime
	absent.RejectShareIdentity(identityFailure)
	(&ReceiverRuntime{}).RejectShareIdentity(identityFailure)
	receiver.RejectShareIdentity(nil)
}
