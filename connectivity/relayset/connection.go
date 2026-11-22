package relayset

import (
	"context"

	"github.com/windshare/windshare/core/session/protocolsession"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

// ReceiverConnection is the authority needed to attach and retire one relay lane.
type ReceiverConnection interface {
	Descriptor() []byte
	Channel() protocolsession.FrameChannel
	Endpoint() v2.RelayEndpoint
	Done() <-chan struct{}
	Err() error
	Close() error
}

type nativeReceiverConnection struct{ *relayv2.ReceiverConnection }

func (connection nativeReceiverConnection) Channel() protocolsession.FrameChannel {
	return connection.ReceiverConnection.Channel()
}

func DialReceiver(ctx context.Context, config relayv2.ReceiverConfig) (ReceiverConnection, error) {
	connection, err := relayv2.DialReceiver(ctx, config)
	if connection == nil {
		return nil, err
	}
	return nativeReceiverConnection{connection}, err
}

// SenderTransport is defined at the lifecycle that consumes it so recovery
// policy does not depend on the concrete WebSocket transport.
type SenderTransport interface {
	Accept(context.Context) (*relayv2.Channel, error)
	Close() error
}

type senderRelayObservationCompleter interface {
	LifecycleTrace() <-chan relayv2.LifecycleTrace
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

// SenderConnection keeps factories concrete while containing the narrow
// transport interface at the consumer boundary.
type SenderConnection struct {
	endpoint           SenderTransport
	finishObservations func()
}

func NewSenderConnection(endpoint SenderTransport) SenderConnection {
	return SenderConnection{endpoint: endpoint}
}

func (connection SenderConnection) valid() bool {
	return connection.endpoint != nil
}

func (connection SenderConnection) Accept(ctx context.Context) (*relayv2.Channel, error) {
	return connection.endpoint.Accept(ctx)
}

func (connection SenderConnection) Close() error {
	if !connection.valid() {
		return nil
	}
	return connection.endpoint.Close()
}

func (connection SenderConnection) LifecycleTrace() <-chan relayv2.LifecycleTrace {
	if !connection.valid() {
		return nil
	}
	observer, ok := connection.endpoint.(senderRelayObservationCompleter)
	if !ok {
		return nil
	}
	return observer.LifecycleTrace()
}

func (connection SenderConnection) CompleteObservations() relayv2.LifecycleObservationCompletion {
	if !connection.valid() {
		return relayv2.LifecycleObservationCompletion{}
	}
	completer, ok := connection.endpoint.(senderRelayObservationCompleter)
	if !ok {
		return relayv2.LifecycleObservationCompletion{}
	}
	return completer.CompleteObservations()
}

// SenderDialer owns the complete attempt outcome. RegistrationNotSentError proves no
// registration was sent by any internal attempt; observations never own policy.
type SenderDialer interface {
	Dial(context.Context, relayv2.SenderConfig) (SenderConnection, error)
}

type relayV2SenderDialer struct{}

func (relayV2SenderDialer) Dial(
	ctx context.Context,
	config relayv2.SenderConfig,
) (SenderConnection, error) {
	connection, err := relayv2.DialSender(ctx, config)
	if err != nil {
		return SenderConnection{}, err
	}
	return NewSenderConnection(connection), nil
}
