package provider

import (
	"net"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestTwoSidedAllocatedUDPPortsCarryPayloadAcrossReplacement(t *testing.T) {
	leftGateway, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer leftGateway.Close()
	rightGateway, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer rightGateway.Close()
	leftExternal := leftGateway.LocalAddr().(*net.UDPAddr).AddrPort()
	rightExternal := rightGateway.LocalAddr().(*net.UDPAddr).AddrPort()
	allocate := func(router, remote net.Addr) *socketauthority.Authority {
		authority := socketauthority.New(socketauthority.Config{ListenPacket: func(network, address string) (net.PacketConn, error) {
			conn, err := net.ListenPacket(network, address)
			if err != nil {
				return nil, err
			}
			return &translatedPacketConn{PacketConn: conn, router: router, remote: remote}, nil
		}})
		t.Cleanup(func() { _ = authority.Close() })
		return authority
	}
	leftLease := testLease(t, allocate(leftGateway.LocalAddr(), rightGateway.LocalAddr()), 1, "127.0.0.1")
	rightLease := testLease(t, allocate(rightGateway.LocalAddr(), leftGateway.LocalAddr()), 2, "127.0.0.1")
	var leftTraffic, rightTraffic atomic.Int32
	go forwardUDP(leftGateway, leftLease.Endpoints()[0], rightGateway.LocalAddr(), &leftTraffic)
	go forwardUDP(rightGateway, rightLease.Endpoints()[0], leftGateway.LocalAddr(), &rightTraffic)
	for range 2 {
		left := testConnection(t, leftLease, nil, []MappedEndpoint{{Local: leftLease.Endpoints()[0], External: leftExternal}})
		right := testConnection(t, rightLease, nil, []MappedEndpoint{{Local: rightLease.Endpoints()[0], External: rightExternal}})
		connectPayload(t, left, right, func(candidate pion.ICECandidateInit) bool {
			parsed, err := ice.UnmarshalCandidate(candidate.Candidate)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := netip.AddrPortFrom(netip.MustParseAddr(parsed.Address()), uint16(parsed.Port()))
			return endpoint == leftExternal || endpoint == rightExternal
		})
		pair, ok := left.SelectedPair()
		if !ok || pair.RemotePort != rightExternal.Port() {
			t.Fatalf("two-sided pair=%+v", pair)
		}
		_ = left.Close()
		_ = right.Close()
	}
	if leftTraffic.Load() == 0 || rightTraffic.Load() == 0 {
		t.Fatal("two-sided mapping did not carry checks and payload")
	}
}
