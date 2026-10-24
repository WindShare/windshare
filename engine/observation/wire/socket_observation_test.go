package wire_test

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/engine/observation/wire"
)

func TestSocketCancellationTracePreservesTimingEndpointsAndSession(t *testing.T) {
	session, _ := appobs.NewProtocolSessionID(append([]byte{1}, make([]byte, 15)...))
	path, _ := appobs.NewPeerPathID(append([]byte{2}, make([]byte, 15)...))
	event, err := appobs.NewNativeConnectivityObserved(appobs.NativeConnectivitySpec{
		Workflow: appobs.WorkflowShare, Side: "sender", State: "unknown", Kind: "stun_refresh_finished",
		Session: session, Path: path, NetworkGeneration: 3,
		Socket: &appobs.NativeSocketFacts{Local: netip.MustParseAddrPort("127.0.0.1:12345"), Server: netip.MustParseAddrPort("127.0.0.1:3478"), Result: "canceled", Duration: 12500 * time.Microsecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := wire.Encode(event)
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
