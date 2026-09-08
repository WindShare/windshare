package runtrace

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

func TestSocketCancellationTracePreservesTimingEndpointsAndSession(t *testing.T) {
	session, _ := clievent.NewProtocolSessionID(append([]byte{1}, make([]byte, 15)...))
	path, _ := clievent.NewPeerPathID(append([]byte{2}, make([]byte, 15)...))
	event, err := clievent.NewNativeConnectivityObserved(clievent.NativeConnectivitySpec{
		Command: clievent.CommandShare, Side: "sender", State: "unknown", Kind: "stun_refresh_finished",
		Session: session, Path: path, NetworkGeneration: 3,
		Socket: &clievent.NativeSocketFacts{Local: netip.MustParseAddrPort("127.0.0.1:12345"), Server: netip.MustParseAddrPort("127.0.0.1:3478"), Result: "canceled", Duration: 12500 * time.Microsecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV3(testRunIdentity(1), entryMetadata{sequence: 1, time: time.Unix(0, 0)}, event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record.Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"local_endpoint":"127.0.0.1:12345"`, `"stun_server":"127.0.0.1:3478"`,
		`"duration_ms":"12.500"`, `"result":"canceled"`, `"network_generation_id":"3"`,
	} {
		if !strings.Contains(string(raw), fragment) {
			t.Fatalf("missing socket evidence %s in %s", fragment, raw)
		}
	}
	if record.Correlation.ProtocolSessionID != "AQAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerPathID != "AgAAAAAAAAAAAAAAAAAAAA" || record.Correlation.PeerAttemptID != "" {
		t.Fatalf("incorrect trace attribution: %+v", record.Correlation)
	}
}
