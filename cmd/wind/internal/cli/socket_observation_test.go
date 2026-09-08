package cli

import (
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestSocketObservationProjectsCancellationAndHandoffResults(t *testing.T) {
	local := netip.MustParseAddrPort("127.0.0.1:12345")
	server := netip.MustParseAddrPort("127.0.0.1:3478")
	at := time.Unix(100, 42)
	for _, test := range []struct {
		kind   socketauthority.EventKind
		result string
	}{
		{socketauthority.STUNRefreshFinished, "canceled"},
		{socketauthority.SocketHandoffStarted, "pending"},
		{socketauthority.SocketHandoffFinished, "retired"},
	} {
		value := nativepeer.Observation{
			Subject: nativepeer.Subject{ProtocolSessionID: [16]byte{1}, PeerPathID: [16]byte{2}, NetworkGenerationID: 3, Side: nativepeer.SideSender},
			Socket:  &socketauthority.Event{Kind: test.kind, Result: test.result, At: at, Local: local, Server: server, Duration: time.Millisecond},
		}
		event, err := projectNativeObservation(clievent.CommandShare, value)
		if err != nil {
			t.Fatal(err)
		}
		facts := event.Facts()
		if facts.Kind != string(test.kind) || facts.At != at || facts.Socket == nil || facts.Socket.Local != local ||
			facts.Socket.Server != server || facts.Socket.Result != test.result || facts.Socket.Duration != time.Millisecond ||
			facts.Session.Bytes()[0] != 1 || facts.Path.Bytes()[0] != 2 || facts.NetworkGeneration != 3 || facts.Attempt.Valid() {
			t.Fatalf("socket facts lost in projection: %+v", facts)
		}
		value.Lifecycle = &nativepeer.LifecycleFacts{}
		if _, err := projectNativeObservation(clievent.CommandShare, value); err == nil {
			t.Fatal("ambiguous socket observation accepted")
		}
	}
}
