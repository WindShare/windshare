package socketauthority

import (
	"net"
	"net/netip"
	"slices"
	"testing"
)

// Fake addresses expose ordering without relying on configured host interfaces.
type orderedPacket struct {
	net.PacketConn
	address net.Addr
}

func (p orderedPacket) LocalAddr() net.Addr { return p.address }

func TestSocketEndpointsPreserveFrozenInterfaceOpportunityOrder(t *testing.T) {
	addresses := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("10.0.0.2")}
	authority := New(Config{ListenPacket: func(_, address string) (net.PacketConn, error) {
		connection, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		local, err := net.ResolveUDPAddr("udp", address)
		if err != nil {
			_ = connection.Close()
			return nil, err
		}
		local.Port = connection.LocalAddr().(*net.UDPAddr).Port
		return orderedPacket{connection, local}, nil
	}})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, append(slices.Clone(addresses), addresses[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	got := lease.Endpoints()
	if len(got) != len(addresses) {
		t.Fatal(got)
	}
	for i, endpoint := range got {
		if endpoint.Addr().Unmap() != addresses[i] {
			t.Fatalf("priority order=%v, want %v", got, addresses)
		}
	}
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{addresses[2], addresses[0], addresses[1], addresses[3]}); err != ErrInvalid {
		t.Fatalf("changed frozen ordering accepted: %v", err)
	}
}
