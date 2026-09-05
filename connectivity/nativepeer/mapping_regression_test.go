package nativepeer

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/webrtc/provider"
)

type pinholeGateway struct{ mappedGateway }

func (g *pinholeGateway) Create(_ context.Context, request reachability.Request) (reachability.Lease, error) {
	return reachability.Lease{External: request.Endpoint.Local, Lifetime: time.Minute, GatewayID: "fake", ResourceID: "pinhole", Kind: "pcp"}, nil
}
func (g *pinholeGateway) Renew(ctx context.Context, request reachability.Request, _ reachability.Lease) (reachability.Lease, error) {
	return g.Create(ctx, request)
}

type addressedPacket struct {
	net.PacketConn
	address *net.UDPAddr
}

func (p addressedPacket) LocalAddr() net.Addr { return p.address }

func TestNativeIPv6PinholeRetainedByExactSelectedHostPair(t *testing.T) {
	h := newHarness(t)
	ip := netip.MustParseAddr("2001:4860::123")
	h.monitor.state = networkstate.State{Addresses: []networkstate.Address{{IP: ip, InterfaceIndex: 1}}, Routes: []networkstate.Route{{InterfaceIndex: 1, Family: 6}}}
	sockets := socketauthority.New(socketauthority.Config{ListenPacket: func(string, string) (net.PacketConn, error) {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		port := conn.LocalAddr().(*net.UDPAddr).Port
		return addressedPacket{conn, &net.UDPAddr{IP: net.IP(ip.AsSlice()), Port: port}}, nil
	}})
	h.native.config.Sockets = sockets
	defer sockets.Close()
	gateway := &pinholeGateway{}
	h.native.config.Reachability = reachability.New(reachability.Config{Now: func() time.Time { return h.now }, Gateways: []reachability.Gateway{gateway}})
	req := request(1)
	_, _ = h.native.SetDemand(req.ProtocolSessionID, req.Binding.PeerPathID, true, false)
	_, _ = h.native.NewPeerConnection(context.Background(), req)
	h.native.Maintain(context.Background())
	h.now = h.now.Add(3 * time.Second)
	h.native.Maintain(context.Background())
	path := h.native.paths[pathKey{req.ProtocolSessionID, req.Binding.PeerPathID}]
	if !h.native.hasMappingLocked(path) {
		t.Fatal("pinhole not recognized")
	}
	if len(h.native.mappedLocked(path)) != 0 {
		t.Fatal("native IPv6 host synthesized as srflx")
	}
	local := h.requests[0].SocketLease.Endpoints()[0]
	h.requests[0].Observe(provider.Event{Pair: &provider.PairFacts{LocalAddress: local.Addr().String(), LocalPort: local.Port(), Protocol: "udp"}})
	h.native.SetDirect(req.ProtocolSessionID, req.Binding.PeerPathID)
	h.now = h.now.Add(4 * time.Second)
	h.native.Maintain(context.Background())
	if gateway.deletes.Load() != 0 || path.mappedLocal != local || path.mappedProtocol != reachability.UDP {
		t.Fatal("selected pinhole lost retention", gateway.deletes.Load(), path.mappedLocal)
	}
	_ = h.native.Close(context.Background())
	if gateway.deletes.Load() != 1 {
		t.Fatal("pinhole not released", gateway.deletes.Load())
	}
}
func TestProvenTCPAllocatesMappingBeforeFreshAttemptAndRetainsOnlyExactProtocol(t *testing.T) {
	if !provider.Capabilities(provider.TCPNativeWindows).IPv4 {
		t.Skip("this platform has no proven native TCP profile")
	}
	h := newHarness(t)
	gateway := &mappedGateway{}
	h.native.config.Reachability = reachability.New(reachability.Config{Now: func() time.Time { return h.now }, Gateways: []reachability.Gateway{gateway}})
	req := request(1)
	_, _ = h.native.ApplyControl(req.ProtocolSessionID, controlBytes(t, 1, protocolsession.PeerPathDemand, ControlLifetime))
	_, _ = h.native.NewPeerConnection(context.Background(), req)
	if len(h.requests[0].SocketLease.TCPEndpoints()) == 0 {
		t.Fatal("capability did not prepare exact TCP socket")
	}
	h.native.Maintain(context.Background())
	h.now = h.now.Add(3 * time.Second)
	h.native.Maintain(context.Background())
	_, _ = h.native.NewPeerConnection(context.Background(), request(2))
	var tcp int
	for _, endpoint := range h.requests[1].MappedEndpoints {
		if endpoint.Protocol == "tcp" {
			tcp++
		}
	}
	if tcp != 1 {
		t.Fatal("production TCP mapping not supplied", h.requests[1].MappedEndpoints)
	}
	h.requests[1].Observe(provider.Event{Pair: &provider.PairFacts{LocalAddress: "8.8.8.8", LocalPort: 4242, Protocol: "tcp"}})
	h.native.SetDirect(req.ProtocolSessionID, req.Binding.PeerPathID)
	h.now = h.now.Add(4 * time.Second)
	h.native.Maintain(context.Background())
	var retained int
	for _, fact := range h.native.config.Reachability.Facts() {
		if fact.GatewayID != "" {
			retained++
			if fact.Endpoint.Protocol != reachability.TCP {
				t.Fatal("unselected UDP mapping retained", fact)
			}
		}
	}
	if retained != 1 {
		t.Fatal("selected TCP mapping lost", retained)
	}
	_ = h.native.Close(context.Background())
}
