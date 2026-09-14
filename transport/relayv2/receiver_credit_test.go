package relayv2

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestReceiverWaitsForBothCreditDimensionsAndPreservesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := newLink(t.Context(), socket, true)
		defer link.stop(nil)
		channel := link.installFixed(relaySessionID(71))
		link.start()
		assertWriteMagic(t, socket, v2.ReceiveCreditMagic)
		payload := framechannel.Frame("client hello")
		result := make(chan error, 1)
		go func() { result <- channel.Send(t.Context(), payload) }()
		synctest.Wait()
		if len(socket.writes) != 0 {
			t.Fatal("receiver sent before explicit initial grant")
		}
		grantReceiverCredit(t, socket, channel.id, 1, 0)
		synctest.Wait()
		if len(socket.writes) != 0 {
			t.Fatal("frame credit bypassed byte budget")
		}
		grantReceiverCredit(t, socket, channel.id, 0, uint32(len(payload)+v2.OpaqueRouteHeaderBytes))
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		assertOpaqueWrite(t, socket, channel.id, string(payload))
		ctx, cancel := context.WithCancel(t.Context())
		go func() { result <- channel.Send(ctx, payload) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatal("unexposed credit wait did not cancel", err)
		}
		grantReceiverCredit(t, socket, channel.id, 1, uint32(len(payload)+v2.OpaqueRouteHeaderBytes))
		if err := channel.Send(t.Context(), payload); err != nil {
			t.Fatal(err)
		}
		assertOpaqueWrite(t, socket, channel.id, string(payload))
		go func() { result <- channel.Send(t.Context(), payload) }()
		synctest.Wait()
		link.stop(ErrClosed)
		synctest.Wait()
		if err := <-result; err == nil {
			t.Fatal("close left credit-blocked send successful")
		}
	})
}

func TestReceiverHeartbeatRunsWhileApplicationCreditIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := &silentHeartbeatSocket{scriptedSocket: newScriptedSocket()}
		link := newLink(t.Context(), socket, true)
		channel := link.installFixed(relaySessionID(72))
		link.heartbeat = HeartbeatConfig{Interval: time.Second, Timeout: 3 * time.Second}
		link.start()
		assertWriteMagic(t, socket.scriptedSocket, v2.ReceiveCreditMagic)
		result := make(chan error, 1)
		go func() { result <- channel.Send(t.Context(), []byte("waiting for capacity")) }()
		synctest.Wait()
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if !errors.Is(link.Err(), ErrHeartbeat) {
			t.Fatal("credit wait hid silent socket", link.Err())
		}
		if err := <-result; err == nil {
			t.Fatal("heartbeat did not settle pending send")
		}
		if len(socket.writes) != 0 {
			t.Fatal("credit wait leaked application bytes")
		}
	})
}

func grantReceiverCredit(t *testing.T, socket *scriptedSocket, id v2.RelaySessionID, frames, bytes uint32) {
	t.Helper()
	encoded, err := (v2.SessionCredit{RelaySessionID: id, Frames: frames, Bytes: bytes}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	socket.respond(encoded)
}
