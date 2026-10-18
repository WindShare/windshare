package relayv2

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2/internal/receiveledger"
)

func TestBothRolesCoalesceConsumptionUntilWriterIsAvailable(t *testing.T) {
	const burstFrames = 16
	for _, fixed := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			link := newTestLink(t.Context(), newScriptedSocket(), fixed)
			defer link.stop(nil)
			channel := link.installFixed(relaySessionID(81))
			var frames, wireBytes uint64
			// Leave the writer unavailable while the consumer drains each burst.
			// The next writer turn must report one cumulative prefix, not replay
			// a separate queued control for every handoff.
			for range 2 {
				for index := range burstFrames {
					payload := make([]byte, index+1)
					frames++
					wireBytes += uint64(len(payload) + v2.OpaqueRouteHeaderBytes)
					if err := channel.deliver(payload); err != nil {
						t.Fatal(err)
					}
				}
				if encoded, _, err := link.receives.take(); err != nil || encoded != nil {
					t.Fatal("arrival acknowledged application-owned storage", err)
				}
				for range burstFrames {
					<-channel.Recv()
				}
				synctest.Wait()
				encoded, _, err := link.receives.take()
				if err != nil {
					t.Fatal(err)
				}
				feedback, err := v2.ParseDeliveryConsumed(encoded)
				if err != nil || feedback.TotalFrames != frames || feedback.TotalEncodedBytes != wireBytes {
					t.Fatal("cumulative consumption", feedback, err)
				}
				if extra, _, err := link.receives.take(); err != nil || extra != nil {
					t.Fatal("one handoff generated duplicate controls", err)
				}
			}
		})
	}
}

func TestConnectionReceiveCapacityIsSharedAcrossSessions(t *testing.T) {
	for _, capacity := range []v2.ReceiveCapacity{
		{Frames: 4, EncodedBytes: 1024},
		{Frames: 16, EncodedBytes: 4 * (v2.OpaqueRouteHeaderBytes + 1)},
	} {
		synctest.Test(t, func(t *testing.T) {
			link, err := newLink(t.Context(), newScriptedSocket(), false, DialOptions{ReceiveCapacity: capacity})
			if err != nil {
				t.Fatal(err)
			}
			defer link.stop(nil)
			first := link.installFixed(relaySessionID(82))
			second := link.installFixed(relaySessionID(83))
			for range 2 {
				if err := first.deliver([]byte{1}); err != nil {
					t.Fatal(err)
				}
				if err := second.deliver([]byte{2}); err != nil {
					t.Fatal(err)
				}
			}
			if err := second.deliver([]byte{3}); !errors.Is(err, receiveledger.ErrCapacity) {
				t.Fatal(err)
			}
			<-first.Recv()
			synctest.Wait()
			if err := second.deliver([]byte{3}); !errors.Is(err, receiveledger.ErrCapacity) {
				t.Fatal("local handoff refunded unreported capacity", err)
			}
			feedback, _, err := link.receives.take()
			if err != nil || feedback == nil {
				t.Fatal("small window stranded consumption", err)
			}
			if err := second.deliver([]byte{3}); err != nil {
				t.Fatal("reported capacity was not reusable", err)
			}
		})
	}
}

func TestSettlementWaitsForQueueReleaseAndPreservesSibling(t *testing.T) {
	for _, discard := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			socket := newScriptedSocket()
			link := newTestLink(t.Context(), socket, false)
			defer link.stop(nil)
			channel := link.installFixed(relaySessionID(84))
			sibling := installGrantedChannel(link, relaySessionID(85))
			link.start()
			respondOpaque(t, socket, channel.id, []byte{1})
			respondOpaque(t, socket, channel.id, []byte{2})
			notice, _ := (v2.SettlementNotice{RelaySessionID: channel.id, TotalFrames: 2,
				TotalEncodedBytes: 2 * (v2.OpaqueRouteHeaderBytes + 1)}).MarshalBinary()
			socket.respond(notice)
			synctest.Wait()
			if len(socket.writes) != 0 {
				t.Fatal("notice acknowledged queued application work")
			}
			if discard {
				if err := channel.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, want := range []byte{1, 2} {
					if frame := <-channel.Recv(); len(frame) != 1 || frame[0] != want {
						t.Fatal(frame)
					}
				}
				if _, open := <-channel.Recv(); open {
					t.Fatal("retired stream remained open")
				}
			}
			synctest.Wait()
			// Handoffs can wake the writer before the retiring worker finishes.
			// Their consumed prefixes may precede, but never follow, settlement.
			var consumed uint64
			for len(socket.writes) > 1 {
				feedback, err := v2.ParseDeliveryConsumed(assertWriteMagic(t, socket, v2.DeliveryConsumedMagic))
				if err != nil || discard || feedback.RelaySessionID != channel.id ||
					feedback.TotalFrames <= consumed || feedback.TotalFrames > 2 ||
					feedback.TotalEncodedBytes != feedback.TotalFrames*(v2.OpaqueRouteHeaderBytes+1) {
					t.Fatal("invalid consumption before settlement", feedback, err)
				}
				consumed = feedback.TotalFrames
			}
			ack, err := v2.ParseSettlementAck(assertWriteMagic(t, socket, v2.SettlementAckMagic))
			if err != nil || ack.RelaySessionID != channel.id || ack.TotalFrames != 2 || ack.TotalEncodedBytes != 42 {
				t.Fatal(ack, err)
			}
			if err := sibling.Send(t.Context(), []byte("healthy")); err != nil {
				t.Fatal(err)
			}
			assertOpaqueWrite(t, socket, sibling.id, "healthy")
			socket.respond(notice)
			synctest.Wait()
			if len(socket.writes) != 0 {
				t.Fatal("duplicate recreated settled metadata")
			}
			if channel.State() != framechannel.Closed || sibling.State() != framechannel.Open {
				t.Fatal("wrong retirement scope")
			}
		})
	}
}

func TestLocalRetirementAccountsLateFramesUntilFrozenNotice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := newTestLink(t.Context(), socket, false)
		defer link.stop(nil)
		channel := link.installFixed(relaySessionID(86))
		link.start()
		respondOpaque(t, socket, channel.id, []byte{1})
		synctest.Wait()
		_ = channel.Close()
		respondOpaque(t, socket, channel.id, []byte{2})
		notice, _ := (v2.SettlementNotice{RelaySessionID: channel.id, TotalFrames: 2,
			TotalEncodedBytes: 2 * (v2.OpaqueRouteHeaderBytes + 1)}).MarshalBinary()
		socket.respond(notice)
		synctest.Wait()
		ack, err := v2.ParseSettlementAck(assertWriteMagic(t, socket, v2.SettlementAckMagic))
		if err != nil || ack.TotalFrames != 2 {
			t.Fatal(ack, err)
		}
		if len(socket.writes) != 0 {
			t.Fatal("discard was reported as consumption")
		}
		link.receives.mu.Lock()
		usage := link.receives.ledger.Usage()
		link.receives.mu.Unlock()
		if usage != (receiveledger.Usage{}) {
			t.Fatal("settlement retained resources", usage)
		}
		if err := channel.deliver([]byte{3}); !errors.Is(err, receiveledger.ErrSession) {
			t.Fatal("stale flow recreated accounting", err)
		}
	})
}

func TestReceiverOwnerCloseReleasesIdleRetiredConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		link := newTestLink(t.Context(), newScriptedSocket(), true)
		channel := link.installFixed(relaySessionID(87))
		if err := channel.deliver([]byte{1}); err != nil {
			t.Fatal(err)
		}
		link.stop(ErrClosed)
		connection := &ReceiverConnection{link: link, channel: channel}
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case <-channel.receive.done:
		default:
			t.Fatal("owner retained delivery worker")
		}
		if late := link.installFixed(relaySessionID(88)); late != nil {
			t.Fatal("closed link opened receive flow")
		}
	})
}

func respondOpaque(t *testing.T, socket *scriptedSocket, id v2.RelaySessionID, payload []byte) {
	t.Helper()
	wire, err := (v2.OpaqueRoute{RelaySessionID: id, Ciphertext: payload}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	socket.respond(wire)
}
