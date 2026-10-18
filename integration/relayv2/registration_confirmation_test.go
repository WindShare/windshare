package relayv2_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/windshare/windshare/internal/testrun"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

// Losing the confirmation must not strand committed ownership behind the live
// original socket. This exercises the native replacement for the old Go host.
func TestLostFreshConfirmationRecoversWhileOriginalSocketIsStillLive(t *testing.T) {
	ctx, trace, urls := startRelaysetScenario(t, 1)
	fixture, err := newRelayProtocolFixture()
	if err != nil {
		t.Fatal(err)
	}
	config := relayv2.SenderConfig{
		RelayBaseURL: urls[0], Init: fixture.init, SenderPrivateKey: fixture.privateKey,
		Descriptor: fixture.descriptor, ResumeToken: fixture.resumeToken,
	}
	dropped := make(chan struct{})
	freshConfig := config
	freshConfig.Dial.SocketDialer = func(ctx context.Context, target string, headers http.Header) (relayv2.BinarySocket, error) {
		// coder/websocket owns the handshake response body on every outcome.
		socket, _, err := websocket.Dial(ctx, target, &websocket.DialOptions{HTTPHeader: headers}) //nolint:bodyclose
		if err != nil {
			return nil, err
		}
		return &dropRegistrationConfirmation{BinarySocket: socket, dropped: dropped}, nil
	}
	freshContext, cancel := context.WithCancel(ctx)
	freshDone := make(chan error, 1)
	freshStopped := make(chan struct{})
	go func() {
		defer close(freshStopped)
		sender, err := relayv2.DialSender(freshContext, freshConfig)
		if sender != nil {
			_ = sender.Close()
		}
		freshDone <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-freshStopped:
		case <-time.After(relayCleanupTimeout):
			t.Error("unconfirmed registration did not release its socket")
		}
	})
	select {
	case <-dropped:
		trace.RequireRecord(t, "registration_confirmation_dropped", testrun.OutcomeSucceeded, nil)
	case err := <-freshDone:
		t.Fatalf("registration ended before the confirmation could be dropped: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	config.Init, err = relayv2.ResumeInit(config.Init)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := relayv2.DialSender(ctx, config)
	if err != nil {
		t.Fatal("resume committed registration:", err)
	}
	defer replacement.Close()
	select {
	case err := <-freshDone:
		if err == nil {
			t.Fatal("unconfirmed registration falsely succeeded")
		}
	case <-ctx.Done():
		t.Fatal("replacement did not retire the unconfirmed socket:", ctx.Err())
	}
	trace.RequireRecord(t, "unconfirmed_owner_replaced", testrun.OutcomeSucceeded, nil)
	receiver, err := relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{RelayBaseURL: urls[0], ShareID: fixture.init.ShareID})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	if err := exchangeRelayFrames(ctx, replacement, receiver, receiver.Channel().RelaySessionID(),
		[]byte(receiverToSenderFrameText), []byte(senderToReceiverFrameText)); err != nil {
		t.Fatal(err)
	}
	trace.RequireSuccess(t)
}

type dropRegistrationConfirmation struct {
	relayv2.BinarySocket
	dropped chan struct{}
}

func (socket *dropRegistrationConfirmation) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	kind, frame, err := socket.BinarySocket.Read(ctx)
	if err == nil && kind == websocket.MessageBinary {
		if _, parseErr := v2.ParseRegistered(frame); parseErr == nil {
			close(socket.dropped)
			return socket.BinarySocket.Read(ctx)
		}
	}
	return kind, frame, err
}
