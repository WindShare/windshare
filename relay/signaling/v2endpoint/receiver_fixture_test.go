package v2endpoint

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2"
)

// Raw receiver fixtures must observe the same initial zero-credit boundary as
// real clients. Descriptor delivery alone does not authorize the first frame.
func sendInitialReceiverFrame(ctx context.Context, socket BinaryConnection, id v2.RelaySessionID, payload []byte) error {
	encoded, err := (v2.OpaqueRoute{RelaySessionID: id, Ciphertext: payload}).MarshalBinary()
	if err != nil {
		return err
	}
	var frames, bytes uint32
	for frames == 0 || bytes < uint32(len(encoded)) {
		response, err := readBinary(ctx, socket)
		if err != nil {
			return err
		}
		credit, err := v2.ParseSessionCredit(response)
		if err != nil || credit.RelaySessionID != id ||
			credit.Frames > v2.SenderWindowFrames-frames || credit.Bytes > v2.SenderWindowBytes-bytes {
			return fmt.Errorf("invalid initial receiver credit: frame=%+v error=%v", credit, err)
		}
		frames += credit.Frames
		bytes += credit.Bytes
	}
	return socket.Write(ctx, websocket.MessageBinary, encoded)
}

func dialIsolationReceiver(ctx context.Context, server *Server, shareID v2.ShareID) (*relayv2.ReceiverConnection, error) {
	return relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{
		RelayBaseURL: isolationRelayBase, ShareID: shareID,
		Dial: relayv2.DialOptions{SocketDialer: isolationServerDialer(ctx, server)},
	})
}

func isolationServerDialer(ctx context.Context, server *Server) func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
	return func(context.Context, string, http.Header) (relayv2.BinarySocket, error) {
		client, relay := newMemorySocketPair()
		go func() { _ = server.Serve(ctx, relay) }()
		return client, nil
	}
}

func establishIsolationSession(ctx context.Context, sender *relayv2.SenderConnection, receiver *relayv2.ReceiverConnection, first []byte) (*relayv2.Channel, error) {
	// Send settles physical exposure independently of the sender application
	// accepting its channel. A rejected hello must not leave Accept waiting.
	if err := receiver.Channel().Send(ctx, first); err != nil {
		return nil, fmt.Errorf("send initial receiver frame: %w", err)
	}
	return sender.Accept(ctx)
}

func TestIsolationSessionReturnsFirstSendFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), isolationScenarioTimeout)
		defer cancel()
		server, sender, fixture := isolationServer(t)
		receiver, err := dialIsolationReceiver(ctx, server, fixture.init.ShareID)
		if err != nil {
			t.Fatal(err)
		}
		_ = receiver.Close()
		started := time.Now()
		channel, err := establishIsolationSession(ctx, sender, receiver, []byte("rejected hello"))
		if channel != nil || !errors.Is(err, relayv2.ErrClosed) || time.Since(started) != 0 {
			t.Fatalf("closed receiver setup: channel=%v error=%v elapsed=%v", channel, err, time.Since(started))
		}
	})
}

type discardInitialReceiverSocket struct{ relayv2.BinarySocket }

func (socket discardInitialReceiverSocket) Write(ctx context.Context, kind websocket.MessageType, frame []byte) error {
	if len(frame) >= 4 && string(frame[:4]) == "WS2O" {
		return nil
	}
	return socket.BinarySocket.Write(ctx, kind, frame)
}

func TestIsolationSessionAcceptHonorsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), isolationScenarioTimeout)
		defer cancel()
		server, sender, fixture := isolationServer(t)
		dial := isolationServerDialer(ctx, server)
		receiver, err := relayv2.DialReceiver(ctx, relayv2.ReceiverConfig{
			RelayBaseURL: isolationRelayBase, ShareID: fixture.init.ShareID,
			Dial: relayv2.DialOptions{SocketDialer: func(ctx context.Context, url string, headers http.Header) (relayv2.BinarySocket, error) {
				socket, err := dial(ctx, url, headers)
				return discardInitialReceiverSocket{socket}, err
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer receiver.Close()
		attemptContext, cancelAttempt := context.WithTimeout(ctx, time.Second)
		defer cancelAttempt()
		started := time.Now()
		channel, err := establishIsolationSession(attemptContext, sender, receiver, []byte("unrouted hello"))
		if channel != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != time.Second {
			t.Fatalf("unrouted receiver setup: channel=%v error=%v elapsed=%v", channel, err, time.Since(started))
		}
		select {
		case <-sender.Done():
			t.Fatal("receiver setup cancellation closed the healthy sender")
		default:
		}
	})
}

func TestRawReceiverWaitsForCompleteInitialCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, relay := newMemorySocketPair()
		ctx, cancel := context.WithTimeout(t.Context(), isolationScenarioTimeout)
		defer cancel()
		var sessionID v2.RelaySessionID
		sessionID[0] = 1
		payload := []byte("hello")
		done := make(chan error, 1)
		go func() { done <- sendInitialReceiverFrame(ctx, client, sessionID, payload) }()
		assertNoFrame := func() {
			t.Helper()
			synctest.Wait()
			select {
			case frame := <-relay.inbound.messages:
				t.Fatalf("receiver sent without complete credit: %x", frame.data)
			default:
			}
		}
		assertNoFrame()
		grant, _ := (v2.SessionCredit{RelaySessionID: sessionID, Frames: 1}).MarshalBinary()
		if err := relay.Write(ctx, websocket.MessageBinary, grant); err != nil {
			t.Fatal(err)
		}
		assertNoFrame()
		grant, _ = (v2.SessionCredit{RelaySessionID: sessionID, Bytes: uint32(len(payload))}).MarshalBinary()
		if err := relay.Write(ctx, websocket.MessageBinary, grant); err != nil {
			t.Fatal(err)
		}
		assertNoFrame()
		grant, _ = (v2.SessionCredit{RelaySessionID: sessionID, Bytes: v2.OpaqueRouteHeaderBytes}).MarshalBinary()
		if err := relay.Write(ctx, websocket.MessageBinary, grant); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		encoded, err := readBinary(ctx, relay)
		if err != nil {
			t.Fatal(err)
		}
		frame, err := v2.ParseOpaqueRoute(encoded)
		if err != nil || frame.RelaySessionID != sessionID || string(frame.Ciphertext) != string(payload) {
			t.Fatalf("initial receiver frame=%+v error=%v", frame, err)
		}
	})
}
