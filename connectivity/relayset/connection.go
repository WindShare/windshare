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
