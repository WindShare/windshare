package nativepeer

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestOwnedSocketHandoffObservationsPreservePathAttribution(t *testing.T) {
	n := New(Config{Side: SideSender, ObservationCapacity: DefaultObservationCapacity})
	t.Cleanup(func() { _ = n.Close(context.Background()) })
	lease, err := n.config.Sockets.Acquire([16]byte{1}, 3, [16]byte{2}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, kind := range []socketauthority.EventKind{socketauthority.SocketHandoffStarted, socketauthority.SocketHandoffFinished} {
		select {
		case event := <-n.Observations():
			if event.Socket == nil || event.Socket.Kind != kind || event.Subject.ProtocolSessionID != lease.SessionID() ||
				event.Subject.PeerPathID != lease.PathID() || event.Subject.NetworkGenerationID != 3 || event.Subject.Side != SideSender ||
				event.Subject.AttemptID != ([16]byte{}) || event.Subject.AttemptSequence != 0 {
				t.Fatalf("incorrect socket attribution: %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("socket event did not reach the native observation stream")
		}
	}
}
