package provider

import (
	"net"
	"net/netip"
	"testing"

	"github.com/pion/ice/v4"
	"github.com/pion/transport/v4"
	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/connectivity/socketauthority"
)

type turnTestInterfaceNet struct {
	transport.Net
	interfaces []*transport.Interface
}

func TestTURNFailedEgressUsesSourceBoundBackupOverUDPAndTCP(t *testing.T) {
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			endpoint, server := localTURN(t, protocol)
			authority := socketauthority.New(socketauthority.Config{})
			t.Cleanup(func() { _ = authority.Close() })
			connection := turnConnection(t, testLease(t, authority, 1, "127.0.0.1"), endpoint)
			plan := connection.turnGathering.plan
			// An unassigned source fails immediately on both transports, without
			// an external blackhole or a real transaction timeout in this test.
			plan.initial[0].LocalAddress = netip.MustParseAddr("192.0.2.99")
			backup := plan.initial[0]
			backup.LocalAddress = netip.MustParseAddr("127.0.0.1")
			plan.backup = []ice.TURNRoute{backup}
			success := make(chan ice.TURNRoute, 1)
			plan.succeeded = func(route ice.TURNRoute) { success <- route }
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
			if stats.ActiveSockets != 1 || connection.request.TURNReservation.socketLimit() != 1 {
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
