package relayv2

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestReceiverReturnsOnlyConsumedWireBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := newLink(t.Context(), socket, true)
		defer link.stop(nil)
		channel := link.installFixed(relaySessionID(81))
		defer channel.Close()
		link.start()
		initial, err := v2.ParseReceiveCredit(assertWriteMagic(t, socket, v2.ReceiveCreditMagic))
		if err != nil || initial.Frames != v2.ReceiveWindowFrames || initial.Bytes != v2.ReceiveWindowBytes {
			t.Fatal("initial receive capacity", initial, err)
		}
		var wireBytes uint32
		for index := range v2.ReceiveCreditBatchFrames {
			payload := make([]byte, index+1)
			wireBytes += uint32(len(payload) + v2.OpaqueRouteHeaderBytes)
			wire, _ := (v2.OpaqueRoute{RelaySessionID: channel.id, Ciphertext: payload}).MarshalBinary()
			socket.respond(wire)
		}
		synctest.Wait()
		if len(socket.writes) != 0 {
			t.Fatal("arrival returned storage still owned by the receive queue")
		}
		for range v2.ReceiveCreditBatchFrames {
			<-channel.Recv()
		}
		synctest.Wait()
		returned, err := v2.ParseReceiveCredit(assertWriteMagic(t, socket, v2.ReceiveCreditMagic))
		if err != nil || returned.Frames != v2.ReceiveCreditBatchFrames || returned.Bytes != wireBytes {
			t.Fatal("consumed credit", returned, err)
		}
	})
}

func TestReceiverRetirementDrainsAndOwnerCloseReleasesAnIdleConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, consume := range []bool{true, false} {
			link := newLink(context.Background(), newScriptedSocket(), true)
			channel := link.installFixed(relaySessionID(82))
			for index := range v2.ReceiveCreditBatchFrames {
				if !channel.deliver([]byte{byte(index)}) {
					t.Fatal("receive reservation rejected")
				}
			}
			link.retire(channel.id)
			if consume {
				for index := range v2.ReceiveCreditBatchFrames {
					if frame := <-channel.Recv(); len(frame) != 1 || frame[0] != byte(index) {
						t.Fatal("retirement discarded accepted data", frame)
					}
				}
				if _, open := <-channel.Recv(); open {
					t.Fatal("retired stream remained open")
				}
			}
			connection := &ReceiverConnection{link: link, channel: channel}
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			select {
			case <-channel.receiveDone:
			default:
				t.Fatal("receiver owner close retained its delivery worker")
			}
			if channel.State() != framechannel.Closed || len(link.receiveCredits) != 0 {
				t.Fatal("retirement retained credits")
			}
		}
	})
}

func TestReceiveCreditCannotRecreateAClosedLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		link := newLink(t.Context(), newScriptedSocket(), true)
		channel := link.installFixed(relaySessionID(83))
		_ = channel.Close()
		link.returnReceiveCredit(channel.id, 100)
		if encoded, err := link.takeReceiveCredit(); encoded != nil || err != nil {
			t.Fatal("closed channel returned credit", encoded, err)
		}
		link.stop(nil)
		late := link.installFixed(relaySessionID(84))
		synctest.Wait()
		select {
		case <-late.receiveDone:
		default:
			t.Fatal("late channel started a retained worker")
		}
	})
}
