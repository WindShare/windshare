package provider

import (
	"io"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func TestTCPMappedExternalPortAndUDPRemainInOnePeerConnection(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("TCP enablement requires the recorded Windows capability profile")
	}
	addresses := []string{"127.0.0.1"}
	if address := localTCPIPv6(t); address != "" {
		addresses = append(addresses, address)
	}
	for _, address := range addresses {
		t.Run(address, func(t *testing.T) { testTCPMapping(t, address) })
	}
}
func testTCPMapping(t *testing.T, address string) {
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	leftLease := testLease(t, authority, 1, address)
	rightLease := testLease(t, authority, 2, address)
	if err := rightLease.PrepareTCP(true); err != nil {
		t.Fatal(err)
	}
	internal := rightLease.TCPEndpoints()[0]
	listenNetwork := "tcp4"
	if net.ParseIP(address).To4() == nil {
		listenNetwork = "tcp6"
	}
	forwarder, err := net.Listen(listenNetwork, net.JoinHostPort(address, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()
	external := forwarder.Addr().(*net.TCPAddr).AddrPort()
	go proxyTCP(forwarder, internal)
	makePeer := func(lease *socketauthority.Lease, profile TCPProfile, mappings []MappedEndpoint) *Connection {
		pc, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: 1, PeerPathID: lease.PathID(), SocketLease: lease, TCPProfile: profile, MappedEndpoints: mappings})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		return pc
	}
	left := makePeer(leftLease, TCPNativeWindows, nil)
	right := makePeer(rightLease, TCPNativeWindows, []MappedEndpoint{{Local: internal, External: external, Protocol: "tcp"}})
	sawUDP := false
	sawMapped := false
	connectPayload(t, left, right, func(candidate pion.ICECandidateInit) bool {
		parsed, err := ice.UnmarshalCandidate(candidate.Candidate)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.NetworkType().IsUDP() {
			sawUDP = true
			return false
		}
		if parsed.Port() != int(external.Port()) {
			return false
		}
		sawMapped = right.IsMappedCandidate(candidate.Candidate)
		return true
	})
	pair, ok := left.SelectedPair()
	if !sawUDP || !sawMapped || !ok || pair.Protocol != "tcp" || pair.RemoteType != "srflx" || pair.RemotePort != external.Port() || external.Port() == internal.Port() {
		t.Fatalf("TCP remapping proof UDP=%v mapped=%v pair=%+v selected=%v", sawUDP, sawMapped, pair, ok)
	}
}
func proxyTCP(listener net.Listener, internal netip.AddrPort) {
	for {
		incoming, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer incoming.Close()
			outgoing, err := net.DialTimeout("tcp", internal.String(), time.Second)
			if err != nil {
				return
			}
			defer outgoing.Close()
			_ = incoming.SetDeadline(time.Now().Add(10 * time.Second))
			_ = outgoing.SetDeadline(time.Now().Add(10 * time.Second))
			done := make(chan struct{})
			go func() { _, _ = io.Copy(outgoing, incoming); _ = outgoing.Close(); close(done) }()
			_, _ = io.Copy(incoming, outgoing)
			_ = incoming.Close()
			<-done
		}()
	}
}
func TestCapabilityGateIsClosedAndPlatformScoped(t *testing.T) {
	if got := tcpCapabilities("windows", TCPNativeWindows); !got.IPv4 || !got.IPv6 || got.PassiveOnly {
		t.Fatal(got)
	}
	if got := tcpCapabilities("windows", TCPChromiumWindows); !got.IPv4 || got.IPv6 || !got.PassiveOnly {
		t.Fatal(got)
	}
	for _, profile := range []TCPProfile{"", TCPNativeWindows, TCPChromiumWindows, "unknown"} {
		if got := tcpCapabilities("linux", profile); got != (TCPCapability{}) {
			t.Fatal(got)
		}
	}
	if got := tcpCapabilities("windows", "unknown"); got != (TCPCapability{}) {
		t.Fatal(got)
	}
}
