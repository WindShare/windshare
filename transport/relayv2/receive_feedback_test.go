package relayv2

import (
	"testing"
	"testing/synctest"
	"time"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

// A declared connection capacity says nothing about the relay's current per-flow
// allowance. Even a single consumed envelope can be its last available credit.
func TestDefaultReceiveCapacityReturnsSparseCreditWithoutTimer(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			socket := newScriptedSocket()
			link := newTestLink(t.Context(), socket, fixed)
			defer link.stop(nil)
			channel := link.installFixed(relaySessionID(89))
			link.start()
			started := time.Now()
			for ordinal := uint64(1); ordinal <= 17; ordinal++ {
				respondOpaque(t, socket, channel.id, []byte{byte(ordinal)})
				synctest.Wait()
				if len(socket.writes) != 0 {
					t.Fatal("arrival refunded storage before application handoff")
				}
				<-channel.Recv()
				synctest.Wait()
				if len(socket.writes) == 0 {
					t.Fatal("consumed credit waits for another frame or timer")
				}
				feedback, err := v2.ParseDeliveryConsumed(assertWriteMagic(t, socket, v2.DeliveryConsumedMagic))
				if err != nil || feedback.TotalFrames != ordinal ||
					feedback.TotalEncodedBytes != ordinal*(v2.OpaqueRouteHeaderBytes+1) {
					t.Fatal("incorrect cumulative consumption", feedback, err)
				}
			}
			if !time.Now().Equal(started) {
				t.Fatal("feedback required advancing the clock")
			}
		})
	}
}

func TestFeedbackCoalescesBehindBlockedWriterAndAllowsReverseData(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const consumedFrames = uint64(17)
		socket := newScriptedSocket()
		// Holding the first physical write lets consumption and a reverse send
		// accumulate without depending on goroutine scheduling or wall time.
		socket.writes = make(chan scriptedMessage)
		link := newTestLink(t.Context(), socket, false)
		defer link.stop(nil)
		channel := installGrantedChannel(link, relaySessionID(90))
		link.start()
		respondOpaque(t, socket, channel.id, []byte{1})
		<-channel.Recv()
		synctest.Wait()
		for ordinal := uint64(2); ordinal <= consumedFrames; ordinal++ {
			respondOpaque(t, socket, channel.id, []byte{byte(ordinal)})
			<-channel.Recv()
		}
		sent := make(chan error, 1)
		go func() { sent <- channel.Send(t.Context(), []byte("reverse")) }()
		synctest.Wait()

		first, err := v2.ParseDeliveryConsumed(assertWriteMagic(t, socket, v2.DeliveryConsumedMagic))
		if err != nil || first.TotalFrames != 1 {
			t.Fatal("initial writer handoff changed", first, err)
		}
		assertOpaqueWrite(t, socket, channel.id, "reverse")
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
		coalesced, err := v2.ParseDeliveryConsumed(assertWriteMagic(t, socket, v2.DeliveryConsumedMagic))
		if err != nil || coalesced.TotalFrames != consumedFrames ||
			coalesced.TotalEncodedBytes != consumedFrames*(v2.OpaqueRouteHeaderBytes+1) {
			t.Fatal("blocked writer did not coalesce pending consumption", coalesced, err)
		}
	})
}
