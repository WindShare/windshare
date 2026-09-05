package provider

import (
	"net"
	"testing"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
)

// This is a capability probe, not product enablement: it proves a native active
// agent and passive listener with UDP completely absent from both ICE agents.
func TestNativeTCPPassiveActivePayload(t *testing.T) {
	addresses := []string{"127.0.0.1"}
	if ipv6 := localTCPIPv6(t); ipv6 != "" {
		addresses = append(addresses, ipv6)
	}
	for _, address := range addresses {
		t.Run(address, func(t *testing.T) {
			network := pion.NetworkTypeTCP4
			listenNetwork := "tcp4"
			if net.ParseIP(address).To4() == nil {
				network = pion.NetworkTypeTCP6
				listenNetwork = "tcp6"
			}
			listener, err := net.Listen(listenNetwork, net.JoinHostPort(address, "0"))
			if err != nil {
				t.Skipf("family unavailable: %v", err)
			}
			mux := ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: listener})
			defer mux.Close()
			makePeer := func(passive bool) *Connection {
				var settings pion.SettingEngine
				settings.SetNetworkTypes([]pion.NetworkType{network})
				settings.SetIncludeLoopbackCandidate(true)
				settings.SetICEMulticastDNSMode(ice.MulticastDNSModeQueryOnly)
				settings.SetIPFilter(func(ip net.IP) bool { return ip.String() == address })
				if passive {
					settings.SetICETCPMux(mux)
					settings.DisableActiveTCP(true)
				}
				pc, createErr := pion.NewAPI(pion.WithSettingEngine(settings)).NewPeerConnection(pion.Configuration{})
				if createErr != nil {
					t.Fatal(createErr)
				}
				connection := &Connection{PeerConnection: pc, release: func() {}}
				t.Cleanup(func() { _ = connection.Close() })
				return connection
			}
			left, right := makePeer(false), makePeer(true)
			connectPayload(t, left, right, func(candidate pion.ICECandidateInit) bool { t.Log(candidate.Candidate); return true })
			pair, ok := left.SelectedPair()
			if !ok || pair.Protocol != "tcp" || pair.RemotePort != uint16(listener.Addr().(*net.TCPAddr).Port) {
				t.Fatalf("TCP pair=%+v, selected=%v", pair, ok)
			}
		})
	}
}
