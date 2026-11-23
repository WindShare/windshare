package pionpeer

import (
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v4"
	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/connectivity/iceconfig"
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
	configuration, err := iceconfig.NewDirect(urls)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{
		ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease, ICE: configuration, MappedEndpoints: mapped, allowLoopback: true,
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
	leftGather, rightGather := make(chan struct{}), make(chan struct{})
	left.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			leftCandidates = append(leftCandidates, candidate.ToJSON())
		} else {
			close(leftGather)
		}
	})
	right.OnICECandidate(func(candidate *pion.ICECandidate) {
		if candidate != nil {
			rightCandidates = append(rightCandidates, candidate.ToJSON())
		} else {
			close(rightGather)
		}
	})
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
	for line := range strings.SplitSeq(sdp, "\r\n") {
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
			// Upstream pion/ice's localInterfaces excludes IPv6 loopback via
			// isSupportedIPv6Partial during standard srflx gathering, so STUN
			// observations apply to IPv4 loopback and non-loopback IPv6 addresses.
			if address != "::1" {
				first, second := await(t, observed), await(t, observed)
				if !first.IsValid() || !second.IsValid() {
					t.Fatalf("STUN requests not observed: %s %s", first, second)
				}
			}
		})
	}
}
func serveSTUN(server net.PacketConn, observed chan<- netip.AddrPort, mappedIP net.IP) {
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
		ip := mappedIP
		if ip == nil {
			ip = address.IP
		}
		reply, err := stun.Build(stun.NewTransactionIDSetter(message.TransactionID), stun.BindingSuccess, &stun.XORMappedAddress{IP: ip, Port: address.Port})
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
	if disallowed, err := NewPeerConnection(pion.Configuration{}, request); err == nil {
		_ = disallowed.Close()
		t.Fatal("production provider accepted loopback socket lease")
	}
	request.allowLoopback = true
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
	if _, err = iceconfig.NewDirect([]string{"invalid:url"}); err == nil {
		t.Fatal("invalid URL accepted")
	}
}

func TestProviderExcludesLoopbackByDefault(t *testing.T) {
	authority := socketauthority.New(socketauthority.Config{})
	defer authority.Close()
	for _, address := range []string{"127.0.0.1", "::1"} {
		t.Run("reject-"+address, func(t *testing.T) {
			lease, err := authority.Acquire([16]byte{1}, 1, pathID(1), []netip.Addr{netip.MustParseAddr(address)})
			if err != nil {
				if address == "::1" {
					t.Skipf("IPv6 loopback unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer lease.Close()
			if conn, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{
				ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease,
			}); err == nil {
				_ = conn.Close()
				t.Fatalf("default provider accepted loopback lease %s", address)
			}
		})
	}
	t.Run("gather-omits-loopback", func(t *testing.T) {
		nonLoopback := netip.MustParseAddr("192.0.2.1")
		sockets := socketauthority.New(socketauthority.Config{ListenPacket: func(_, _ string) (net.PacketConn, error) {
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			local := &net.UDPAddr{IP: net.IP(nonLoopback.AsSlice()), Port: conn.LocalAddr().(*net.UDPAddr).Port}
			return localAddrPacketConn{PacketConn: conn, local: local}, nil
		}})
		defer sockets.Close()
		lease, err := sockets.Acquire([16]byte{1}, 1, pathID(2), []netip.Addr{nonLoopback})
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		conn, err := NewPeerConnection(pion.Configuration{}, AttemptConfig{
			ProtocolSessionID: lease.SessionID(), NetworkGenerationID: lease.GenerationID(), PeerPathID: lease.PathID(), SocketLease: lease,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.CreateDataChannel("no-loopback", nil); err != nil {
			t.Fatal(err)
		}
		gathered := make(chan struct{})
		var candidates []string
		conn.OnICECandidate(func(candidate *pion.ICECandidate) {
			if candidate == nil {
				close(gathered)
				return
			}
			candidates = append(candidates, candidate.Address)
		})
		offer, err := conn.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		await(t, gathered)
		if len(candidates) == 0 {
			t.Fatal("expected non-loopback host candidate")
		}
		for _, raw := range candidates {
			if ip, err := netip.ParseAddr(raw); err == nil && ip.IsLoopback() {
				t.Fatalf("production provider emitted loopback candidate: %s", raw)
			}
		}
	})
}

type localAddrPacketConn struct {
	net.PacketConn
	local net.Addr
}

func (c localAddrPacketConn) LocalAddr() net.Addr { return c.local }
