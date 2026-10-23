package engine

import (
	"context"
	"time"

	"github.com/windshare/windshare/connectivity/relayset"
	"github.com/windshare/windshare/core/session/protocolsession"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

// ReceiveConnection transfers one authenticated relay connection's ownership to the task.
type ReceiveConnection interface {
	Descriptor() []byte
	Channel() protocolsession.FrameChannel
	Done() <-chan struct{}
	Err() error
	Close() error
}
type ReceiveDialRequest struct {
	RelayBaseURL string
	ShareID      []byte
	Diagnostics  bool
}
type RecoveryClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}
type ReceiveRecoverySettings struct {
	Clock          RecoveryClock
	TimeoutContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	InitialWait    time.Duration
	FastWindow     time.Duration
	WaitTimeout    time.Duration
	Jitter         func(time.Duration) time.Duration
}

func (settings ReceiveRecoverySettings) native() relayset.ReceiverRecoveryOptions {
	return relayset.ReceiverRecoveryOptions{Clock: settings.Clock, TimeoutContext: settings.TimeoutContext, InitialWait: settings.InitialWait, FastWindow: settings.FastWindow, WaitTimeout: settings.WaitTimeout, Jitter: settings.Jitter}
}

type injectedReceiveConnection struct {
	ReceiveConnection
	endpoint v2.RelayEndpoint
}

func (connection injectedReceiveConnection) Endpoint() v2.RelayEndpoint { return connection.endpoint }
func receiveDial(dial func(context.Context, ReceiveDialRequest) (ReceiveConnection, error)) func(context.Context, relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
	if dial == nil {
		return nil
	}
	return func(ctx context.Context, config relayv2.ReceiverConfig) (relayset.ReceiverConnection, error) {
		endpoint, err := v2.RelayEndpointForShare(config.RelayBaseURL, config.ShareID)
		if err != nil {
			return nil, err
		}
		connection, err := dial(ctx, ReceiveDialRequest{RelayBaseURL: config.RelayBaseURL, ShareID: append([]byte(nil), config.ShareID[:]...), Diagnostics: config.Dial.LifecycleObservationCapacity > 0})
		if connection == nil {
			return nil, err
		}
		return injectedReceiveConnection{ReceiveConnection: connection, endpoint: endpoint}, err
	}
}
