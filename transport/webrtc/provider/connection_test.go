package provider

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

func pathID(value byte) [16]byte { return [16]byte{value} }
func testLease(t *testing.T, authority *socketauthority.Authority, path byte, address string) *socketauthority.Lease {
	t.Helper()
	return testSessionLease(t, authority, [16]byte{1}, path, address)
}
func testSessionLease(t *testing.T, authority *socketauthority.Authority, session [16]byte, path byte, address string) *socketauthority.Lease {
	t.Helper()
	lease, err := authority.Acquire(session, 1, pathID(path), []netip.Addr{netip.MustParseAddr(address)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return lease
}
func testConnection(t *testing.T, lease *socketauthority.Lease, urls []string, mapped []MappedEndpoint) *Connection {
	t.Helper()
	connection, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{
		ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease, STUNURLs: urls, MappedEndpoints: mapped,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}
func await[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("provider evidence timed out")
		var zero T
		return zero
	}
}
func connectPayload(t *testing.T, left, right *Connection, filter func(pion.ICECandidateInit) bool) (*pion.DataChannel, <-chan string) {
	t.Helper()
	opened := make(chan struct{}, 1)
	received := make(chan string, 1)
	right.OnDataChannel(func(channel *pion.DataChannel) {
		channel.OnMessage(func(message pion.DataChannelMessage) { received <- string(message.Data) })
	})
	channel, err := left.CreateDataChannel("evidence", nil)
	if err != nil {
		t.Fatal(err)
	}
	channel.OnOpen(func() { opened <- struct{}{} })
	var leftCandidates, rightCandidates []pion.ICECandidateInit
	left.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			leftCandidates = append(leftCandidates, candidate.ToJSON())
		}
	})
	right.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			rightCandidates = append(rightCandidates, candidate.ToJSON())
		}
	})
	leftGather := pion.GatheringCompletePromise(left.PeerConnection)
	offer, err := left.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = left.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	await(t, leftGather)
	// Strip gathered SDP candidates so the fixture explicitly controls the
	// externally advertised endpoints while still using real ICE checks.
	remoteOffer := *left.LocalDescription()
	remoteOffer.SDP = withoutCandidates(remoteOffer.SDP)
	if err = right.SetRemoteDescription(remoteOffer); err != nil {
		t.Fatal(err)
	}
	rightGather := pion.GatheringCompletePromise(right.PeerConnection)
	answer, err := right.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = right.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	await(t, rightGather)
	remoteAnswer := *right.LocalDescription()
	remoteAnswer.SDP = withoutCandidates(remoteAnswer.SDP)
	if err = left.SetRemoteDescription(remoteAnswer); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range leftCandidates {
		if filter == nil || filter(candidate) {
			if err = right.AddICECandidate(candidate); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, candidate := range rightCandidates {
		if filter == nil || filter(candidate) {
			if err = left.AddICECandidate(candidate); err != nil {
				t.Fatal(err)
			}
		}
	}
	await(t, opened)
	const payload = "authenticated data on the leased physical endpoint"
	if err = channel.SendText(payload); err != nil {
		t.Fatal(err)
	}
	if got := await(t, received); got != payload {
		t.Fatalf("payload=%q", got)
	}
	pair, ok := left.SelectedPair()
	if !ok || pair.RemoteType == "relay" {
		t.Fatalf("pair=%+v,ok=%v", pair, ok)
	}
	return channel, received
}
func withoutCandidates(sdp string) string {
	var lines []string
	for _, line := range strings.Split(sdp, "\r\n") {
		if !strings.HasPrefix(line, "a=candidate:") && line != "a=end-of-candidates" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\r\n")
}

func TestSocketCarriesSTUNGatherChecksPayloadAndFreshReplacement(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run(address, func(t *testing.T) {
			network := "udp4"
			if address == "::1" {
				network = "udp6"
			}
			server, err := net.ListenPacket(network, net.JoinHostPort(address, "0"))
			if err != nil {
				t.Skipf("family unavailable: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })
			observed := make(chan netip.AddrPort, 16)
			go serveSTUN(server, observed, nil)
			authority := socketauthority.New(socketauthority.Config{})
			t.Cleanup(func() { _ = authority.Close() })
			leftLease := testLease(t, authority, 1, address)
			rightLease := testLease(t, authority, 2, address)
			stableLeft, stableRight := leftLease.Endpoints()[0], rightLease.Endpoints()[0]
			url := "stun:" + server.LocalAddr().String()
			for range 2 {
				left := testConnection(t, leftLease, []string{url}, nil)
				right := testConnection(t, rightLease, []string{url}, nil)
				connectPayload(t, left, right, nil)
				if got := leftLease.Endpoints()[0]; got != stableLeft {
					t.Fatal("left endpoint changed")
				}
				if got := rightLease.Endpoints()[0]; got != stableRight {
					t.Fatal("right endpoint changed")
				}
				_ = left.Close()
				_ = right.Close()
			}
			first, second := await(t, observed), await(t, observed)
			if !((first == stableLeft && second == stableRight) || (first == stableRight && second == stableLeft)) {
				t.Fatalf("STUN endpoints %s %s; sockets %s %s", first, second, stableLeft, stableRight)
			}
		})
	}
}
func serveSTUN(server net.PacketConn, observed chan<- netip.AddrPort, count *atomic.Int32) {
	buffer := make([]byte, 1500)
	for {
		n, source, err := server.ReadFrom(buffer)
		if err != nil {
			return
		}
		message := &stun.Message{Raw: append([]byte{}, buffer[:n]...)}
		if message.Decode() != nil || message.Type != stun.BindingRequest {
			continue
		}
		address := source.(*net.UDPAddr)
		if observed != nil {
			observed <- address.AddrPort()
		}
		if count != nil {
			count.Add(1)
		}
		reply, err := stun.Build(stun.NewTransactionIDSetter(message.TransactionID), stun.BindingSuccess, &stun.XORMappedAddress{IP: address.IP, Port: address.Port})
		if err == nil {
			_, _ = server.WriteTo(reply.Raw, source)
		}
	}
}

func TestExternalPortCandidateCarriesPayload(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{})
	t.Cleanup(func() { _ = authority.Close() })
	rightLease := testLease(t, authority, 2, "127.0.0.1")
	forwarder, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = forwarder.Close() })
	external := forwarder.LocalAddr().(*net.UDPAddr).AddrPort()
	remote := net.UDPAddrFromAddrPort(rightLease.Endpoints()[0])
	natAuthority := socketauthority.New(socketauthority.Config{ListenPacket: func(network, address string) (net.PacketConn, error) {
		conn, listenErr := net.ListenPacket(network, address)
		if listenErr != nil {
			return nil, listenErr
		}
		return &translatedPacketConn{PacketConn: conn, router: forwarder.LocalAddr(), remote: remote}, nil
	}})
	t.Cleanup(func() { _ = natAuthority.Close() })
	leftLease := testLease(t, natAuthority, 1, "127.0.0.1")
	internal := leftLease.Endpoints()[0]
	var forwarded atomic.Int32
	go forwardUDP(forwarder, internal, remote, &forwarded)
	left := testConnection(t, leftLease, nil, []MappedEndpoint{{Local: internal, External: external}})
	right := testConnection(t, rightLease, nil, nil)
	connectPayload(t, left, right, func(candidate pion.ICECandidateInit) bool {
		parsed, err := ice.UnmarshalCandidate(candidate.Candidate)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Port() == int(internal.Port()) {
			return false
		}
		return true
	})
	if external.Port() == internal.Port() || forwarded.Load() == 0 {
		t.Fatal("distinct external port was not traversed")
	}
	pair, ok := right.SelectedPair()
	if !ok || pair.RemotePort != external.Port() {
		t.Fatalf("external pair not selected: %+v", pair)
	}
}

// Each side of the forwarding fixture has a real UDP socket; packets routed to
// the allocated external port are delivered to the advertised internal base.
type translatedPacketConn struct {
	net.PacketConn
	router, remote net.Addr
}

func (c *translatedPacketConn) WriteTo(payload []byte, _ net.Addr) (int, error) {
	return c.PacketConn.WriteTo(payload, c.router)
}
func (c *translatedPacketConn) ReadFrom(payload []byte) (int, net.Addr, error) {
	n, _, err := c.PacketConn.ReadFrom(payload)
	return n, c.remote, err
}

func forwardUDP(conn net.PacketConn, internal netip.AddrPort, remote net.Addr, count *atomic.Int32) {
	buffer := make([]byte, 65536)
	for {
		n, from, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		if from.String() == internal.String() {
			if remote != nil {
				_, _ = conn.WriteTo(buffer[:n], remote)
			}
		} else {
			remote = from
			count.Add(1)
			_, _ = conn.WriteTo(buffer[:n], net.UDPAddrFromAddrPort(internal))
		}
	}
}

func TestProviderRejectsMismatchedAndConcurrentResources(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	lease := testLease(t, authority, 1, "127.0.0.1")
	if _, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{}); err == nil {
		t.Fatal("nil lease accepted")
	}
	request := AttemptConfig{ProtocolSessionID: lease.SessionID(), NetworkGenerationID: 1, PeerPathID: pathID(1), SocketLease: lease, Observe: func(Event) {}}
	request.MappedEndpoints = []MappedEndpoint{{Local: netip.MustParseAddrPort("127.0.0.1:1"), External: netip.MustParseAddrPort("127.0.0.1:2")}}
	if _, err := NewPeerConnection(pion.Configuration{}, request); err == nil {
		t.Fatal("foreign socket accepted")
	}
	request.MappedEndpoints = nil
	request.ProtocolSessionID = [16]byte{2}
	if foreign, err := NewPeerConnection(pion.Configuration{}, request); err == nil {
		_ = foreign.Close()
		t.Fatal("foreign session socket accepted")
	}
	request.ProtocolSessionID = lease.SessionID()
	connection, err := NewPeerConnection(pion.Configuration{}, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPeerConnection(pion.Configuration{}, request); err != socketauthority.ErrActive {
		t.Fatalf("concurrent err=%v", err)
	}
	connection.OnICEConnectionStateChange(nil)
	connection.OnConnectionStateChange(nil)
	if _, ok := connection.SelectedPair(); ok {
		t.Fatal("unselected connection has pair")
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	request.STUNURLs = []string{"invalid:url"}
	if _, err = NewPeerConnection(pion.Configuration{}, request); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestPriorityInterleavesFamiliesAndInterfaces(t *testing.T) {
	endpoints := []netip.AddrPort{netip.MustParseAddrPort("192.168.1.1:1"), netip.MustParseAddrPort("192.168.2.1:2"), netip.MustParseAddrPort("[2001:db8::1]:3")}
	policy := localPreference(endpoints)
	want := []uint16{65535, 65533, 65534}
	for i, endpoint := range endpoints {
		candidate, err := ice.NewCandidateHost(&ice.CandidateHostConfig{Network: "udp", Address: endpoint.Addr().String(), Port: int(endpoint.Port()), Component: 1})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := policy(candidate)
		if !ok || got != want[i] {
			t.Fatalf("preference=%d,%v", got, ok)
		}
	}
	candidate, _ := ice.NewCandidateServerReflexive(&ice.CandidateServerReflexiveConfig{Network: "udp", Address: "203.0.113.1", Port: 4, RelAddr: "192.168.1.1", RelPort: 1, Component: 1})
	if got, ok := policy(candidate); !ok || got != 65535 {
		t.Fatal("srflx lost base preference")
	}
}

func TestInitialCheckingWindowIndependentOfConnectedTimeout(t *testing.T) {
	const pac = 200 * time.Millisecond
	disconnected, failed := 10*time.Millisecond, 10*time.Millisecond
	agent, err := ice.NewAgentWithOptions(ice.WithNetworkTypes([]ice.NetworkType{ice.NetworkTypeUDP4}), ice.WithDisconnectedTimeout(disconnected), ice.WithFailedTimeout(failed), ice.WithProviderConfig(ice.ProviderConfig{InitialCheckingTimeout: pac}))
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = agent.Dial(ctx, "remote-ufrag", "remote-password")
	if err == nil || time.Since(start) < 60*time.Millisecond {
		t.Fatalf("initial ICE failed before PAC: %v, %v", err, time.Since(start))
	}
	if !errors.Is(err, ice.ErrCanceledByCaller) {
		t.Fatalf("application ceiling should own cancellation: %v", err)
	}
}
