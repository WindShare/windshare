package senderrelay

import (
	"context"
	"github.com/windshare/windshare/transport/relayv2"
)

// Endpoint is defined at the lifecycle that consumes it so recovery
// policy does not depend on the concrete WebSocket transport.
type Endpoint interface {
	Accept(context.Context) (*relayv2.Channel, error)
	Close() error
}

type senderRelayObservationCompleter interface {
	LifecycleTrace() <-chan relayv2.LifecycleTrace
	CompleteObservations() relayv2.LifecycleObservationCompletion
}

// Connection keeps factories concrete while containing the narrow
// transport interface at the consumer boundary.
type Connection struct {
	endpoint           Endpoint
	finishObservations func()
}

func NewConnection(endpoint Endpoint) Connection {
	return Connection{endpoint: endpoint}
}

func (connection Connection) valid() bool {
	return connection.endpoint != nil
}

func (connection Connection) Accept(ctx context.Context) (*relayv2.Channel, error) {
	return connection.endpoint.Accept(ctx)
}

func (connection Connection) Close() error {
	if !connection.valid() {
		return nil
	}
	return connection.endpoint.Close()
}

func (connection Connection) LifecycleTrace() <-chan relayv2.LifecycleTrace {
	if !connection.valid() {
		return nil
	}
	observer, ok := connection.endpoint.(senderRelayObservationCompleter)
	if !ok {
		return nil
	}
	return observer.LifecycleTrace()
}

func (connection Connection) CompleteObservations() relayv2.LifecycleObservationCompletion {
	if !connection.valid() {
		return relayv2.LifecycleObservationCompletion{}
	}
	completer, ok := connection.endpoint.(senderRelayObservationCompleter)
	if !ok {
		return relayv2.LifecycleObservationCompletion{}
	}
	return completer.CompleteObservations()
}

type Dialer interface {
	Dial(context.Context, relayv2.SenderConfig) (Connection, error)
}

type relayV2SenderDialer struct{}

func (relayV2SenderDialer) Dial(
	ctx context.Context,
	config relayv2.SenderConfig,
) (Connection, error) {
	connection, err := relayv2.DialSender(ctx, config)
	if err != nil {
		return Connection{}, err
	}
	return NewConnection(connection), nil
}
