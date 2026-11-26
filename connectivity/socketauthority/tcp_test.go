package socketauthority

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestTCPListenerUsesPathDemandCapacityAndRetirement(t *testing.T) {
	authority := New(Config{UDPCapacity: 2, TCPCapacity: 1})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(); err != nil {
		t.Fatal(err)
	}
	first := lease.TCPEndpoints()[0]
	if err = lease.PrepareTCP(); err != nil {
		t.Fatal(err)
	}
	if lease.TCPEndpoints()[0] != first {
		t.Fatal("TCP listener changed across preparation")
	}
	second, err := authority.Acquire([16]byte{1}, 1, [16]byte{2}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatalf("TCP listener blocked mandatory UDP acquisition: %v", err)
	}
	defer second.Close()
	if err = second.PrepareTCP(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("expected TCP capacity exhaustion, got %v", err)
	}
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{3}, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	mux := lease.TCP()
	conn, err := mux.GetConnByUfrag("first", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if conn.LocalAddr().String() != first.String() {
		t.Fatal("wrong TCP base")
	}
	_ = conn.Close()
	mux.RemoveConnByUfrag("first")
	if _, err = mux.GetAllConns("other", false, net.IPv4(127, 0, 0, 2)); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, err = mux.GetConnByUfrag("other", false, net.IPv4(127, 0, 0, 2)); err != ErrInvalid {
		t.Fatal(err)
	}
	_, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(); err != ErrActive {
		t.Fatal(err)
	}
	release()
	authority.Retire(1)
	if err = lease.PrepareTCP(); err != ErrRetired {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(); err != ErrClosed {
		t.Fatal(err)
	}
	var absent *Lease
	if err = absent.PrepareTCP(); err != ErrInvalid {
		t.Fatal(err)
	}
}
func TestTCPOptionalAllocationFailureLeavesUDPCapacityIntact(t *testing.T) {
	authority := New(Config{UDPCapacity: 2, TCPCapacity: 1})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = lease.PrepareTCP(); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	if len(lease.TCPEndpoints()) != 0 {
		t.Fatal("failed listener leaked")
	}
	calls := 0
	var first net.Listener
	failing := New(Config{ListenTCP: func(network, address string) (net.Listener, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("listener refused")
		}
		var err error
		first, err = net.Listen(network, address)
		return first, err
	}})
	defer failing.Close()
	dual, err := failing.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")})
	if err != nil {
		t.Fatal(err)
	}
	defer dual.Close()
	if err = dual.PrepareTCP(); err == nil {
		t.Fatal("second listener should fail")
	}
	if _, err = first.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("partial TCP allocation leaked: %v", err)
	}
	if len(dual.TCPEndpoints()) != 0 {
		t.Fatal("partial listener published")
	}
}

func TestPrepareTCPIdempotentWhenListenerRewritesIPv6Zone(t *testing.T) {
	// Simulate Windows/Linux net.Listen returning interface-name zones ("%WiFi")
	// for link-local IPv6 addresses requested with numeric interface indexes ("%19").
	rewriteZone := func(addr netip.Addr) netip.Addr {
		if addr.Is6() && addr.Zone() == "19" {
			return addr.WithZone("WiFi")
		}
		return addr
	}
	tcpCalls := 0
	authority := New(Config{
		UDPCapacity: 2,
		TCPCapacity: 2,
		ListenPacket: func(_, address string) (net.PacketConn, error) {
			requested, err := netip.ParseAddrPort(address)
			if err != nil {
				return nil, err
			}
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			port := conn.LocalAddr().(*net.UDPAddr).AddrPort().Port()
			return testAddressedPacket{PacketConn: conn, address: net.UDPAddrFromAddrPort(netip.AddrPortFrom(rewriteZone(requested.Addr()), port))}, nil
		},
		ListenTCP: func(_, address string) (net.Listener, error) {
			tcpCalls++
			requested, err := netip.ParseAddrPort(address)
			if err != nil {
				return nil, err
			}
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			port := listener.Addr().(*net.TCPAddr).AddrPort().Port()
			return testAddressedListener{Listener: listener, address: net.TCPAddrFromAddrPort(netip.AddrPortFrom(rewriteZone(requested.Addr()), port))}, nil
		},
	})
	defer authority.Close()
	addresses := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("fe80::1%19")}
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.PrepareTCP(); err != nil {
		t.Fatal(err)
	}
	first := lease.TCPEndpoints()
	if err := lease.PrepareTCP(); err != nil {
		t.Fatalf("second PrepareTCP failed or exceeded TCP capacity: %v", err)
	}
	_, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := lease.PrepareTCP(); err != nil {
		t.Fatalf("retry PrepareTCP failed: %v", err)
	}
	if tcpCalls != len(addresses) || len(lease.TCPEndpoints()) != len(addresses) {
		t.Fatalf("duplicate TCP listeners opened: calls=%d endpoints=%d want=%d", tcpCalls, len(lease.TCPEndpoints()), len(addresses))
	}
	if lease.TCPEndpoints()[1] != first[1] {
		t.Fatalf("link-local TCP listener changed: got=%v want=%v", lease.TCPEndpoints()[1], first[1])
	}
	authority.mu.Lock()
	used := authority.tcpCount
	authority.mu.Unlock()
	if used != len(addresses) {
		t.Fatalf("tcpCount = %d, want %d", used, len(addresses))
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	authority.mu.Lock()
	remaining := authority.tcpCount
	authority.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("tcpCount after close = %d, want 0", remaining)
	}
}
