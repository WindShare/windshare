package connectivity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	transportrelay "github.com/windshare/windshare/transport/relay"
)

type fakeRelaySignalChannel struct {
	received chan transportrelay.Signal
	sent     chan transportrelay.Signal
}

func newFakeRelaySignalChannel() *fakeRelaySignalChannel {
	return &fakeRelaySignalChannel{
		received: make(chan transportrelay.Signal, 2),
		sent:     make(chan transportrelay.Signal, 2),
	}
}

func (c *fakeRelaySignalChannel) SendSignal(_ context.Context, kind string, payload json.RawMessage) error {
	c.sent <- transportrelay.Signal{Kind: kind, Payload: append(json.RawMessage(nil), payload...)}
	return nil
}

func (c *fakeRelaySignalChannel) Signals() <-chan transportrelay.Signal { return c.received }

func TestRelaySignalingCopiesPayloadsAndPreservesScope(t *testing.T) {
	channel := newFakeRelaySignalChannel()
	signaling, err := NewRelaySignaling(channel)
	if err != nil {
		t.Fatal(err)
	}

	outbound := json.RawMessage(`{"sdp":"offer"}`)
	if err := signaling.Send(t.Context(), Signal{Kind: "offer", Payload: outbound}); err != nil {
		t.Fatal(err)
	}
	outbound[8] = 'X'
	sent := <-channel.sent
	if string(sent.Payload) != `{"sdp":"offer"}` {
		t.Fatalf("sent payload = %s", sent.Payload)
	}

	inbound := json.RawMessage(`{"candidate":"one"}`)
	channel.received <- transportrelay.Signal{Kind: "candidate", Payload: inbound}
	got, err := signaling.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inbound[14] = 'X'
	if got.Kind != "candidate" || string(got.Payload) != `{"candidate":"one"}` {
		t.Fatalf("received signal = %#v", got)
	}
}

func TestRelaySignalingRejectsInvalidAndClosedInput(t *testing.T) {
	if _, err := NewRelaySignaling(nil); !errors.Is(err, ErrNilDependency) {
		t.Fatalf("nil channel error = %v", err)
	}
	channel := newFakeRelaySignalChannel()
	signaling, _ := NewRelaySignaling(channel)
	if err := signaling.Send(t.Context(), Signal{Kind: "offer", Payload: json.RawMessage(`nope`)}); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("invalid send error = %v", err)
	}
	channel.received <- transportrelay.Signal{Kind: "", Payload: json.RawMessage(`{}`)}
	if _, err := signaling.Receive(t.Context()); !errors.Is(err, ErrInvalidSignal) {
		t.Fatalf("invalid receive error = %v", err)
	}
	close(channel.received)
	if _, err := signaling.Receive(t.Context()); !errors.Is(err, ErrSignalingClosed) {
		t.Fatalf("closed receive error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := signaling.Receive(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled receive error = %v", err)
	}
}
