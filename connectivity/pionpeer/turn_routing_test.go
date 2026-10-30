package pionpeer

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/pion/transport/v5"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
	"github.com/windshare/windshare/connectivity/turnroute"
)

type turnTestInterfaceNet struct {
	transport.Net
	interfaces []*transport.Interface
}

func TestTURNRouteAdapterReleasesFailedEgressAndBindsBackupOverUDPAndTCP(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			endpoint, server := localTURN(t, protocol)
			authority := socketauthority.New(socketauthority.Config{})
			t.Cleanup(func() { _ = authority.Close() })
			connection := turnConnection(t, testLease(t, authority, 1, "127.0.0.1"), endpoint)
			// Policy tests own fallback selection. This script checks that the Pion
			// adapter honors source binding and transfers/releases the actual reservation.
			backup := turnroute.Route{Server: endpoint, LocalAddress: netip.MustParseAddr("127.0.0.1")}
			success := make(chan turnroute.Route, 1)
			connection.turnGathering = scriptedTURNGatherer{run: func(ctx context.Context, attempt turnroute.Attempt) {
				reservation := connection.request.TURN.Reservation
				release, ok := reservation.StartInitialRoute()
				if !ok {
					t.Error("initial route not admitted")
					return
				}
				failed := turnroute.Route{Server: endpoint, LocalAddress: netip.MustParseAddr("192.0.2.99")}
				if allocation := attempt(ctx, failed, release); allocation != nil {
					allocation.Discard()
					t.Error("unassigned source unexpectedly worked")
					return
				}
				release, ok = reservation.TryBackupRoute()
				if !ok {
					t.Error("backup route not admitted")
					return
				}
				if allocation := attempt(ctx, backup, release); allocation != nil {
					success <- backup
				}
			}}
			if _, err := connection.CreateDataChannel("backup", nil); err != nil {
				t.Fatal(err)
			}
			gathered := pion.GatheringCompletePromise(connection.PeerConnection)
			offer, err := connection.CreateOffer(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = connection.SetLocalDescription(offer); err != nil {
				t.Fatal(err)
			}
			await(t, gathered)
			if route := await(t, success); route.LocalAddress != backup.LocalAddress {
				t.Fatal("gathering ignored the requested local egress")
			}
			awaitTURNAllocations(t, server, 1)
			stats, _ := connection.TURNResources()
			if stats.ActiveSockets != 1 || connection.request.TURN.Reservation.SocketLimit() != 1 {
				t.Fatal("failed egress retained capacity")
			}
			if err = connection.Close(); err != nil {
				t.Fatal(err)
			}
			awaitTURNAllocations(t, server, 0)
		})
	}
}

func (n turnTestInterfaceNet) Interfaces() ([]*transport.Interface, error) { return n.interfaces, nil }

type turnTestLocalSocket struct {
	net.PacketConn
	local net.Addr
}

func (s turnTestLocalSocket) LocalAddr() net.Addr { return s.local }

func TestTURNRoutesEachServerWithoutBindingDirectInterfaces(t *testing.T) {
	udpURL, udpServer := localTURN(t, "udp")
	tcpURL, tcpServer := localTURN(t, "tcp")
	// Advertise addresses that cannot reach the TURN service. The relay control
	// connection must follow its own OS route, independent of direct candidates.
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}
	authority := socketauthority.New(socketauthority.Config{ListenPacket: func(_, address string) (net.PacketConn, error) {
		socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		local, err := net.ResolveUDPAddr("udp4", address)
		if err != nil {
			_ = socket.Close()
			return nil, err
		}
		local.Port = socket.LocalAddr().(*net.UDPAddr).Port
		return turnTestLocalSocket{socket, local}, nil
	}})
	t.Cleanup(func() { _ = authority.Close() })
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{2}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	connection := turnConnection(t, lease, udpURL, tcpURL)
	iface := transport.NewInterface(net.Interface{Index: 1, Name: "isolated-direct", Flags: net.FlagUp})
	for _, address := range addresses {
		iface.AddAddress(&net.IPNet{IP: net.IP(address.AsSlice()), Mask: net.CIDRMask(24, 32)})
	}
	connection.turn.Net = turnTestInterfaceNet{connection.turn.Net, []*transport.Interface{iface}}
	if _, err := connection.CreateDataChannel("routed-turn", nil); err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(connection.PeerConnection)
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	await(t, gathered)
	awaitTURNAllocations(t, udpServer, 1)
	awaitTURNAllocations(t, tcpServer, 1)
	stats, _ := connection.TURNResources()
	if stats.ActiveSockets != 2 || stats.OpenedSockets != 2 {
		t.Fatalf("endpoint alternatives multiplied by direct interfaces: %+v", stats)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	awaitTURNAllocations(t, udpServer, 0)
	awaitTURNAllocations(t, tcpServer, 0)
}

type scriptedTURNGatherer struct {
	run func(context.Context, turnroute.Attempt)
}

func (g scriptedTURNGatherer) Run(ctx context.Context, attempt turnroute.Attempt) {
	g.run(ctx, attempt)
}
func (scriptedTURNGatherer) Select(netip.AddrPort) {}
