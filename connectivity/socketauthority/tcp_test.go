package socketauthority

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestTCPListenerUsesPathDemandCapacityAndRetirement(t *testing.T) {
	authority := New(Config{Capacity: 2})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(false); err != nil {
		t.Fatal(err)
	}
	first := lease.TCPEndpoints()[0]
	if err = lease.PrepareTCP(false); err != nil {
		t.Fatal(err)
	}
	if lease.TCPEndpoints()[0] != first {
		t.Fatal("TCP listener changed across preparation")
	}
	if _, err = authority.Acquire([16]byte{1}, 1, [16]byte{2}, []netip.Addr{netip.MustParseAddr("127.0.0.1")}); err != ErrCapacity {
		t.Fatal(err)
	}
	mux := lease.TCP()
	conn, err := mux.GetConnByUfrag("first", false, net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	alias, err := mux.GetConnForEndpoint("first", first)
	if err != nil {
		t.Fatal(err)
	}
	if conn.LocalAddr().String() != first.String() || alias.LocalAddr().String() != first.String() {
		t.Fatal("wrong TCP base")
	}
	_ = conn.Close()
	_ = alias.Close()
	mux.RemoveConnByUfrag("first")
	if _, err = mux.GetAllConns("other", false, net.IPv4(127, 0, 0, 2)); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, err = mux.GetConnByUfrag("other", false, net.IPv4(127, 0, 0, 2)); err != ErrInvalid {
		t.Fatal(err)
	}
	if _, err = mux.GetConnForEndpoint("other", netip.MustParseAddrPort("127.0.0.1:1")); err != ErrInvalid {
		t.Fatal(err)
	}
	_, release, err := lease.Claim()
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(false); err != ErrActive {
		t.Fatal(err)
	}
	release()
	authority.Retire(1)
	if err = lease.PrepareTCP(false); err != ErrRetired {
		t.Fatal(err)
	}
	if err = lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lease.PrepareTCP(false); err != ErrClosed {
		t.Fatal(err)
	}
	var absent *Lease
	if err = absent.PrepareTCP(false); err != ErrInvalid {
		t.Fatal(err)
	}
}
func TestTCPOptionalAllocationFailureLeavesUDPCapacityIntact(t *testing.T) {
	authority := New(Config{Capacity: 1})
	defer authority.Close()
	lease, err := authority.Acquire([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = lease.PrepareTCP(false); err != ErrCapacity {
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
	if err = dual.PrepareTCP(false); err == nil {
		t.Fatal("second listener should fail")
	}
	if _, err = first.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("partial TCP allocation leaked: %v", err)
	}
	if len(dual.TCPEndpoints()) != 0 {
		t.Fatal("partial listener published")
	}
}
