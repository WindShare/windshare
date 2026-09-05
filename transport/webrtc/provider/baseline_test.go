package provider

import (
	"encoding/json"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/core/downloadmetrics"
)

// This reconstructs the historical socket/STUN policy, not an old application
// binary. Both arms use the pinned dependency and the same controlled topology.
func TestLocalFixedSTUNSocketLifecycleBaseline(t *testing.T) {
	if testing.Short() {
		t.Skip("optional historical comparison: four real ICE/DTLS/SCTP handshakes")
	}
	for _, stable := range []bool{false, true} {
		name := "historical-default-per-attempt"
		if stable {
			name = "stable-path-socket"
		}
		t.Run(name, func(t *testing.T) {
			server, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			observed := make(chan netip.AddrPort, 32)
			go serveSTUN(server, observed, nil)
			url := "stun:" + server.LocalAddr().String()
			authority := socketauthority.New(socketauthority.Config{})
			defer authority.Close()
			var leftLease, rightLease *socketauthority.Lease
			if stable {
				leftLease = testLease(t, authority, 1, "127.0.0.1")
				rightLease = testLease(t, authority, 2, "127.0.0.1")
			}
			metrics := downloadmetrics.New(name, time.Now, false)
			pending := metrics.Pending()
			defer pending()
			var pairs []PairFacts
			for attempt := range 2 {
				var left, right *Connection
				if stable {
					left = testConnection(t, leftLease, []string{url}, nil)
					right = testConnection(t, rightLease, []string{url}, nil)
				} else {
					left = historicalBaselinePeer(t, url)
					right = historicalBaselinePeer(t, url)
				}
				channel, received := connectPayload(t, left, right, func(candidate pion.ICECandidateInit) bool {
					return strings.Contains(candidate.Candidate, " typ srflx ")
				})
				pair, ok := left.SelectedPair()
				if !ok || pair.Protocol != "udp" || pair.LocalType == "relay" || pair.RemoteType == "relay" {
					t.Fatalf("pair=%+v", pair)
				}
				pairs = append(pairs, pair)
				// The shared harness has verified DTLS/SCTP payload before this fixture
				// admits the direct transport. This is not a ProtocolSession/UI measurement.
				metrics.Availability(true)
				const chunkSize = 4096
				payload := strings.Repeat(strconv.Itoa(attempt), chunkSize)
				if err = channel.SendText(payload); err != nil {
					t.Fatal(err)
				}
				if got := await(t, received); got != payload {
					t.Fatal("download payload mismatch")
				}
				start := uint64(attempt * chunkSize)
				metrics.Delivered("fixture-file", start, start+chunkSize, downloadmetrics.Direct)
				// Duplicate delivery cannot inflate useful-content attribution.
				metrics.Delivered("fixture-file", start, start+chunkSize, downloadmetrics.Direct)
				if err = left.Close(); err != nil {
					t.Fatal(err)
				}
				if err = right.Close(); err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					metrics.Availability(false)
				}
			}
			pending()
			snapshot := metrics.Snapshot(true)
			if snapshot.DirectBytes != 8192 || snapshot.DirectFraction == nil || *snapshot.DirectFraction != 1 || snapshot.FirstDirectElapsed == nil || snapshot.Incomplete {
				t.Fatalf("metrics=%+v", snapshot)
			}
			if stable && (pairs[0].LocalPort != pairs[1].LocalPort || pairs[0].RemotePort != pairs[1].RemotePort) {
				t.Fatalf("stable endpoints changed: %+v", pairs)
			}
			// The remote srflx port must have real STUN provenance. Default Pion
			// may select its separate host socket locally through peer-reflexive
			// checks; the stable arm must use the STUN socket on both sides.
			sources := []netip.AddrPort{await(t, observed), await(t, observed)}
			for {
				select {
				case source := <-observed:
					sources = append(sources, source)
				default:
					goto collected
				}
			}
		collected:
			for _, pair := range pairs {
				var ports []uint16
				if stable || pair.RemoteType == "srflx" {
					ports = append(ports, pair.RemotePort)
				}
				if stable || pair.LocalType == "srflx" {
					ports = append(ports, pair.LocalPort)
				}
				for _, port := range ports {
					found := false
					for _, source := range sources {
						found = found || source.Port() == port
					}
					if !found {
						t.Fatalf("selected port %d absent from STUN sources %v", port, sources)
					}
				}
			}
			elapsed := strconv.FormatInt(snapshot.FirstDirectElapsed.Milliseconds(), 10)
			output := map[string]any{
				"fixture": name, "topology": "local-udp4-loopback-single-stun", "scope": "transport-payload-download-reconstruction",
				"download_connectivity": map[string]any{
					"download_id": snapshot.DownloadID, "first_direct_elapsed_ms": elapsed,
					"direct_bytes": strconv.FormatUint(snapshot.DirectBytes, 10), "turn_bytes": "0", "application_relay_bytes": "0", "unknown_bytes": "0",
					"direct_fraction": snapshot.DirectFraction, "fallback_stall_ms": strconv.FormatInt(snapshot.FallbackStall.Milliseconds(), 10), "incomplete": snapshot.Incomplete, "final": snapshot.Final,
				},
				"selected_pairs": pairs, "stun_sources": sources,
			}
			data, err := json.Marshal(output)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(data))
		})
	}
}
func historicalBaselinePeer(t *testing.T, url string) *Connection {
	t.Helper()
	var settings pion.SettingEngine
	settings.SetNetworkTypes([]pion.NetworkType{pion.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.Equal(net.IPv4(127, 0, 0, 1)) })
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
	// No UDP mux or provider configuration: the PeerConnection owns and closes
	// default Pion sockets, just as the historical per-attempt factory did.
	pc, err := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{ICEServers: []pion.ICEServer{{URLs: []string{url}}}})
	if err != nil {
		t.Fatal(err)
	}
	connection := &Connection{PeerConnection: pc, release: func() {}}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}
