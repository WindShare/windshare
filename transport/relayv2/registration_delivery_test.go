package relayv2

import (
	"context"
	"crypto/ed25519"
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
						return writeFailureSocket{BinarySocket: socket, cause: cause}, nil
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

func TestSenderRegistrationKeepsValidationFailuresDistinctFromSocketFailures(t *testing.T) {
	fixture := newClientFixture(t)
	for _, tt := range []struct {
		name     string
		purpose  v2.ChallengePurpose
		key      ed25519.PrivateKey
		cause    error
		protocol bool
	}{
		{"wrong challenge purpose", v2.ChallengeStop, fixture.privateKey, v2.ErrPurpose, true},
		{"wrong sender key", v2.ChallengeRegister, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), v2.ErrProof, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			socket := newScriptedSocket()
			socket.respond(challengeFrame(t, tt.purpose))
			_, err := DialSender(t.Context(), SenderConfig{
				RelayBaseURL:     "https://relay.example",
				Init:             fixture.fresh,
				SenderPrivateKey: tt.key,
				Descriptor:       fixture.descriptor,
				Dial:             DialOptions{SocketDialer: socket.dial},
			})
			if !errors.Is(err, tt.cause) || errors.Is(err, ErrProtocol) != tt.protocol || errors.Is(err, ErrClosed) {
				t.Fatalf("registration error = %v, want cause %v, protocol=%t", err, tt.cause, tt.protocol)
			}
		})
	}
}

type writeFailureSocket struct {
	BinarySocket
	cause error
}

func (socket writeFailureSocket) Write(context.Context, websocket.MessageType, []byte) error {
	return socket.cause
}
