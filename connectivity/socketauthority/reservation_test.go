package socketauthority

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
)

func socketRequest(t *testing.T, a *Authority, path byte) *Request {
	t.Helper()
	r, err := a.Request([16]byte{1}, 1, [16]byte{path}, []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOptionalTCPPoolIsIsolatedFromMandatoryUDP(t *testing.T) {
	allocations := 0
	a := New(Config{UDPCapacity: 2, TCPCapacity: 1, ListenPacket: func(network, address string) (net.PacketConn, error) {
		allocations++
		return net.ListenPacket(network, address)
	}})
	defer a.Close()
	first, err := socketRequest(t, a, 1).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	second, err := socketRequest(t, a, 2).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if allocations != 0 {
		t.Fatal("reservation opened sockets")
	}
	waiting := socketRequest(t, a, 3)
	changed := waiting.Changes()
	_, err = waiting.Reserve()
	capacity, ok := errors.AsType[*CapacityError](err)
	if !ok || capacity.Protocol != "udp" || capacity.Used != 0 || capacity.Reserved != 2 || capacity.Requested != 1 || capacity.Limit != 2 {
		t.Fatal("missing exact UDP capacity cause", err)
	}
	if capacity.Error() == "" || !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	lease, err := first.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	// Optional TCP has an independent ledger, so reserving all remaining UDP
	// slots does not block TCP preparation and TCP never reduces UDP headroom.
	if err := lease.PrepareTCP(); err != nil {
		t.Fatal("TCP blocked by unrelated UDP reservation", err)
	}
	second.Close()
	second.Close()
	select {
	case <-changed:
	default:
		t.Fatal("no release notification")
	}
	third, err := waiting.Reserve()
	if err != nil {
		t.Fatal("TCP listener starved mandatory UDP reservation", err)
	}
	thirdLease, err := third.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer thirdLease.Close()
	err = thirdLease.PrepareTCP()
	tcpCap, ok := errors.AsType[*CapacityError](err)
	if !ok || !errors.Is(err, ErrCapacity) || tcpCap.Protocol != "tcp" || tcpCap.Used != 1 || tcpCap.Requested != 1 || tcpCap.Limit != 1 {
		t.Fatalf("expected isolated TCP capacity error, got %v", err)
	}
	if _, err := first.Activate(); !errors.Is(err, ErrInvalid) {
		t.Fatal("reservation activated twice", err)
	}
	first.Close()
}

type testAddressedListener struct {
	net.Listener
	address *net.TCPAddr
}

func (l testAddressedListener) Addr() net.Addr { return l.address }

type testAddressedPacket struct {
	net.PacketConn
	address *net.UDPAddr
}

func (p testAddressedPacket) LocalAddr() net.Addr { return p.address }

func TestMultiHomedOptionalPathsAndEssentialReserveFitDefaultBudgets(t *testing.T) {
	for _, addressCount := range []int{11, MaxAddressesPerPath} {
		t.Run(fmt.Sprintf("%d-addresses", addressCount), func(t *testing.T) {
			a := New(Config{
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
					return testAddressedPacket{PacketConn: conn, address: net.UDPAddrFromAddrPort(netip.AddrPortFrom(requested.Addr().Unmap(), port))}, nil
				},
				ListenTCP: func(_, address string) (net.Listener, error) {
					requested, err := netip.ParseAddrPort(address)
					if err != nil {
						return nil, err
					}
					listener, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						return nil, err
					}
					port := listener.Addr().(*net.TCPAddr).AddrPort().Port()
					return testAddressedListener{Listener: listener, address: net.TCPAddrFromAddrPort(netip.AddrPortFrom(requested.Addr().Unmap(), port))}, nil
				},
			})
			defer a.Close()
			addresses := make([]netip.Addr, 0, addressCount)
			for i := 1; i <= addressCount; i++ {
				addresses = append(addresses, netip.AddrFrom4([4]byte{192, 0, 2, byte(i)}))
			}
			openPath := func(path byte, optional bool) *Lease {
				t.Helper()
				req, err := a.Request([16]byte{1}, 1, [16]byte{path}, addresses)
				if err != nil {
					t.Fatal(err)
				}
				var res *Reservation
				if optional {
					res, err = req.ReserveOptional()
				} else {
					res, err = req.Reserve()
				}
				if err != nil {
					t.Fatalf("path %d (optional=%v) reserve failed: %v", path, optional, err)
				}
				lease, err := res.Activate()
				if err != nil {
					t.Fatalf("path %d activate failed: %v", path, err)
				}
				t.Cleanup(func() { _ = lease.Close() })
				if err := lease.PrepareTCP(); err != nil {
					t.Fatalf("path %d PrepareTCP failed: %v", path, err)
				}
				if err := lease.PrepareTCP(); err != nil {
					t.Fatalf("path %d idempotent PrepareTCP failed: %v", path, err)
				}
				if len(lease.TCPEndpoints()) != addressCount {
					t.Fatalf("path %d tcp endpoints = %d, want %d", path, len(lease.TCPEndpoints()), addressCount)
				}
				return lease
			}
			// Three direct paths (1 essential + 2 optional throughput exploration)
			// plus a fourth incoming essential path must all fit default budgets.
			openPath(1, false)
			openPath(2, true)
			openPath(3, true)
			if addressCount == MaxAddressesPerPath {
				req, err := a.Request([16]byte{1}, 1, [16]byte{4}, addresses)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := req.ReserveOptional(); !errors.Is(err, ErrCapacity) {
					t.Fatalf("optional path consumed last essential slot: %v", err)
				}
			}
			openPath(4, false)
		})
	}
}

func TestOptionalTCPRespectsEssentialTCPReserve(t *testing.T) {
	a := New(Config{UDPCapacity: 3, TCPCapacity: 2})
	defer a.Close()
	firstRes, err := socketRequest(t, a, 1).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstRes.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.PrepareTCP(); err != nil {
		t.Fatal(err)
	}
	optRes, err := socketRequest(t, a, 2).ReserveOptional()
	if err != nil {
		t.Fatal(err)
	}
	opt, err := optRes.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer opt.Close()
	if err := opt.PrepareTCP(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("optional TCP consumed reserved essential TCP slot: %v", err)
	}
	essentialRes, err := socketRequest(t, a, 3).Reserve()
	if err != nil {
		t.Fatal(err)
	}
	essential, err := essentialRes.Activate()
	if err != nil {
		t.Fatal(err)
	}
	defer essential.Close()
	if err := essential.PrepareTCP(); err != nil {
		t.Fatalf("essential path could not claim reserved TCP slot: %v", err)
	}
}

func TestReservationRetainsExistingPathAndRejectsRetirement(t *testing.T) {
	for _, retirement := range []string{"none", "generation", "authority"} {
		t.Run(retirement, func(t *testing.T) {
			a := New(Config{UDPCapacity: 1})
			defer a.Close()
			r := socketRequest(t, a, 1)
			initial, _ := r.Reserve()
			lease, err := initial.Activate()
			if err != nil {
				t.Fatal(err)
			}
			retained, err := r.Reserve()
			if err != nil {
				t.Fatal("existing path charged twice", err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			switch retirement {
			case "generation":
				a.Retire(1)
			case "authority":
				_ = a.Close()
			}
			reused, err := retained.Activate()
			if retirement == "none" {
				if err != nil {
					t.Fatal(err)
				}
				_ = reused.Close()
			} else if err == nil {
				t.Fatal("retired allocation revived")
			}
			retained.Close()
		})
	}
}

func TestReservationAbandonmentAndFailedActivationReturnCapacity(t *testing.T) {
	a := New(Config{UDPCapacity: 1, ListenPacket: func(string, string) (net.PacketConn, error) {
		return nil, errors.New("bind failed")
	}})
	defer a.Close()
	r := socketRequest(t, a, 1)
	first, _ := r.Reserve()
	if _, err := r.Reserve(); !errors.Is(err, ErrCapacity) {
		t.Fatal("same path reserved twice", err)
	}
	first.Close()
	second, err := r.Reserve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Activate(); err == nil {
		t.Fatal("bind failure lost")
	}
	third, err := r.Reserve()
	if err != nil {
		t.Fatal("failed bind leaked capacity", err)
	}
	a.Retire(1)
	if _, err := third.Activate(); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	if _, err := r.Reserve(); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	_ = a.Close()
	if _, err := r.Reserve(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	var absent *Reservation
	absent.Close()
	if _, err := a.Request([16]byte{1}, 1, [16]byte{1}, []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("127.0.0.2")}); !errors.Is(err, ErrInvalid) {
		t.Fatal("impossible request should not queue", err)
	}
}
