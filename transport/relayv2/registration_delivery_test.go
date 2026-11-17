package relayv2

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestSenderRegistrationFailuresBeforeWritingProveNonDelivery(t *testing.T) {
	fixture := newClientFixture(t)
	dialFailure := errors.New("upgrade refused")
	for _, test := range []struct {
		name      string
		configure func(*SenderConfig)
		cause     error
	}{
		{"capacity", func(c *SenderConfig) { c.Dial.ReceiveCapacity.Frames = 1 }, ErrReceiveCapacity},
		{"observation capacity", func(c *SenderConfig) { c.Dial.LifecycleObservationCapacity = -1 }, ErrLifecycleObservationCapacity},
		{"heartbeat", func(c *SenderConfig) { c.Dial.Heartbeat.Interval = -time.Second }, ErrHeartbeatConfig},
		{"endpoint", func(c *SenderConfig) { c.RelayBaseURL = "invalid" }, ErrProtocol},
		{"registration", func(c *SenderConfig) { c.SenderPrivateKey = nil }, ErrProtocol},
		{"upgrade", func(*SenderConfig) {}, dialFailure},
		{"upgrade with observation", func(c *SenderConfig) { c.Dial.ObserveStage = func(DialStage) {} }, dialFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := SenderConfig{
				RelayBaseURL: "https://relay.example", Init: fixture.fresh,
				SenderPrivateKey: fixture.privateKey, Descriptor: fixture.descriptor,
				Dial: DialOptions{SocketDialer: func(context.Context, string, http.Header) (BinarySocket, error) {
					return nil, dialFailure
				}},
			}
			test.configure(&config)
			connection, err := DialSender(t.Context(), config)
			var notSent *RegistrationNotSentError
			if connection != nil || !errors.As(err, &notSent) || !errors.Is(err, test.cause) {
				t.Fatalf("dial = (%v, %v), want undelivered registration with cause %v", connection, err, test.cause)
			}
			if err.Error() == "" {
				t.Fatal("delivery error lost its diagnostic description")
			}
		})
	}
}

func TestSenderRegistrationWriteAndAcknowledgementFailuresRemainUncertain(t *testing.T) {
	fixture := newClientFixture(t)
	cause := errors.New("connection lost")
	for _, failWrite := range []bool{true, false} {
		t.Run(map[bool]string{true: "write", false: "acknowledgement"}[failWrite], func(t *testing.T) {
			socket := newScriptedSocket()
			if !failWrite {
				socket.respond(challengeFrame(t, v2.ChallengeRegister))
				socket.reads <- scriptedMessage{err: cause}
			}
			config := SenderConfig{
				RelayBaseURL: "https://relay.example", Init: fixture.fresh,
				SenderPrivateKey: fixture.privateKey, Descriptor: fixture.descriptor,
				Dial: DialOptions{SocketDialer: func(context.Context, string, http.Header) (BinarySocket, error) {
					if failWrite {
						return registrationWriteFailureSocket{BinarySocket: socket, cause: cause}, nil
					}
					return socket, nil
				}},
			}
			_, err := DialSender(t.Context(), config)
			var notSent *RegistrationNotSentError
			if !errors.Is(err, cause) || errors.As(err, &notSent) {
				t.Fatalf("registration failure = %v, want delivery uncertainty", err)
			}
		})
	}
}

type registrationWriteFailureSocket struct {
	BinarySocket
	cause error
}

func (socket registrationWriteFailureSocket) Write(context.Context, websocket.MessageType, []byte) error {
	// A transport can fail after a partial write; its error cannot prove absence.
	return socket.cause
}
