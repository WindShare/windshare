package connectivity

import (
	"context"
	"encoding/json"
	"fmt"

	transportrelay "github.com/windshare/windshare/transport/relay"
)

// Signal is one session-scoped WebRTC negotiation message. Session identity is
// deliberately absent: the Signaling instance already represents exactly one
// receiver, preventing callers from routing an offer to a sibling by mistake.
type Signal struct {
	Kind    string
	Payload json.RawMessage
}

// Signaling is defined where negotiation consumes it, so tests and future relay
// implementations need only provide the two semantic operations used here.
type Signaling interface {
	Send(context.Context, Signal) error
	Receive(context.Context) (Signal, error)
}

// RelaySignalChannel is the signaling projection exposed by a relay session.
type RelaySignalChannel interface {
	SendSignal(context.Context, string, json.RawMessage) error
	Signals() <-chan transportrelay.Signal
}

// RelaySignaling adapts a relay session's control lane without copying any
// fallback or WebRTC policy into the relay transport.
type RelaySignaling struct {
	channel RelaySignalChannel
}

func NewRelaySignaling(channel RelaySignalChannel) (*RelaySignaling, error) {
	if channel == nil {
		return nil, fmt.Errorf("%w: relay signaling channel", ErrNilDependency)
	}
	return &RelaySignaling{channel: channel}, nil
}

func (s *RelaySignaling) Send(ctx context.Context, signal Signal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if signal.Kind == "" || len(signal.Payload) == 0 || !json.Valid(signal.Payload) {
		return fmt.Errorf("%w: outbound signal kind or JSON payload", ErrInvalidSignal)
	}
	return s.channel.SendSignal(ctx, signal.Kind, append(json.RawMessage(nil), signal.Payload...))
}

func (s *RelaySignaling) Receive(ctx context.Context) (Signal, error) {
	if err := ctx.Err(); err != nil {
		return Signal{}, err
	}
	select {
	case <-ctx.Done():
		return Signal{}, ctx.Err()
	case signal, ok := <-s.channel.Signals():
		if !ok {
			return Signal{}, ErrSignalingClosed
		}
		if signal.Kind == "" || len(signal.Payload) == 0 || !json.Valid(signal.Payload) {
			return Signal{}, fmt.Errorf("%w: inbound signal kind or JSON payload", ErrInvalidSignal)
		}
		return Signal{
			Kind:    signal.Kind,
			Payload: append(json.RawMessage(nil), signal.Payload...),
		}, nil
	}
}
